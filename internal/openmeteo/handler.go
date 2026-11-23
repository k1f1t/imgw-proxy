package openmeteo

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"imgw-openmeteo-proxy/internal/geo"
	"imgw-openmeteo-proxy/internal/store"
)

// ForecastProvider resolves a forecast bundle for a coordinate pair (serving from SQLite
// or optionally fetching on-demand according to ForecastMode).
type ForecastProvider interface {
	GetForecastForCoords(ctx context.Context, lat, lon float64) (*store.CachedForecast, error)
}

// Handler serves Open-Meteo compatible GET /v1/forecast requests.
type Handler struct {
	store    *store.Store
	provider ForecastProvider
	nowFunc  func() time.Time
}

// NewHandler creates an Open-Meteo /v1/forecast HTTP handler.
func NewHandler(st *store.Store, provider ForecastProvider) *Handler {
	return &Handler{
		store:    st,
		provider: provider,
		nowFunc:  func() time.Time { return time.Now().UTC() },
	}
}

type errorResponse struct {
	Error  bool   `json:"error"`
	Reason string `json:"reason"`
}

type legacyCurrentWeather struct {
	Time          any      `json:"time"`
	Interval      int      `json:"interval"`
	Temperature   *float64 `json:"temperature"`
	WindSpeed     *float64 `json:"windspeed"`
	WindDirection *float64 `json:"winddirection"`
	IsDay         int      `json:"is_day"`
	WeatherCode   int      `json:"weathercode"`
}

type forecastResponse struct {
	Latitude             float64               `json:"latitude"`
	Longitude            float64               `json:"longitude"`
	GenerationTimeMS     float64               `json:"generationtime_ms"`
	UTCOffsetSeconds     int                   `json:"utc_offset_seconds"`
	Timezone             string                `json:"timezone"`
	TimezoneAbbreviation string                `json:"timezone_abbreviation"`
	Elevation            float64               `json:"elevation"`
	CurrentWeatherUnits  map[string]string     `json:"current_weather_units,omitempty"`
	CurrentWeather       *legacyCurrentWeather `json:"current_weather,omitempty"`
	CurrentUnits         map[string]string     `json:"current_units,omitempty"`
	Current              map[string]any        `json:"current,omitempty"`
	HourlyUnits          map[string]string     `json:"hourly_units,omitempty"`
	Hourly               map[string]any        `json:"hourly,omitempty"`
	DailyUnits           map[string]string     `json:"daily_units,omitempty"`
	Daily                map[string]any        `json:"daily,omitempty"`
}

