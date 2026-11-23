package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	ForecastModeAll      = "all"
	ForecastModeSelected = "selected"
)

// LocationConfig represents a configured forecast location (either by station ID or lat/lon).
type LocationConfig struct {
	Name      string  `yaml:"name" json:"name"`
	StationID string  `yaml:"station_id" json:"station_id,omitempty"`
	Lat       float64 `yaml:"lat" json:"lat"`
	Lon       float64 `yaml:"lon" json:"lon"`
}

// Config holds all runtime settings for the IMGW Open-Meteo proxy.
type Config struct {
	ListenAddr string `yaml:"listen_addr"`
	DBPath     string `yaml:"db_path"`
	APIKey     string `yaml:"api_key"`
	LogLevel   string `yaml:"log_level"`

	DanepubliczneBaseURL string        `yaml:"danepubliczne_base_url"`
	MeteoForecastBaseURL string        `yaml:"meteo_forecast_base_url"`
	MeteoForecastToken   string        `yaml:"meteo_forecast_token"`
	ForecastModel        string        `yaml:"forecast_model"`
	UpstreamTimeout      time.Duration `yaml:"upstream_timeout"`
	UserAgent            string        `yaml:"user_agent"`

	SynopPollInterval    time.Duration `yaml:"synop_poll_interval"`
	MeteoPollInterval    time.Duration `yaml:"meteo_poll_interval"`
	HydroPollInterval    time.Duration `yaml:"hydro_poll_interval"`
	WarningsPollInterval time.Duration `yaml:"warnings_poll_interval"`

	ForecastMode          string           `yaml:"forecast_mode"`
	ForecastPollInterval  time.Duration    `yaml:"forecast_poll_interval"`
	ForecastTTL           time.Duration    `yaml:"forecast_ttl"`
	ForecastRequestDelay  time.Duration    `yaml:"forecast_request_delay"`
	ForecastAllowOnDemand *bool            `yaml:"forecast_allow_ondemand"`
	ForecastLocations     []LocationConfig `yaml:"forecast_locations"`

	RetentionDays int `yaml:"retention_days"`
}

// Default returns sensible, IMGW-friendly self-hosted defaults.
func Default() Config {
	return Config{
		ListenAddr:           "127.0.0.1:8080",
		DBPath:               "./data/imgw.db",
		APIKey:               "",
		LogLevel:             "info",
		DanepubliczneBaseURL: "https://danepubliczne.imgw.pl/api/data",
		MeteoForecastBaseURL: "https://meteo.imgw.pl",
		MeteoForecastToken:   "p4DXKjsYadfBV21TYrDk",
		ForecastModel:        "hybrid",
		UpstreamTimeout:      15 * time.Second,
		UserAgent:            "imgw-openmeteo-proxy/1.0 (self-hosted)",
		SynopPollInterval:    20 * time.Minute,
		MeteoPollInterval:    10 * time.Minute,
		HydroPollInterval:    15 * time.Minute,
		WarningsPollInterval: 10 * time.Minute,
		ForecastMode:         ForecastModeAll,
		ForecastPollInterval: 2 * time.Hour,
		ForecastTTL:          2 * time.Hour,
		ForecastRequestDelay: 750 * time.Millisecond,
		RetentionDays:        90,
	}
}

// AllowOnDemandForecasts reports whether arbitrary coordinates may trigger an on-demand
// cached fetch from meteo.imgw.pl when not yet in SQLite.
func (c Config) AllowOnDemandForecasts() bool {
	if c.ForecastAllowOnDemand != nil {
		return *c.ForecastAllowOnDemand
	}
	return c.ForecastMode == ForecastModeAll
}

