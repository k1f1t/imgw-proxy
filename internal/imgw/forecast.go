package imgw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"imgw-openmeteo-proxy/internal/store"
)

type rawForecastEnvelope struct {
	Data *rawForecastPayload `json:"data"`
}

type rawForecastPayload struct {
	Model    string `json:"Model"`
	Valid    bool   `json:"Valid"`
	Location struct {
		Lat float64 `json:"Lat"`
		Lon float64 `json:"Lon"`
	} `json:"Location"`
	Data []rawForecastItem `json:"Data"`
}

type rawForecastItem struct {
	Type             string  `json:"Type"`
	Date             string  `json:"Date"`
	Temperature      *string `json:"Temperature"`
	Chill            *string `json:"Chill"`
	PressureMSL      *string `json:"PressureMSL"`
	Pressure         *string `json:"Pressure"`
	Humidity         *string `json:"Humidity"`
	Cloud            *string `json:"Cloud"`
	WindSpeed        *string `json:"Wind_Speed"`
	WindDir          *string `json:"Wind_Dir"`
	WindGust         *string `json:"Wind_Gust"`
	Precipitation    *string `json:"Precipitation"`
	Rain             *string `json:"Rain"`
	Snow             *string `json:"Snow"`
	Precipitation10m *string `json:"Precipitation10m"`
	Rain10m          *string `json:"Rain10m"`
	Snow10m          *string `json:"Snow10m"`
	Icon             string  `json:"Icon"`
	Icon10           string  `json:"Icon10"`
}