// ServeHTTP handles GET /v1/forecast.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOMError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	lats, err := parseFloatList(q.Get("latitude"))
	if err != nil || len(lats) == 0 {
		writeOMError(w, http.StatusBadRequest, "missing or invalid latitude parameter")
		return
	}
	lons, err := parseFloatList(q.Get("longitude"))
	if err != nil || len(lons) == 0 {
		writeOMError(w, http.StatusBadRequest, "missing or invalid longitude parameter")
		return
	}
	if len(lats) != len(lons) {
		writeOMError(w, http.StatusBadRequest, "latitude and longitude must contain the same number of coordinates")
		return
	}
	for i := range lats {
		if lats[i] < -90 || lats[i] > 90 || lons[i] < -180 || lons[i] > 180 {
			writeOMError(w, http.StatusBadRequest, fmt.Sprintf("coordinate out of range at index %d", i))
			return
		}
	}

	units := DefaultUnitPrefs()
	if v := q.Get("temperature_unit"); v != "" {
		units.Temperature = strings.ToLower(v)
	}
	if v := q.Get("wind_speed_unit"); v != "" {
		units.WindSpeed = strings.ToLower(v)
	}
	if v := q.Get("precipitation_unit"); v != "" {
		units.Precipitation = strings.ToLower(v)
	}
	if v := q.Get("timeformat"); v != "" {
		units.TimeFormat = strings.ToLower(v)
	}
	if err := units.Validate(); err != nil {
		writeOMError(w, http.StatusBadRequest, err.Error())
		return
	}

	loc, tzName, err := resolveTimezone(q.Get("timezone"))
	if err != nil {
		writeOMError(w, http.StatusBadRequest, err.Error())
		return
	}

	pastDays := 0
	if v := q.Get("past_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 92 {
			writeOMError(w, http.StatusBadRequest, "past_days must be between 0 and 92")
			return
		}
		pastDays = n
	}

	forecastDays := 7
	if v := q.Get("forecast_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 16 {
			writeOMError(w, http.StatusBadRequest, "forecast_days must be between 0 and 16")
			return
		}
		forecastDays = n
	}

	var startDate, endDate *time.Time
	if v := strings.TrimSpace(q.Get("start_date")); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			writeOMError(w, http.StatusBadRequest, "invalid start_date (expected YYYY-MM-DD)")
			return
		}
		startDate = &t
	}
	if v := strings.TrimSpace(q.Get("end_date")); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			writeOMError(w, http.StatusBadRequest, "invalid end_date (expected YYYY-MM-DD)")
			return
		}
		endDate = &t
	}
	if startDate != nil && endDate != nil && endDate.Before(*startDate) {
		writeOMError(w, http.StatusBadRequest, "end_date must be >= start_date")
		return
	}

	currentVars := parseStringList(q["current"])
	hourlyVars := parseStringList(q["hourly"])
	dailyVars := parseStringList(q["daily"])
	wantLegacyCurrent := strings.EqualFold(q.Get("current_weather"), "true") || q.Get("current_weather") == "1"

	synopStations, err := h.store.ListStations(r.Context(), "synop")
	if err != nil {
		writeOMError(w, http.StatusInternalServerError, "failed to load cached synoptic stations")
		return
	}
	meteoStations, err := h.store.ListStations(r.Context(), "meteo")
	if err != nil {
		writeOMError(w, http.StatusInternalServerError, "failed to load cached meteo stations")
		return
	}

	now := h.nowFunc().UTC()
	responses := make([]forecastResponse, 0, len(lats))

	for i := range lats {
		resp, err := h.buildSingle(
			r.Context(),
			lats[i], lons[i],
			now, loc, tzName,
			units, pastDays, forecastDays, startDate, endDate,
			currentVars, hourlyVars, dailyVars, wantLegacyCurrent,
			synopStations, meteoStations,
		)
		if err != nil {
			writeOMError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		resp.GenerationTimeMS = round3(float64(time.Since(started).Microseconds()) / 1000.0)
		responses = append(responses, resp)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if len(responses) == 1 {
		_ = json.NewEncoder(w).Encode(responses[0])
		return
	}
	_ = json.NewEncoder(w).Encode(responses)
}

func (h *Handler) buildSingle(
	ctx context.Context,
	reqLat, reqLon float64,
	now time.Time,
	loc *time.Location,
	tzName string,
	units UnitPrefs,
	pastDays, forecastDays int,
	startDate, endDate *time.Time,
	currentVars, hourlyVars, dailyVars []string,
	wantLegacyCurrent bool,
	synopStations, meteoStations []store.Station,
) (forecastResponse, error) {
	nearestSynop, synopDist := findNearestStation(reqLat, reqLon, synopStations)
	nearestMeteo, meteoDist := findNearestStation(reqLat, reqLon, meteoStations)

	var fc *store.CachedForecast
	if h.provider != nil && (len(hourlyVars) > 0 || len(dailyVars) > 0 || len(currentVars) > 0 || wantLegacyCurrent) {
		fc, _ = h.provider.GetForecastForCoords(ctx, reqLat, reqLon)
	}

	if nearestSynop == nil && nearestMeteo == nil && fc == nil {
		return forecastResponse{}, fmt.Errorf("no cached IMGW data available yet; initial sync in progress")
	}

	respLat, respLon, elev := reqLat, reqLon, 0.0
	if fc != nil {
		respLat, respLon = fc.Lat, fc.Lon
		if fc.Elevation != nil {
			elev = *fc.Elevation
		}
	} else if nearestMeteo != nil && (nearestSynop == nil || meteoDist <= synopDist) {
		respLat, respLon = nearestMeteo.Lat, nearestMeteo.Lon
		if nearestMeteo.Elevation != nil {
			elev = *nearestMeteo.Elevation
		}
	} else if nearestSynop != nil {
		respLat, respLon = nearestSynop.Lat, nearestSynop.Lon
		if nearestSynop.Elevation != nil {
			elev = *nearestSynop.Elevation
		}
	}
	if elev == 0 && nearestSynop != nil && nearestSynop.Elevation != nil {
		elev = *nearestSynop.Elevation
	}

	tzAbbr, utcOffset := now.In(loc).Zone()
	resp := forecastResponse{
		Latitude:             round4(respLat),
		Longitude:            round4(respLon),
		UTCOffsetSeconds:     utcOffset,
		Timezone:             tzName,
		TimezoneAbbreviation: tzAbbr,
		Elevation:            round1(elev),
	}

	// Build unified current point (merging meteo 10-min + synop pressure/fallback + forecast cloud/icon).
	curPoint := h.buildCurrentPoint(ctx, now, nearestSynop, nearestMeteo, fc, respLat, respLon)

	if wantLegacyCurrent {
		resp.CurrentWeatherUnits = map[string]string{
			"time":          timeUnitLabel(units.TimeFormat),
			"interval":      "seconds",
			"temperature":   units.TempUnitLabel(),
			"windspeed":     units.WindUnitLabel(),
			"winddirection": "°",
			"is_day":        "",
			"weathercode":   "wmo code",
		}
		isDay := 0
		if geo.IsDay(curPoint.Time, respLat, respLon) {
			isDay = 1
		}
		resp.CurrentWeather = &legacyCurrentWeather{
			Time:          formatTimestamp(curPoint.Time, loc, units.TimeFormat, true),
			Interval:      900,
			Temperature:   units.ConvertTemp(curPoint.TemperatureC),
			WindSpeed:     units.ConvertWind(curPoint.WindSpeedMS),
			WindDirection: roundPtr1(curPoint.WindDirDeg),
			IsDay:         isDay,
			WeatherCode:   resolvePointWeatherCode(curPoint),
		}
	}

	if len(currentVars) > 0 {
		cu, cv := buildCurrentObject(curPoint, currentVars, respLat, respLon, loc, units)
		resp.CurrentUnits = cu
		resp.Current = cv
	}

	// Determine requested time window in target timezone.
	nowLocal := now.In(loc)
	todayLocal := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)
	windowStart := todayLocal.AddDate(0, 0, -pastDays)
	windowEnd := todayLocal.AddDate(0, 0, forecastDays).Add(-time.Hour)
	if startDate != nil {
		windowStart = *startDate
	}
	if endDate != nil {
		windowEnd = endDate.Add(23 * time.Hour)
	}
	if windowEnd.Before(windowStart) {
		windowEnd = windowStart.Add(23 * time.Hour)
	}

	if len(hourlyVars) > 0 || len(dailyVars) > 0 {
		timeline := h.buildHourlyTimeline(ctx, windowStart.UTC(), windowEnd.UTC(), nearestSynop, nearestMeteo, fc, respLat, respLon)

		if len(hourlyVars) > 0 {
			hu, hv := buildHourlyObject(timeline, hourlyVars, respLat, respLon, loc, units)
			resp.HourlyUnits = hu
			resp.Hourly = hv
		}
		if len(dailyVars) > 0 {
			du, dv := buildDailyObject(timeline, windowStart, windowEnd, dailyVars, respLat, respLon, loc, units)
			resp.DailyUnits = du
			resp.Daily = dv
		}
	}

	return resp, nil
}

