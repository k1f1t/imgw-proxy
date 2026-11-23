package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"imgw-openmeteo-proxy/internal/config"
	"imgw-openmeteo-proxy/internal/imgw"
	"imgw-openmeteo-proxy/internal/scheduler"
	"imgw-openmeteo-proxy/internal/store"
)

func fptr(v float64) *float64 { return &v }

func TestAPIAuthAndEndpoints(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC()
	if err := st.UpsertStations(ctx, []store.Station{
		{Source: "synop", ID: "12375", Name: "Warszawa", Lat: 52.1628, Lon: 20.9611, TERYT: "1465", Voivodeship: "mazowieckie", UpdatedAt: now},
		{Source: "hydro", ID: "152210010", Name: "Warszawa-Bulwary", Lat: 52.24, Lon: 21.03, River: "Wisła", Voivodeship: "mazowieckie", WarningLevel: fptr(600), AlarmLevel: fptr(650), UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertHydroObservations(ctx, []store.HydroObservation{
		{StationID: "152210010", ObservedAt: now, WaterLevelCM: fptr(620), FetchedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	validFrom := now.Add(-time.Hour)
	validTo := now.Add(4 * time.Hour)
	if err := st.ReplaceWarnings(ctx, "meteo", []store.Warning{
		{Type: "meteo", ID: "m1", Event: "Silny wiatr", Level: "1", ValidFrom: &validFrom, ValidTo: &validTo, TERYTs: []string{"1465"}, FetchedAt: now},
		{Type: "meteo", ID: "m2", Event: "Upał", Level: "2", ValidFrom: &validFrom, ValidTo: &validTo, TERYTs: []string{"1261"}, FetchedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.APIKey = "super-secret-key"
	cfg.ForecastMode = config.ForecastModeSelected
	cfg.ForecastLocations = []config.LocationConfig{{StationID: "12375"}}

	srv := NewServer(cfg, st, nil, nil)

	// /healthz should bypass API key auth for Docker healthchecks.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz expected 200, got %d", rec.Code)
	}

	// /v1/stations without key -> 401.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/stations", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without API key, got %d", rec.Code)
	}

	// /v1/attribution with Bearer token -> 200 and exact Polish phrases.
	req := httptest.NewRequest(http.MethodGet, "/v1/attribution", nil)
	req.Header.Set("Authorization", "Bearer super-secret-key")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with Bearer token, got %d", rec.Code)
	}
	var attr map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &attr)
	if attr["source"] != AttributionSourcePL {
		t.Fatalf("missing required IMGW attribution phrase, got %v", attr["source"])
	}

	// /v1/hydro with X-API-Key -> returns flood_warning=true (620 >= 600), flood_alarm=false (620 < 650).
	reqHydro := httptest.NewRequest(http.MethodGet, "/v1/hydro?latitude=52.23&longitude=21.01", nil)
	reqHydro.Header.Set("X-API-Key", "super-secret-key")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, reqHydro)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/hydro expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var hydroResp struct {
		Count    int          `json:"count"`
		Stations []hydroEntry `json:"stations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hydroResp); err != nil {
		t.Fatal(err)
	}
	if hydroResp.Count != 1 || hydroResp.Stations[0].WarningState == nil || !*hydroResp.Stations[0].WarningState || *hydroResp.Stations[0].AlarmState {
		t.Fatalf("unexpected hydro warning/alarm state: %+v", hydroResp.Stations)
	}

	// Verify partial or NaN coords are rejected with 400 on /v1/hydro.
	reqBadHydro := httptest.NewRequest(http.MethodGet, "/v1/hydro?latitude=NaN&longitude=21.01&apikey=super-secret-key", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, reqBadHydro)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for NaN latitude, got %d", rec.Code)
	}

	// /v1/warnings with ?apikey= & Warszawa coords -> matches only TERYT 1465 warning.
	reqWarn := httptest.NewRequest(http.MethodGet, "/v1/warnings?latitude=52.23&longitude=21.01&apikey=super-secret-key", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, reqWarn)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/warnings expected 200, got %d", rec.Code)
	}
	var warnResp struct {
		Count           int            `json:"count"`
		TERYT           string         `json:"teryt"`
		ProcessedNotice string         `json:"processed_notice"`
		Warnings        []warningEntry `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &warnResp); err != nil {
		t.Fatal(err)
	}
	if warnResp.Count != 1 || warnResp.TERYT != "1465" || warnResp.Warnings[0].ID != "m1" || warnResp.Warnings[0].Status != "active" {
		t.Fatalf("unexpected warnings filter result: %+v", warnResp)
	}
	if warnResp.ProcessedNotice != AttributionProcessedPL {
		t.Fatalf("missing processed_notice in /v1/warnings, got %q", warnResp.ProcessedNotice)
	}
}

func TestSchedulerSelectedModeAndStaleFallback(t *testing.T) {
	var upstreamHits atomic.Int32
	var failUpstream atomic.Bool

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if failUpstream.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{
			"data": {
				"Model": "HYBRID",
				"Valid": true,
				"Location": {"Lat": 52.1628, "Lon": 20.9611},
				"Data": [
					{"Type":"Type_Hour","Date":"2026-11-23T17:00:00Z","Temperature":"275.15","PressureMSL":"101200","Humidity":"85","Cloud":"50","Wind_Speed":"4.0","Wind_Dir":"270","Precipitation":"0.0","Icon":"n4z00n"}
				]
			}
		}`))
	}))
	defer ts.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.ForecastMode = config.ForecastModeSelected
	cfg.ForecastLocations = []config.LocationConfig{{StationID: "12375"}}
	cfg.ForecastRequestDelay = 0
	cfg.ForecastTTL = time.Hour

	client := imgw.NewClient(ts.URL, ts.URL, "tok", "hybrid", "test", 5*time.Second)
	sched, err := scheduler.New(cfg, client, st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(sched.Targets()) != 1 {
		t.Fatalf("expected 1 target in selected mode, got %d", len(sched.Targets()))
	}

	ctx := context.Background()
	if err := sched.SyncForecasts(ctx); err != nil {
		t.Fatalf("SyncForecasts failed: %v", err)
	}
	hitsAfterSync := upstreamHits.Load()
	if hitsAfterSync == 0 {
		t.Fatal("expected upstream calls during initial SyncForecasts")
	}

	// Client query for arbitrary coordinates in selected mode must NOT trigger upstream calls.
	fc, err := sched.GetForecastForCoords(ctx, 50.06, 19.94)
	if err != nil || fc == nil {
		t.Fatalf("expected nearest cached forecast in selected mode, got %v %v", fc, err)
	}
	if upstreamHits.Load() != hitsAfterSync {
		t.Fatalf("selected mode triggered unexpected upstream call on client query")
	}

	// Now test "all" mode stale-while-error fallback when upstream returns 500:
	// nearby query (~20km from Warszawa) falls back to cached Warszawa forecast,
	// whereas distant query (~250km Kraków) refuses to silently serve Warszawa.
	cfgAll := cfg
	cfgAll.ForecastMode = config.ForecastModeAll
	cfgAll.ForecastAllowOnDemand = nil
	schedAll, err := scheduler.New(cfgAll, client, st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(schedAll.Targets()) < 60 {
		t.Fatalf("expected all ~62 synoptic targets in all mode, got %d", len(schedAll.Targets()))
	}

	failUpstream.Store(true)
	fcNearFallback, err := schedAll.GetForecastForCoords(ctx, 52.35, 21.10)
	if err != nil || fcNearFallback == nil {
		t.Fatalf("expected nearby stale-while-error fallback to cached Warszawa forecast, got %v err=%v", fcNearFallback, err)
	}
	fcFarFallback, err := schedAll.GetForecastForCoords(ctx, 50.06, 19.94)
	if err == nil || fcFarFallback != nil {
		t.Fatalf("expected distant query (~250km) not to fall back to Warszawa on upstream 500, got fc=%v err=%v", fcFarFallback, err)
	}
}