// Load reads defaults, optional YAML config file (CONFIG_FILE), and environment overrides.
func Load() (Config, error) {
	cfg := Default()

	if path := strings.TrimSpace(os.Getenv("CONFIG_FILE")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config file %q: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config file %q: %w", path, err)
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) error {
	if v, ok := os.LookupEnv("LISTEN_ADDR"); ok && strings.TrimSpace(v) != "" {
		cfg.ListenAddr = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("DB_PATH"); ok && strings.TrimSpace(v) != "" {
		cfg.DBPath = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("API_KEY"); ok {
		cfg.APIKey = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("LOG_LEVEL"); ok && strings.TrimSpace(v) != "" {
		cfg.LogLevel = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := os.LookupEnv("DANEPUBLICZNE_BASE_URL"); ok && strings.TrimSpace(v) != "" {
		cfg.DanepubliczneBaseURL = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if v, ok := os.LookupEnv("METEO_FORECAST_BASE_URL"); ok && strings.TrimSpace(v) != "" {
		cfg.MeteoForecastBaseURL = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if v, ok := os.LookupEnv("METEO_FORECAST_TOKEN"); ok && strings.TrimSpace(v) != "" {
		cfg.MeteoForecastToken = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("FORECAST_MODEL"); ok && strings.TrimSpace(v) != "" {
		cfg.ForecastModel = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := os.LookupEnv("USER_AGENT"); ok && strings.TrimSpace(v) != "" {
		cfg.UserAgent = strings.TrimSpace(v)
	}

	durFields := []struct {
		env string
		dst *time.Duration
	}{
		{"UPSTREAM_TIMEOUT", &cfg.UpstreamTimeout},
		{"SYNOP_POLL_INTERVAL", &cfg.SynopPollInterval},
		{"METEO_POLL_INTERVAL", &cfg.MeteoPollInterval},
		{"HYDRO_POLL_INTERVAL", &cfg.HydroPollInterval},
		{"WARNINGS_POLL_INTERVAL", &cfg.WarningsPollInterval},
		{"FORECAST_POLL_INTERVAL", &cfg.ForecastPollInterval},
		{"FORECAST_TTL", &cfg.ForecastTTL},
		{"FORECAST_REQUEST_DELAY", &cfg.ForecastRequestDelay},
	}
	for _, f := range durFields {
		if v, ok := os.LookupEnv(f.env); ok && strings.TrimSpace(v) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("invalid duration for %s=%q: %w", f.env, v, err)
			}
			*f.dst = d
		}
	}

	if v, ok := os.LookupEnv("FORECAST_MODE"); ok && strings.TrimSpace(v) != "" {
		cfg.ForecastMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := os.LookupEnv("FORECAST_ALLOW_ONDEMAND"); ok && strings.TrimSpace(v) != "" {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("invalid boolean for FORECAST_ALLOW_ONDEMAND=%q: %w", v, err)
		}
		cfg.ForecastAllowOnDemand = &b
	}
	if v, ok := os.LookupEnv("FORECAST_LOCATIONS"); ok && strings.TrimSpace(v) != "" {
		locs, err := ParseLocationsEnv(v)
		if err != nil {
			return fmt.Errorf("invalid FORECAST_LOCATIONS: %w", err)
		}
		cfg.ForecastLocations = locs
	}
	if v, ok := os.LookupEnv("RETENTION_DAYS"); ok && strings.TrimSpace(v) != "" {
		days, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("invalid integer for RETENTION_DAYS=%q: %w", v, err)
		}
		cfg.RetentionDays = days
	}

	return nil
}

// ParseLocationsEnv parses semicolon-separated entries:
//   - "12375" (synoptic station ID)
//   - "52.23,21.01" (lat,lon)
//   - "Warszawa:52.23,21.01" (name:lat,lon)
func ParseLocationsEnv(raw string) ([]LocationConfig, error) {
	parts := strings.Split(raw, ";")
	var out []LocationConfig
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		var loc LocationConfig
		if idx := strings.Index(item, ":"); idx != -1 {
			loc.Name = strings.TrimSpace(item[:idx])
			item = strings.TrimSpace(item[idx+1:])
		}
		if strings.Contains(item, ",") {
			coords := strings.Split(item, ",")
			if len(coords) != 2 {
				return nil, fmt.Errorf("invalid coordinate pair %q", part)
			}
			lat, err := strconv.ParseFloat(strings.TrimSpace(coords[0]), 64)
			if err != nil {
				return nil, fmt.Errorf("invalid latitude in %q: %w", part, err)
			}
			lon, err := strconv.ParseFloat(strings.TrimSpace(coords[1]), 64)
			if err != nil {
				return nil, fmt.Errorf("invalid longitude in %q: %w", part, err)
			}
			loc.Lat = lat
			loc.Lon = lon
			if loc.Name == "" {
				loc.Name = fmt.Sprintf("%.4f,%.4f", lat, lon)
			}
		} else {
			loc.StationID = item
			if loc.Name == "" {
				loc.Name = item
			}
		}
		out = append(out, loc)
	}
	return out, nil
}

// Validate ensures all configuration values are coherent and safe.
func (c Config) Validate() error {
	if c.ForecastMode != ForecastModeAll && c.ForecastMode != ForecastModeSelected {
		return fmt.Errorf("forecast_mode must be %q or %q, got %q", ForecastModeAll, ForecastModeSelected, c.ForecastMode)
	}
	if c.RetentionDays < 1 {
		return fmt.Errorf("retention_days must be >= 1, got %d", c.RetentionDays)
	}
	if c.UpstreamTimeout <= 0 {
		return fmt.Errorf("upstream_timeout must be > 0")
	}
	if c.SynopPollInterval <= 0 || c.MeteoPollInterval <= 0 || c.HydroPollInterval <= 0 || c.WarningsPollInterval <= 0 {
		return fmt.Errorf("all poll intervals must be > 0")
	}
	if c.ForecastPollInterval <= 0 || c.ForecastTTL <= 0 {
		return fmt.Errorf("forecast_poll_interval and forecast_ttl must be > 0")
	}
	if c.ForecastRequestDelay < 0 {
		return fmt.Errorf("forecast_request_delay must be >= 0")
	}
	for _, loc := range c.ForecastLocations {
		if loc.StationID == "" {
			if loc.Lat < -90 || loc.Lat > 90 || loc.Lon < -180 || loc.Lon > 180 {
				return fmt.Errorf("location %q has out-of-range coordinates (%.4f, %.4f)", loc.Name, loc.Lat, loc.Lon)
			}
		}
	}
	return nil
}