func (h *Handler) buildCurrentPoint(
	ctx context.Context,
	now time.Time,
	nearestSynop, nearestMeteo *store.Station,
	fc *store.CachedForecast,
	lat, lon float64,
) store.ForecastPoint {
	pt := store.ForecastPoint{Time: now.Truncate(time.Minute)}

	var synopObs *store.MeteoObservation
	if nearestSynop != nil {
		synopObs, _ = h.store.LatestMeteoObservation(ctx, "synop", nearestSynop.ID)
	}
	var meteoObs *store.MeteoObservation
	if nearestMeteo != nil {
		meteoObs, _ = h.store.LatestMeteoObservation(ctx, "meteo", nearestMeteo.ID)
	}

	// Prefer 10-min automatic meteo station if fresh (<= 3h), then enrich from synop.
	if meteoObs != nil && now.Sub(meteoObs.ObservedAt) <= 3*time.Hour {
		pt.Time = meteoObs.ObservedAt
		pt.TemperatureC = meteoObs.TemperatureC
		pt.HumidityPct = meteoObs.HumidityPct
		pt.WindSpeedMS = meteoObs.WindSpeedMS
		pt.WindGustMS = meteoObs.WindGustMS
		pt.WindDirDeg = meteoObs.WindDirDeg
		// Sum last-hour 10-min precipitation readings from this meteo station so current.precipitation
		// matches Open-Meteo's hourly precipitation rate rather than a single 10-min bucket.
		recent, err := h.store.ListMeteoObservations(ctx, "meteo", nearestMeteo.ID, meteoObs.ObservedAt.Add(-50*time.Minute), meteoObs.ObservedAt)
		if err == nil && len(recent) > 0 {
			var sum float64
			hasPrecip := false
			for _, r := range recent {
				if r.PrecipitationMM != nil {
					sum += *r.PrecipitationMM
					hasPrecip = true
				}
			}
			if hasPrecip {
				pt.PrecipitationMM = &sum
			}
		} else {
			pt.PrecipitationMM = meteoObs.PrecipitationMM
		}
	}

	var synopPrecipFallback *float64
	if synopObs != nil && now.Sub(synopObs.ObservedAt) <= 6*time.Hour {
		if pt.TemperatureC == nil {
			pt.TemperatureC = synopObs.TemperatureC
			pt.Time = synopObs.ObservedAt
		}
		if pt.HumidityPct == nil {
			pt.HumidityPct = synopObs.HumidityPct
		}
		if pt.WindSpeedMS == nil {
			pt.WindSpeedMS = synopObs.WindSpeedMS
		}
		if pt.WindDirDeg == nil {
			pt.WindDirDeg = synopObs.WindDirDeg
		}
		if pt.PressureHPa == nil {
			pt.PressureHPa = synopObs.PressureHPa
		}
		// Keep synop suma_opadu only as a last-resort fallback since IMGW synop reports
		// cumulative precipitation since 06:00 UTC rather than a 1-hour rate.
		synopPrecipFallback = synopObs.PrecipitationMM
	}

	// Enrich missing fields (cloud cover, weather_code, rain/snow split, apparent temp) from closest forecast hour.
	if fc != nil && len(fc.Hourly) > 0 {
		closest := findClosestForecastPoint(fc.Hourly, now)
		if closest != nil {
			if pt.TemperatureC == nil {
				pt.TemperatureC = closest.TemperatureC
				pt.Time = closest.Time
			}
			if pt.HumidityPct == nil {
				pt.HumidityPct = closest.HumidityPct
			}
			if pt.WindSpeedMS == nil {
				pt.WindSpeedMS = closest.WindSpeedMS
			}
			if pt.WindGustMS == nil {
				pt.WindGustMS = closest.WindGustMS
			}
			if pt.WindDirDeg == nil {
				pt.WindDirDeg = closest.WindDirDeg
			}
			if pt.PrecipitationMM == nil {
				pt.PrecipitationMM = closest.PrecipitationMM
			}
			if pt.PressureHPa == nil {
				pt.PressureHPa = closest.PressureHPa
			}
			pt.CloudCoverPct = closest.CloudCoverPct
			pt.RainMM = closest.RainMM
			pt.SnowfallCM = closest.SnowfallCM
			pt.PrecipProbPct = closest.PrecipProbPct
			pt.IconCode = closest.IconCode
			pt.WeatherCode = closest.WeatherCode
			if pt.ApparentTempC == nil {
				pt.ApparentTempC = closest.ApparentTempC
			}
		}
	}

	if pt.PrecipitationMM == nil {
		pt.PrecipitationMM = synopPrecipFallback
	}

	enrichDerivedPoint(&pt)
	_ = lat
	_ = lon
	return pt
}

