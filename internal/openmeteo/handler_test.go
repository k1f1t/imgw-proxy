package openmeteo

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"imgw-openmeteo-proxy/internal/store"
)

type stubProvider struct {
	fc *store.CachedForecast
}

func (p stubProvider) GetForecastForCoords(ctx context.Context, lat, lon float64) (*store.CachedForecast, error) {
	return p.fc, nil
}

func fptr(v float64) *float64 { return &v }

func TestWeatherCodeFromIMGWIcon(t *testing.T) {
	cases := []struct {
		icon string
		want int
	}{
		{"n0z00d", 0},
		{"n3z00d", 1},
		{"n4z00n", 2},
		{"n8z00d", 3},
		{"n8z61d", 61},
		{"n8z73n", 73},
		{"n8z95d", 95},
	}
	for _, tc := range cases {
		got, ok := WeatherCodeFromIMGWIcon(tc.icon)
		if !ok || got != tc.want {
			t.Errorf("WeatherCodeFromIMGWIcon(%q) = (%d, %v), want (%d, true)", tc.icon, got, ok, tc.want)
		}
	}
}

func TestOpenMeteoForecastHandlerStrictSchemaAndUnits(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "om.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fixedNow := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

	elev := 106.0
	if err := st.UpsertStations(ctx, []store.Station{
		{Source: "synop", ID: "12375", Name: "Warszawa", Lat: 52.1628, Lon: 20.9611, Elevation: &elev, TERYT: "1465", UpdatedAt: fixedNow},
	}); err != nil {
		t.Fatal(err)
	}
	// Historical past observation + current observation.
	if err := st.UpsertMeteoObservations(ctx, []store.MeteoObservation{
		{
			Source:          "synop",
			StationID:       "12375",
			ObservedAt:      fixedNow.Add(-2 * time.Hour),
			TemperatureC:    fptr(1.0),
			HumidityPct:     fptr(90.0),
			WindSpeedMS:     fptr(10.0), // 36.0 km/h
			WindDirDeg:      fptr(270.0),
			PrecipitationMM: fptr(0.0),
			PressureHPa:     fptr(1010.0),
			FetchedAt:       fixedNow,
		},
		{
			Source:          "synop",
			StationID:       "12375",
			ObservedAt:      fixedNow,
			TemperatureC:    fptr(2.0),
			HumidityPct:     fptr(80.0),
			WindSpeedMS:     fptr(5.0), // 18.0 km/h
			WindDirDeg:      fptr(260.0),
			PrecipitationMM: fptr(0.5),
			PressureHPa:     fptr(1012.5),
			FetchedAt:       fixedNow,
		},
	}); err != nil {
		t.Fatal(err)
	}

	fc := &store.CachedForecast{
		LocationKey: "synop:12375",
		Lat:         52.23,
		Lon:         21.01,
		Elevation:   &elev,
		Model:       "HYBRID+GFS",
		FetchedAt:   fixedNow,
		ExpiresAt:   fixedNow.Add(2 * time.Hour),
		Hourly: []store.ForecastPoint{
			{Time: fixedNow, TemperatureC: fptr(2.5), HumidityPct: fptr(82.0), WindSpeedMS: fptr(5.5), WindGustMS: fptr(9.0), WindDirDeg: fptr(260.0), PrecipitationMM: fptr(0.4), PressureHPa: fptr(1012.0), CloudCoverPct: fptr(100.0), IconCode: "n8z61d"},
			{Time: fixedNow.Add(time.Hour), TemperatureC: fptr(3.0), HumidityPct: fptr(78.0), WindSpeedMS: fptr(6.0), WindGustMS: fptr(10.0), WindDirDeg: fptr(265.0), PrecipitationMM: fptr(0.0), PressureHPa: fptr(1013.0), CloudCoverPct: fptr(50.0), IconCode: "n4z00d"},
			{Time: fixedNow.Add(24 * time.Hour), TemperatureC: fptr(4.0), HumidityPct: fptr(70.0), WindSpeedMS: fptr(4.0), WindGustMS: fptr(7.0), WindDirDeg: fptr(240.0), PrecipitationMM: fptr(1.2), PressureHPa: fptr(1015.0), CloudCoverPct: fptr(90.0), IconCode: "n8z63d"},
		},
	}

	h := NewHandler(st, stubProvider{fc: fc})
	h.nowFunc = func() time.Time { return fixedNow }

	// 1. Standard Home Assistant style request (default wind unit = km/h, timezone=auto -> Europe/Warsaw).
	req := httptest.NewRequest(http.MethodGet,
		"/v1/forecast?latitude=52.23&longitude=21.01&timezone=auto&current_weather=true&current=temperature_2m,relative_humidity_2m,apparent_temperature,wind_speed_10m,surface_pressure,weather_code,is_day&hourly=temperature_2m,wind_speed_10m,precipitation,weather_code&daily=weather_code,temperature_2m_max,temperature_2m_min,precipitation_sum,sunrise,sunset&past_days=1&forecast_days=3",
		nil,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["timezone"] != "Europe/Warsaw" {
		t.Errorf("expected timezone Europe/Warsaw, got %v", body["timezone"])
	}
	if body["utc_offset_seconds"] != float64(3600) {
		t.Errorf("expected utc_offset_seconds 3600 in Nov CET, got %v", body["utc_offset_seconds"])
	}

	curUnits := body["current_units"].(map[string]any)
	if curUnits["wind_speed_10m"] != "km/h" {
		t.Errorf("expected default Open-Meteo wind_speed_10m unit km/h, got %v", curUnits["wind_speed_10m"])
	}
	cur := body["current"].(map[string]any)
	// Current synop wind = 5.0 m/s -> 18.0 km/h
	if cur["wind_speed_10m"] != float64(18.0) {
		t.Errorf("expected current wind_speed_10m=18.0 km/h, got %v", cur["wind_speed_10m"])
	}
	if cur["temperature_2m"] != float64(2.0) {
		t.Errorf("expected current temperature_2m=2.0, got %v", cur["temperature_2m"])
	}
	if cur["weather_code"] != float64(61) {
		t.Errorf("expected weather_code=61 from icon n8z61d, got %v", cur["weather_code"])
	}
	if cur["is_day"] != float64(1) {
		t.Errorf("expected is_day=1 at 12:00 UTC in Warszawa, got %v", cur["is_day"])
	}

	// Verify daily emits full Open-Meteo requested range: past_days=1 + forecast_days=3 -> 4 days (Nov 22..25).
	daily := body["daily"].(map[string]any)
	dailyTimes := daily["time"].([]any)
	if len(dailyTimes) != 4 || dailyTimes[0] != "2026-11-22" || dailyTimes[3] != "2026-11-25" {
		t.Errorf("unexpected daily times: %v", dailyTimes)
	}
	hourly := body["hourly"].(map[string]any)
	hourlyTimes := hourly["time"].([]any)
	if len(hourlyTimes) != 96 {
		t.Errorf("expected 96 hourly slots for past_days=1 + forecast_days=3, got %d", len(hourlyTimes))
	}

	// 2. Fahrenheit + mph + inch + multi-location array output test.
	reqMulti := httptest.NewRequest(http.MethodGet,
		"/v1/forecast?latitude=52.23,50.06&longitude=21.01,19.94&temperature_unit=fahrenheit&wind_speed_unit=mph&precipitation_unit=inch&current=temperature_2m,wind_speed_10m,precipitation",
		nil,
	)
	recMulti := httptest.NewRecorder()
	h.ServeHTTP(recMulti, reqMulti)
	if recMulti.Code != http.StatusOK {
		t.Fatalf("multi-location expected 200, got %d: %s", recMulti.Code, recMulti.Body.String())
	}
	var arr []map[string]any
	if err := json.Unmarshal(recMulti.Body.Bytes(), &arr); err != nil {
		t.Fatalf("expected JSON array for multi-location: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 location responses, got %d", len(arr))
	}
	cur0 := arr[0]["current"].(map[string]any)
	// 2.0°C = 35.6°F
	if math.Abs(cur0["temperature_2m"].(float64)-35.6) > 0.1 {
		t.Errorf("expected 35.6°F, got %v", cur0["temperature_2m"])
	}

	// 3. Invalid params return Open-Meteo error JSON format.
	reqBad := httptest.NewRequest(http.MethodGet, "/v1/forecast?latitude=52.23", nil)
	recBad := httptest.NewRecorder()
	h.ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recBad.Code)
	}
	var errObj map[string]any
	if err := json.Unmarshal(recBad.Body.Bytes(), &errObj); err != nil || errObj["error"] != true {
		t.Fatalf("expected Open-Meteo error JSON, got %s", recBad.Body.String())
	}
}

