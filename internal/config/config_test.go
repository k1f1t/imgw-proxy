package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseLocationsEnv(t *testing.T) {
	locs, err := ParseLocationsEnv("12375;50.06,19.94;Home:52.23,21.01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(locs) != 3 {
		t.Fatalf("expected 3 locations, got %d", len(locs))
	}
	if locs[0].StationID != "12375" {
		t.Errorf("expected station_id 12375, got %+v", locs[0])
	}
	if locs[1].Lat != 50.06 || locs[1].Lon != 19.94 {
		t.Errorf("unexpected coords for locs[1]: %+v", locs[1])
	}
	if locs[2].Name != "Home" || locs[2].Lat != 52.23 || locs[2].Lon != 21.01 {
		t.Errorf("unexpected locs[2]: %+v", locs[2])
	}
}

func TestLoadFromYAMLAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "cfg.yaml")
	content := `
listen_addr: "127.0.0.1:9090"
forecast_mode: "selected"
retention_days: 30
forecast_locations:
  - name: "Warszawa"
    station_id: "12375"
`
	if err := os.WriteFile(yamlPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_FILE", yamlPath)
	t.Setenv("RETENTION_DAYS", "45")
	t.Setenv("SYNOP_POLL_INTERVAL", "25m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Errorf("expected listen_addr 127.0.0.1:9090, got %s", cfg.ListenAddr)
	}
	if cfg.ForecastMode != ForecastModeSelected {
		t.Errorf("expected selected mode, got %s", cfg.ForecastMode)
	}
	if cfg.AllowOnDemandForecasts() {
		t.Errorf("expected AllowOnDemandForecasts=false by default in selected mode")
	}
	if cfg.RetentionDays != 45 {
		t.Errorf("expected env override retention_days=45, got %d", cfg.RetentionDays)
	}
	if cfg.SynopPollInterval != 25*time.Minute {
		t.Errorf("expected synop_poll_interval=25m, got %v", cfg.SynopPollInterval)
	}
}

func TestValidateRejectsBadForecastMode(t *testing.T) {
	cfg := Default()
	cfg.ForecastMode = "invalid"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid forecast_mode")
	}
}