func (h *Handler) buildHourlyTimeline(
	ctx context.Context,
	fromUTC, toUTC time.Time,
	nearestSynop, nearestMeteo *store.Station,
	fc *store.CachedForecast,
	lat, lon float64,
) []store.ForecastPoint {
	byHour := make(map[int64]store.ForecastPoint)

	// 1. Seed with forecast points first.
	if fc != nil {
		for _, p := range fc.Hourly {
			ts := p.Time.UTC().Truncate(time.Hour)
			if ts.Before(fromUTC) || ts.After(toUTC) {
				continue
			}
			p.Time = ts
			byHour[ts.Unix()] = p
		}
	}

	// 2. Overlay observed synop history for past hours (authoritative measurements).
	// Note: IMGW /synop suma_opadu is cumulative since 06:00 UTC, so derive hourly delta
	// between consecutive hours when available instead of overwriting hourly slots with a multi-hour sum.
	if nearestSynop != nil {
		synopHist, err := h.store.ListMeteoObservations(ctx, "synop", nearestSynop.ID, fromUTC.Add(-time.Hour), toUTC)
		if err == nil {
			var prev *store.MeteoObservation
			for i := range synopHist {
				o := synopHist[i]
				ts := o.ObservedAt.UTC().Truncate(time.Hour)
				if !ts.Before(fromUTC) && !ts.After(toUTC) {
					pt := byHour[ts.Unix()]
					pt.Time = ts
					if o.TemperatureC != nil {
						pt.TemperatureC = o.TemperatureC
					}
					if o.HumidityPct != nil {
						pt.HumidityPct = o.HumidityPct
					}
					if o.WindSpeedMS != nil {
						pt.WindSpeedMS = o.WindSpeedMS
					}
					if o.WindDirDeg != nil {
						pt.WindDirDeg = o.WindDirDeg
					}
					if o.PressureHPa != nil {
						pt.PressureHPa = o.PressureHPa
					}
					if o.PrecipitationMM != nil {
						if prev != nil && prev.PrecipitationMM != nil &&
							ts.Sub(prev.ObservedAt.UTC().Truncate(time.Hour)) == time.Hour &&
							ts.Hour() != 6 && *o.PrecipitationMM >= *prev.PrecipitationMM {
							delta := *o.PrecipitationMM - *prev.PrecipitationMM
							pt.PrecipitationMM = &delta
						} else if pt.PrecipitationMM == nil && ts.Hour() == 6 {
							// 06:00 UTC starts a fresh accumulation window.
							v := *o.PrecipitationMM
							pt.PrecipitationMM = &v
						}
					}
					byHour[ts.Unix()] = pt
				}
				cur := o
				prev = &cur
			}
		}
	}

	// 3. Overlay meteo automatic station 10-minute observations:
	// sum opad_10min across all intervals in the hour for accurate hourly precipitation,
	// take max wind gust, and fill missing temp/humidity/wind fields.
	if nearestMeteo != nil {
		meteoHist, err := h.store.ListMeteoObservations(ctx, "meteo", nearestMeteo.ID, fromUTC, toUTC.Add(59*time.Minute))
		if err == nil {
			precipSum := make(map[int64]float64)
			hasPrecip := make(map[int64]bool)
			for _, o := range meteoHist {
				ts := o.ObservedAt.UTC().Truncate(time.Hour)
				if ts.Before(fromUTC) || ts.After(toUTC) {
					continue
				}
				hk := ts.Unix()
				pt := byHour[hk]
				pt.Time = ts
				if pt.TemperatureC == nil && o.TemperatureC != nil {
					pt.TemperatureC = o.TemperatureC
				}
				if pt.HumidityPct == nil && o.HumidityPct != nil {
					pt.HumidityPct = o.HumidityPct
				}
				if pt.WindSpeedMS == nil && o.WindSpeedMS != nil {
					pt.WindSpeedMS = o.WindSpeedMS
				}
				if o.WindGustMS != nil {
					if pt.WindGustMS == nil || *o.WindGustMS > *pt.WindGustMS {
						v := *o.WindGustMS
						pt.WindGustMS = &v
					}
				}
				if pt.WindDirDeg == nil && o.WindDirDeg != nil {
					pt.WindDirDeg = o.WindDirDeg
				}
				if o.PrecipitationMM != nil {
					precipSum[hk] += *o.PrecipitationMM
					hasPrecip[hk] = true
				}
				byHour[hk] = pt
			}
			for hk, sum := range precipSum {
				if hasPrecip[hk] {
					pt := byHour[hk]
					v := sum
					pt.PrecipitationMM = &v
					byHour[hk] = pt
				}
			}
		}
	}

	// Emit full contiguous requested [fromUTC, toUTC] hourly grid (matching Open-Meteo contract;
	// hours without observations or forecasts are emitted with null variable values).
	start := fromUTC.UTC().Truncate(time.Hour)
	end := toUTC.UTC().Truncate(time.Hour)
	if end.Before(start) {
		return nil
	}

	out := make([]store.ForecastPoint, 0, int(end.Sub(start)/time.Hour)+1)
	for cur := start; !cur.After(end); cur = cur.Add(time.Hour) {
		pt, ok := byHour[cur.Unix()]
		if !ok {
			pt = store.ForecastPoint{Time: cur}
		}
		enrichDerivedPoint(&pt)
		out = append(out, pt)
	}
	_ = lat
	_ = lon
	return out
}

