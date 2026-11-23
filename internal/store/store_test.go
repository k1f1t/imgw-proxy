package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func fptr(v float64) *float64 { return &v }
func iptr(v int) *int         { return &v }

func TestStoreObservationsDeduplicationAndPruning(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Hour)
	oldTime := now.AddDate(0, 0, -100)

	// Insert two observations, then re-insert one with updated pressure (deduplication test).
	obs := []MeteoObservation{
		{
			Source:       "synop",
			StationID:    "12375",
			ObservedAt:   oldTime,
			TemperatureC: fptr(10.0),
			FetchedAt:    now,
		},
		{
			Source:       "synop",
			StationID:    "12375",
			ObservedAt:   now,
			TemperatureC: fptr(2.5),
			HumidityPct:  fptr(85.0),
			FetchedAt:    now,
		},
	}
	if err := st.UpsertMeteoObservations(ctx, obs); err != nil {
		t.Fatalf("UpsertMeteoObservations failed: %v", err)
	}
	// Duplicate primary key with pressure added and temperature omitted -> COALESCE preserves temp.
	if err := st.UpsertMeteoObservations(ctx, []MeteoObservation{
		{
			Source:      "synop",
			StationID:   "12375",
			ObservedAt:  now,
			PressureHPa: fptr(1012.3),
			FetchedAt:   now.Add(time.Minute),
		},
	}); err != nil {
		t.Fatalf("duplicate UpsertMeteoObservations failed: %v", err)
	}

	latest, err := st.LatestMeteoObservation(ctx, "synop", "12375")
	if err != nil || latest == nil {
		t.Fatalf("LatestMeteoObservation failed: %v", err)
	}
	if latest.TemperatureC == nil || *latest.TemperatureC != 2.5 {
		t.Fatalf("expected preserved temp 2.5, got %v", latest.TemperatureC)
	}
	if latest.PressureHPa == nil || *latest.PressureHPa != 1012.3 {
		t.Fatalf("expected updated pressure 1012.3, got %v", latest.PressureHPa)
	}

	// Prune older than 90 days -> should remove oldTime observation only.
	deleted, err := st.PruneOlderThan(ctx, now.AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("PruneOlderThan failed: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 pruned row, got %d", deleted)
	}

	list, err := st.ListMeteoObservations(ctx, "synop", "12375", oldTime.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListMeteoObservations failed: %v", err)
	}
	if len(list) != 1 || !list[0].ObservedAt.Equal(now) {
		t.Fatalf("expected only recent observation remaining, got %+v", list)
	}
}

func TestStoreForecastAndWarnings(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Hour)
	fc := CachedForecast{
		LocationKey: "synop:12375",
		Lat:         52.1628,
		Lon:         20.9611,
		StationID:   "12375",
		Name:        "Warszawa",
		Model:       "HYBRID+GFS",
		FetchedAt:   now,
		ExpiresAt:   now.Add(2 * time.Hour),
		Hourly: []ForecastPoint{
			{Time: now, TemperatureC: fptr(3.1), IconCode: "n4z61d"},
			{Time: now.Add(time.Hour), TemperatureC: fptr(2.8), IconCode: "n8z00n"},
		},
	}
	if err := st.UpsertForecast(ctx, fc); err != nil {
		t.Fatalf("UpsertForecast failed: %v", err)
	}
	got, err := st.GetForecast(ctx, "synop:12375")
	if err != nil || got == nil {
		t.Fatalf("GetForecast failed: %v", err)
	}
	if len(got.Hourly) != 2 || got.Hourly[0].IconCode != "n4z61d" {
		t.Fatalf("unexpected cached forecast hourly: %+v", got.Hourly)
	}

	// ReplaceWarnings should replace old set atomically.
	w1 := []Warning{{Type: "meteo", ID: "w1", Event: "Oblodzenie", Level: "1", Probability: iptr(80), TERYTs: []string{"1465"}, FetchedAt: now}}
	if err := st.ReplaceWarnings(ctx, "meteo", w1); err != nil {
		t.Fatal(err)
	}
	w2 := []Warning{{Type: "meteo", ID: "w2", Event: "Silny wiatr", Level: "2", Probability: iptr(90), TERYTs: []string{"1261"}, FetchedAt: now}}
	if err := st.ReplaceWarnings(ctx, "meteo", w2); err != nil {
		t.Fatal(err)
	}
	listed, err := st.ListWarnings(ctx, "meteo")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "w2" {
		t.Fatalf("expected replaced warnings [w2], got %+v", listed)
	}
}