// FetchForecast fetches and blends short-term (fcapi hybrid/alaro) and medium-term (forecastapi GFS)
// hourly forecasts from meteo.imgw.pl for (lat, lon).
func (c *Client) FetchForecast(ctx context.Context, lat, lon float64) (*store.CachedForecast, error) {
	shortModel := c.forecastModel
	if shortModel == "" {
		shortModel = "hybrid"
	}

	shortPayload, shortErr := c.fetchMeteoEndpoint(ctx, "/api/v1/forecast/fcapi", lat, lon, shortModel)
	if (shortErr != nil || shortPayload == nil || len(shortPayload.Data) == 0) && shortModel == "hybrid" {
		if fb, fbErr := c.fetchMeteoEndpoint(ctx, "/api/v1/forecast/fcapi", lat, lon, "alaro"); fbErr == nil && fb != nil && len(fb.Data) > 0 {
			shortPayload = fb
			shortErr = nil
		}
	}

	longPayload, longErr := c.fetchMeteoEndpoint(ctx, "/api/v1/forecast/forecastapi", lat, lon, "")

	if (shortPayload == nil || len(shortPayload.Data) == 0) && (longPayload == nil || len(longPayload.Data) == 0) {
		if shortErr != nil {
			return nil, fmt.Errorf("fetch forecast failed (fcapi: %w; forecastapi: %v)", shortErr, longErr)
		}
		return nil, fmt.Errorf("fetch forecast returned empty data (forecastapi: %v)", longErr)
	}

	byHour := make(map[int64]store.ForecastPoint)
	models := make([]string, 0, 2)
	respLat, respLon := lat, lon

	// Seed with medium-term GFS first so high-resolution short-term fcapi overrides overlapping hours.
	if longPayload != nil && len(longPayload.Data) > 0 {
		mergeRawForecastItems(byHour, longPayload.Data)
		if longPayload.Model != "" {
			models = append(models, longPayload.Model)
		}
		if longPayload.Location.Lat != 0 || longPayload.Location.Lon != 0 {
			respLat, respLon = longPayload.Location.Lat, longPayload.Location.Lon
		}
	}

	if shortPayload != nil && len(shortPayload.Data) > 0 {
		mergeRawForecastItems(byHour, shortPayload.Data)
		if shortPayload.Model != "" {
			models = append([]string{shortPayload.Model}, models...)
		}
		if shortPayload.Location.Lat != 0 || shortPayload.Location.Lon != 0 {
			respLat, respLon = shortPayload.Location.Lat, shortPayload.Location.Lon
		}
	}

	keys := make([]int64, 0, len(byHour))
	for k := range byHour {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	hourly := make([]store.ForecastPoint, 0, len(keys))
	for _, k := range keys {
		hourly = append(hourly, byHour[k])
	}

	now := time.Now().UTC()
	return &store.CachedForecast{
		Lat:       respLat,
		Lon:       respLon,
		Model:     strings.Join(models, "+"),
		FetchedAt: now,
		Hourly:    hourly,
	}, nil
}

func (c *Client) fetchMeteoEndpoint(ctx context.Context, path string, lat, lon float64, model string) (*rawForecastPayload, error) {
	u, err := url.Parse(c.meteoForecastBaseURL + path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	if c.meteoForecastToken != "" {
		q.Set("token", c.meteoForecastToken)
	}
	q.Set("lat", fmt.Sprintf("%.6f", lat))
	q.Set("lon", fmt.Sprintf("%.6f", lon))
	if model != "" {
		q.Set("m", model)
	}
	u.RawQuery = q.Encode()

	body, status, err := c.getBytes(ctx, u.String())
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", status, path)
	}

	var env rawForecastEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if env.Data == nil {
		return nil, fmt.Errorf("empty data envelope from %s", path)
	}
	return env.Data, nil
}

func mergeRawForecastItems(dst map[int64]store.ForecastPoint, items []rawForecastItem) {
	// Accumulate 10-minute precipitation entries per hour for early HYBRID nowcast hours.
	tenMinPrecip := make(map[int64]float64)
	tenMinRain := make(map[int64]float64)
	tenMinSnow := make(map[int64]float64)
	hasTenMin := make(map[int64]bool)

	for _, it := range items {
		if it.Type != "Type_Ten_Minutes" {
			continue
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(it.Date))
		if err != nil {
			continue
		}
		hk := t.UTC().Truncate(time.Hour).Unix()
		if v := parseOptFloat(it.Precipitation10m); v != nil {
			tenMinPrecip[hk] += *v
			hasTenMin[hk] = true
		}
		if v := parseOptFloat(it.Rain10m); v != nil {
			tenMinRain[hk] += *v
		}
		if v := parseOptFloat(it.Snow10m); v != nil {
			tenMinSnow[hk] += *v
		}
	}

	for _, it := range items {
		if it.Type != "" && it.Type != "Type_Hour" {
			continue
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(it.Date))
		if err != nil {
			continue
		}
		ts := t.UTC().Truncate(time.Hour)
		hk := ts.Unix()

		pt := store.ForecastPoint{
			Time:          ts,
			TemperatureC:  kelvinToCelsius(parseOptFloat(it.Temperature)),
			ApparentTempC: kelvinToCelsius(parseOptFloat(it.Chill)),
			HumidityPct:   parseOptFloat(it.Humidity),
			CloudCoverPct: parseOptFloat(it.Cloud),
			WindSpeedMS:   parseOptFloat(it.WindSpeed),
			WindGustMS:    parseOptFloat(it.WindGust),
			WindDirDeg:    parseOptFloat(it.WindDir),
			PressureHPa:   pascalToHPa(firstNonNil(parseOptFloat(it.PressureMSL), parseOptFloat(it.Pressure))),
		}

		precip := parseOptFloat(it.Precipitation)
		rain := parseOptFloat(it.Rain)
		snowMM := parseOptFloat(it.Snow)
		if precip == nil {
			if p10 := parseOptFloat(it.Precipitation10m); p10 != nil {
				sum := *p10
				if hasTenMin[hk] {
					sum += tenMinPrecip[hk]
				}
				precip = &sum
			}
		}
		if rain == nil {
			if r10 := parseOptFloat(it.Rain10m); r10 != nil {
				sum := *r10
				if hasTenMin[hk] {
					sum += tenMinRain[hk]
				}
				rain = &sum
			}
		}
		if snowMM == nil {
			if s10 := parseOptFloat(it.Snow10m); s10 != nil {
				sum := *s10
				if hasTenMin[hk] {
					sum += tenMinSnow[hk]
				}
				snowMM = &sum
			}
		}

		pt.PrecipitationMM = precip
		pt.RainMM = rain
		if snowMM != nil {
			// IMGW reports snow in liquid water equivalent (mm); Open-Meteo snowfall is in cm (~0.7 cm per mm SWE).
			snowCM := *snowMM * 0.7
			pt.SnowfallCM = &snowCM
		}

		icon := strings.TrimSpace(it.Icon)
		if icon == "" {
			icon = strings.TrimSpace(it.Icon10)
		}
		pt.IconCode = icon

		dst[hk] = pt
	}
}

func kelvinToCelsius(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	if c > 150.0 {
		c -= 273.15
	}
	return &c
}

func pascalToHPa(v *float64) *float64 {
	if v == nil {
		return nil
	}
	p := *v
	if p > 2000.0 {
		p /= 100.0
	}
	return &p
}

func firstNonNil(a, b *float64) *float64 {
	if a != nil {
		return a
	}
	return b
}