func enrichDerivedPoint(pt *store.ForecastPoint) {
	if pt.ApparentTempC == nil && pt.TemperatureC != nil {
		rh := 65.0
		if pt.HumidityPct != nil {
			rh = *pt.HumidityPct
		}
		ws := 0.0
		if pt.WindSpeedMS != nil {
			ws = *pt.WindSpeedMS
		}
		at := geo.ApparentTemperatureC(*pt.TemperatureC, rh, ws)
		pt.ApparentTempC = &at
	}
	if pt.RainMM == nil && pt.SnowfallCM == nil && pt.PrecipitationMM != nil {
		p := *pt.PrecipitationMM
		if pt.TemperatureC != nil && *pt.TemperatureC <= 0.5 {
			zero := 0.0
			snow := p * 0.7 // ~7mm snow depth per 1mm liquid water equivalent
			pt.RainMM = &zero
			pt.SnowfallCM = &snow
		} else {
			rain := p
			zero := 0.0
			pt.RainMM = &rain
			pt.SnowfallCM = &zero
		}
	}
	if pt.WeatherCode == nil {
		if code, ok := WeatherCodeFromIMGWIcon(pt.IconCode); ok {
			pt.WeatherCode = &code
		} else if pt.TemperatureC != nil || pt.PrecipitationMM != nil || pt.CloudCoverPct != nil {
			code := SynthesizeWeatherCode(pt.TemperatureC, pt.PrecipitationMM, pt.RainMM, pt.SnowfallCM, pt.CloudCoverPct, pt.HumidityPct)
			pt.WeatherCode = &code
		}
	}
}

func resolvePointWeatherCode(pt store.ForecastPoint) int {
	if pt.WeatherCode != nil {
		return *pt.WeatherCode
	}
	return SynthesizeWeatherCode(pt.TemperatureC, pt.PrecipitationMM, pt.RainMM, pt.SnowfallCM, pt.CloudCoverPct, pt.HumidityPct)
}