func TestHourlyMeteo10MinPrecipitationSummation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "meteo_sum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	if err := st.UpsertStations(ctx, []store.Station{
		{Source: "meteo", ID: "352200375", Name: "WARSZAWA-OKĘCIE", Lat: 52.1628, Lon: 20.9611, UpdatedAt: base},
	}); err != nil {
		t.Fatal(err)
	}
	// Three 10-minute observations in the 10:00 UTC hour totaling 1.5 mm.
	if err := st.UpsertMeteoObservations(ctx, []store.MeteoObservation{
		{Source: "meteo", StationID: "352200375", ObservedAt: base.Add(10 * time.Minute), TemperatureC: fptr(4.0), PrecipitationMM: fptr(0.4), FetchedAt: base},
		{Source: "meteo", StationID: "352200375", ObservedAt: base.Add(20 * time.Minute), TemperatureC: fptr(4.1), PrecipitationMM: fptr(0.6), FetchedAt: base},
		{Source: "meteo", StationID: "352200375", ObservedAt: base.Add(30 * time.Minute), TemperatureC: fptr(4.2), PrecipitationMM: fptr(0.5), FetchedAt: base},
	}); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(st, nil)
	h.nowFunc = func() time.Time { return base.Add(35 * time.Minute) }

	req := httptest.NewRequest(http.MethodGet,
		"/v1/forecast?latitude=52.1628&longitude=20.9611&timezone=UTC&current=precipitation&hourly=precipitation&forecast_days=1",
		nil,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Current map[string]any   `json:"current"`
		Hourly  map[string][]any `json:"hourly"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if math.Abs(body.Current["precipitation"].(float64)-1.5) > 0.01 {
		t.Errorf("expected current.precipitation=1.5mm (summed last hour), got %v", body.Current["precipitation"])
	}
	// Hour index 10 corresponds to 10:00 UTC.
	if len(body.Hourly["precipitation"]) != 24 {
		t.Fatalf("expected 24 hourly slots for forecast_days=1, got %d", len(body.Hourly["precipitation"]))
	}
	if math.Abs(body.Hourly["precipitation"][10].(float64)-1.5) > 0.01 {
		t.Errorf("expected hourly.precipitation[10]=1.5mm, got %v", body.Hourly["precipitation"][10])
	}
}