func buildCurrentObject(
	pt store.ForecastPoint,
	vars []string,
	lat, lon float64,
	loc *time.Location,
	units UnitPrefs,
) (map[string]string, map[string]any) {
	cu := map[string]string{
		"time":     timeUnitLabel(units.TimeFormat),
		"interval": "seconds",
	}
	cv := map[string]any{
		"time":     formatTimestamp(pt.Time, loc, units.TimeFormat, true),
		"interval": 900,
	}

	for _, name := range vars {
		cu[name] = unitLabelForVar(name, units)
		cv[name] = extractPointValue(pt, name, lat, lon, units)
	}
	return cu, cv
}

func buildHourlyObject(
	timeline []store.ForecastPoint,
	vars []string,
	lat, lon float64,
	loc *time.Location,
	units UnitPrefs,
) (map[string]string, map[string]any) {
	hu := map[string]string{
		"time": timeUnitLabel(units.TimeFormat),
	}
	times := make([]any, 0, len(timeline))
	for _, pt := range timeline {
		times = append(times, formatTimestamp(pt.Time, loc, units.TimeFormat, true))
	}
	hv := map[string]any{
		"time": times,
	}

	for _, name := range vars {
		hu[name] = unitLabelForVar(name, units)
		vals := make([]any, 0, len(timeline))
		for _, pt := range timeline {
			vals = append(vals, extractPointValue(pt, name, lat, lon, units))
		}
		hv[name] = vals
	}
	return hu, hv
}

func buildDailyObject(
	timeline []store.ForecastPoint,
	windowStart, windowEnd time.Time,
	vars []string,
	lat, lon float64,
	loc *time.Location,
	units UnitPrefs,
) (map[string]string, map[string]any) {
	type dayBucket struct {
		date   time.Time
		points []store.ForecastPoint
	}

	buckets := make(map[string]*dayBucket)
	var dayKeys []string

	// Seed buckets for every requested calendar day in [windowStart, windowEnd] (matching Open-Meteo).
	startDay := time.Date(windowStart.In(loc).Year(), windowStart.In(loc).Month(), windowStart.In(loc).Day(), 0, 0, 0, 0, loc)
	endDay := time.Date(windowEnd.In(loc).Year(), windowEnd.In(loc).Month(), windowEnd.In(loc).Day(), 0, 0, 0, 0, loc)
	for d := startDay; !d.After(endDay); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		buckets[key] = &dayBucket{date: d}
		dayKeys = append(dayKeys, key)
	}

	for _, pt := range timeline {
		localT := pt.Time.In(loc)
		key := localT.Format("2006-01-02")
		b, ok := buckets[key]
		if !ok {
			d := time.Date(localT.Year(), localT.Month(), localT.Day(), 0, 0, 0, 0, loc)
			b = &dayBucket{date: d}
			buckets[key] = b
			dayKeys = append(dayKeys, key)
		}
		b.points = append(b.points, pt)
	}
	sort.Strings(dayKeys)

	du := map[string]string{
		"time": timeUnitLabel(units.TimeFormat),
	}
	dateVals := make([]any, 0, len(dayKeys))
	for _, k := range dayKeys {
		b := buckets[k]
		if units.TimeFormat == "unixtime" {
			dateVals = append(dateVals, b.date.Unix())
		} else {
			dateVals = append(dateVals, k)
		}
	}
	dv := map[string]any{
		"time": dateVals,
	}

	for _, name := range vars {
		du[name] = unitLabelForDailyVar(name, units)
		vals := make([]any, 0, len(dayKeys))
		for _, k := range dayKeys {
			b := buckets[k]
			vals = append(vals, aggregateDailyVar(b.date, b.points, name, lat, lon, loc, units))
		}
		dv[name] = vals
	}
	return du, dv
}

func extractPointValue(pt store.ForecastPoint, name string, lat, lon float64, units UnitPrefs) any {
	switch name {
	case "temperature_2m":
		return units.ConvertTemp(pt.TemperatureC)
	case "apparent_temperature":
		return units.ConvertTemp(pt.ApparentTempC)
	case "relative_humidity_2m":
		return roundPtrInt(pt.HumidityPct)
	case "dew_point_2m":
		if pt.TemperatureC != nil && pt.HumidityPct != nil {
			dp := dewPointC(*pt.TemperatureC, *pt.HumidityPct)
			return units.ConvertTemp(&dp)
		}
		return nil
	case "precipitation":
		return units.ConvertPrecipMM(pt.PrecipitationMM)
	case "rain", "showers":
		return units.ConvertPrecipMM(pt.RainMM)
	case "snowfall":
		return units.ConvertSnowCM(pt.SnowfallCM)
	case "precipitation_probability":
		return roundPtrInt(pt.PrecipProbPct)
	case "pressure_msl", "surface_pressure":
		return roundPtr1(pt.PressureHPa)
	case "cloud_cover", "cloudcover":
		return roundPtrInt(pt.CloudCoverPct)
	case "wind_speed_10m", "windspeed_10m":
		return units.ConvertWind(pt.WindSpeedMS)
	case "wind_gusts_10m", "windgusts_10m":
		return units.ConvertWind(pt.WindGustMS)
	case "wind_direction_10m", "winddirection_10m":
		return roundPtrInt(pt.WindDirDeg)
	case "weather_code", "weathercode":
		if pt.WeatherCode != nil {
			return *pt.WeatherCode
		}
		return nil
	case "is_day":
		if geo.IsDay(pt.Time, lat, lon) {
			return 1
		}
		return 0
	default:
		return nil
	}
}

func aggregateDailyVar(
	dayLocal time.Time,
	pts []store.ForecastPoint,
	name string,
	lat, lon float64,
	loc *time.Location,
	units UnitPrefs,
) any {
	switch name {
	case "temperature_2m_max":
		return units.ConvertTemp(maxFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.TemperatureC }))
	case "temperature_2m_min":
		return units.ConvertTemp(minFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.TemperatureC }))
	case "apparent_temperature_max":
		return units.ConvertTemp(maxFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.ApparentTempC }))
	case "apparent_temperature_min":
		return units.ConvertTemp(minFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.ApparentTempC }))
	case "precipitation_sum":
		return units.ConvertPrecipMM(sumFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.PrecipitationMM }))
	case "rain_sum":
		return units.ConvertPrecipMM(sumFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.RainMM }))
	case "snowfall_sum":
		return units.ConvertSnowCM(sumFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.SnowfallCM }))
	case "precipitation_hours":
		hours := 0.0
		for _, p := range pts {
			if p.PrecipitationMM != nil && *p.PrecipitationMM > 0 {
				hours++
			}
		}
		return hours
	case "precipitation_probability_max":
		return roundPtrInt(maxFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.PrecipProbPct }))
	case "wind_speed_10m_max", "windspeed_10m_max":
		return units.ConvertWind(maxFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.WindSpeedMS }))
	case "wind_gusts_10m_max", "windgusts_10m_max":
		return units.ConvertWind(maxFloatPtr(pts, func(p store.ForecastPoint) *float64 { return p.WindGustMS }))
	case "wind_direction_10m_dominant", "winddirection_10m_dominant":
		return dominantWindDir(pts)
	case "weather_code", "weathercode":
		best := -1
		for _, p := range pts {
			if p.WeatherCode != nil && *p.WeatherCode > best {
				best = *p.WeatherCode
			}
		}
		if best < 0 {
			return nil
		}
		return best
	case "sunrise":
		sr, _, ok := geo.SunriseSunset(dayLocal.Year(), dayLocal.Month(), dayLocal.Day(), lat, lon)
		if !ok {
			return nil
		}
		return formatTimestamp(sr, loc, units.TimeFormat, true)
	case "sunset":
		_, ss, ok := geo.SunriseSunset(dayLocal.Year(), dayLocal.Month(), dayLocal.Day(), lat, lon)
		if !ok {
			return nil
		}
		return formatTimestamp(ss, loc, units.TimeFormat, true)
	case "daylight_duration":
		sr, ss, ok := geo.SunriseSunset(dayLocal.Year(), dayLocal.Month(), dayLocal.Day(), lat, lon)
		if !ok {
			return nil
		}
		return round1(ss.Sub(sr).Seconds())
	default:
		return nil
	}
}

func unitLabelForVar(name string, units UnitPrefs) string {
	switch name {
	case "temperature_2m", "apparent_temperature", "dew_point_2m":
		return units.TempUnitLabel()
	case "relative_humidity_2m", "cloud_cover", "cloudcover", "precipitation_probability":
		return "%"
	case "precipitation", "rain", "showers":
		return units.PrecipUnitLabel()
	case "snowfall":
		return units.SnowUnitLabel()
	case "pressure_msl", "surface_pressure":
		return "hPa"
	case "wind_speed_10m", "windspeed_10m", "wind_gusts_10m", "windgusts_10m":
		return units.WindUnitLabel()
	case "wind_direction_10m", "winddirection_10m":
		return "°"
	case "weather_code", "weathercode":
		return "wmo code"
	case "is_day":
		return ""
	default:
		return ""
	}
}

func unitLabelForDailyVar(name string, units UnitPrefs) string {
	switch name {
	case "temperature_2m_max", "temperature_2m_min", "apparent_temperature_max", "apparent_temperature_min":
		return units.TempUnitLabel()
	case "precipitation_sum", "rain_sum":
		return units.PrecipUnitLabel()
	case "snowfall_sum":
		return units.SnowUnitLabel()
	case "precipitation_hours":
		return "h"
	case "precipitation_probability_max":
		return "%"
	case "wind_speed_10m_max", "windspeed_10m_max", "wind_gusts_10m_max", "windgusts_10m_max":
		return units.WindUnitLabel()
	case "wind_direction_10m_dominant", "winddirection_10m_dominant":
		return "°"
	case "weather_code", "weathercode":
		return "wmo code"
	case "sunrise", "sunset":
		return timeUnitLabel(units.TimeFormat)
	case "daylight_duration":
		return "s"
	default:
		return ""
	}
}

func timeUnitLabel(tf string) string {
	if tf == "unixtime" {
		return "unixtime"
	}
	return "iso8601"
}

func formatTimestamp(t time.Time, loc *time.Location, tf string, withHour bool) any {
	if tf == "unixtime" {
		return t.UTC().Unix()
	}
	lt := t.In(loc)
	if withHour {
		return lt.Format("2006-01-02T15:04")
	}
	return lt.Format("2006-01-02")
}

func resolveTimezone(tzParam string) (*time.Location, string, error) {
	tz := strings.TrimSpace(tzParam)
	if tz == "" || strings.EqualFold(tz, "GMT") || strings.EqualFold(tz, "UTC") {
		return time.UTC, "GMT", nil
	}
	if strings.EqualFold(tz, "auto") {
		tz = "Europe/Warsaw"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, "", fmt.Errorf("invalid timezone %q", tzParam)
	}
	return loc, tz, nil
}

func findNearestStation(lat, lon float64, stations []store.Station) (*store.Station, float64) {
	if len(stations) == 0 {
		return nil, math.MaxFloat64
	}
	bestIdx := 0
	bestDist := geo.HaversineKm(lat, lon, stations[0].Lat, stations[0].Lon)
	for i := 1; i < len(stations); i++ {
		d := geo.HaversineKm(lat, lon, stations[i].Lat, stations[i].Lon)
		if d < bestDist {
			bestDist = d
			bestIdx = i
		}
	}
	st := stations[bestIdx]
	return &st, bestDist
}

func findClosestForecastPoint(pts []store.ForecastPoint, target time.Time) *store.ForecastPoint {
	if len(pts) == 0 {
		return nil
	}
	bestIdx := 0
	bestDiff := absDuration(pts[0].Time.Sub(target))
	for i := 1; i < len(pts); i++ {
		d := absDuration(pts[i].Time.Sub(target))
		if d < bestDiff {
			bestDiff = d
			bestIdx = i
		}
	}
	if bestDiff > 6*time.Hour {
		return nil
	}
	pt := pts[bestIdx]
	return &pt
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func parseFloatList(raw string) ([]float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("invalid float %q", p)
		}
		out = append(out, f)
	}
	return out, nil
}

func parseStringList(values []string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			s := strings.TrimSpace(part)
			if s == "" {
				continue
			}
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func maxFloatPtr(pts []store.ForecastPoint, fn func(store.ForecastPoint) *float64) *float64 {
	var best *float64
	for _, p := range pts {
		v := fn(p)
		if v == nil {
			continue
		}
		if best == nil || *v > *best {
			x := *v
			best = &x
		}
	}
	return best
}

func minFloatPtr(pts []store.ForecastPoint, fn func(store.ForecastPoint) *float64) *float64 {
	var best *float64
	for _, p := range pts {
		v := fn(p)
		if v == nil {
			continue
		}
		if best == nil || *v < *best {
			x := *v
			best = &x
		}
	}
	return best
}

func sumFloatPtr(pts []store.ForecastPoint, fn func(store.ForecastPoint) *float64) *float64 {
	var total float64
	found := false
	for _, p := range pts {
		v := fn(p)
		if v == nil {
			continue
		}
		total += *v
		found = true
	}
	if !found {
		return nil
	}
	return &total
}

func dominantWindDir(pts []store.ForecastPoint) any {
	var sumSin, sumCos float64
	count := 0
	for _, p := range pts {
		if p.WindDirDeg == nil {
			continue
		}
		weight := 1.0
		if p.WindSpeedMS != nil && *p.WindSpeedMS > 0 {
			weight = *p.WindSpeedMS
		}
		rad := *p.WindDirDeg * math.Pi / 180.0
		sumSin += math.Sin(rad) * weight
		sumCos += math.Cos(rad) * weight
		count++
	}
	if count == 0 {
		return nil
	}
	deg := math.Atan2(sumSin, sumCos) * 180.0 / math.Pi
	if deg < 0 {
		deg += 360.0
	}
	return int(math.Round(deg)) % 360
}

func dewPointC(tempC, rh float64) float64 {
	if rh <= 0 {
		rh = 1
	}
	const a, b = 17.27, 237.7
	gamma := (a*tempC)/(b+tempC) + math.Log(rh/100.0)
	return (b * gamma) / (a - gamma)
}

func roundPtr1(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := round1(*v)
	return &r
}

func roundPtrInt(v *float64) any {
	if v == nil {
		return nil
	}
	return int(math.Round(*v))
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

func writeOMError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: true, Reason: reason})
}
