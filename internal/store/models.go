package store

import "time"

// Station represents a synoptic, automatic meteorological, or hydrological IMGW station.
type Station struct {
	Source       string    `json:"source"` // "synop", "meteo", "hydro"
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Lat          float64   `json:"lat"`
	Lon          float64   `json:"lon"`
	Elevation    *float64  `json:"elevation,omitempty"`
	TERYT        string    `json:"teryt,omitempty"`
	Voivodeship  string    `json:"voivodeship,omitempty"`
	River        string    `json:"river,omitempty"`
	WarningLevel *float64  `json:"warning_level,omitempty"`
	AlarmLevel   *float64  `json:"alarm_level,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// MeteoObservation stores a single normalized weather observation from IMGW /synop or /meteo.
// All units are stored in canonical SI/IMGW units: °C, %, m/s, degrees, mm, hPa.
type MeteoObservation struct {
	Source          string    `json:"source"` // "synop" or "meteo"
	StationID       string    `json:"station_id"`
	ObservedAt      time.Time `json:"observed_at"`
	TemperatureC    *float64  `json:"temperature_c,omitempty"`
	GroundTempC     *float64  `json:"ground_temp_c,omitempty"`
	HumidityPct     *float64  `json:"humidity_pct,omitempty"`
	WindSpeedMS     *float64  `json:"wind_speed_ms,omitempty"`
	WindGustMS      *float64  `json:"wind_gust_ms,omitempty"`
	WindDirDeg      *float64  `json:"wind_dir_deg,omitempty"`
	PrecipitationMM *float64  `json:"precipitation_mm,omitempty"`
	PressureHPa     *float64  `json:"pressure_hpa,omitempty"`
	FetchedAt       time.Time `json:"fetched_at"`
}

// HydroObservation stores a single hydrological measurement from IMGW /hydro.
type HydroObservation struct {
	StationID           string     `json:"station_id"`
	ObservedAt          time.Time  `json:"observed_at"`
	WaterLevelCM        *float64   `json:"water_level_cm,omitempty"`
	WaterLevelAt        *time.Time `json:"water_level_at,omitempty"`
	WaterTempC          *float64   `json:"water_temp_c,omitempty"`
	WaterTempAt         *time.Time `json:"water_temp_at,omitempty"`
	WaterFlowM3S        *float64   `json:"water_flow_m3s,omitempty"`
	WaterFlowAt         *time.Time `json:"water_flow_at,omitempty"`
	IcePhenomena        *string    `json:"ice_phenomena,omitempty"`
	IceAt               *time.Time `json:"ice_at,omitempty"`
	VegetationPhenomena *string    `json:"vegetation_phenomena,omitempty"`
	VegetationAt        *time.Time `json:"vegetation_at,omitempty"`
	FetchedAt           time.Time  `json:"fetched_at"`
}

// HydroArea describes an affected hydrological warning area.
type HydroArea struct {
	Voivodeship    string   `json:"voivodeship"`
	Description    string   `json:"description"`
	CatchmentCodes []string `json:"catchment_codes,omitempty"`
}

// Warning stores an IMGW meteorological or hydrological warning.
type Warning struct {
	Type        string      `json:"type"` // "meteo" or "hydro"
	ID          string      `json:"id"`
	Event       string      `json:"event"`
	Level       string      `json:"level"`
	Probability *int        `json:"probability,omitempty"`
	ValidFrom   *time.Time  `json:"valid_from,omitempty"`
	ValidTo     *time.Time  `json:"valid_to,omitempty"`
	PublishedAt *time.Time  `json:"published_at,omitempty"`
	Content     string      `json:"content,omitempty"`
	Comment     string      `json:"comment,omitempty"`
	Office      string      `json:"office,omitempty"`
	TERYTs      []string    `json:"teryts,omitempty"`
	Areas       []HydroArea `json:"areas,omitempty"`
	FetchedAt   time.Time   `json:"fetched_at"`
}

// ForecastPoint represents one normalized hourly forecast step from meteo.imgw.pl.
// Units are canonical: °C, %, m/s, degrees, mm, cm (snow), hPa, % (cloud cover).
type ForecastPoint struct {
	Time            time.Time `json:"time"`
	TemperatureC    *float64  `json:"temperature_c,omitempty"`
	ApparentTempC   *float64  `json:"apparent_temp_c,omitempty"`
	HumidityPct     *float64  `json:"humidity_pct,omitempty"`
	WindSpeedMS     *float64  `json:"wind_speed_ms,omitempty"`
	WindGustMS      *float64  `json:"wind_gust_ms,omitempty"`
	WindDirDeg      *float64  `json:"wind_dir_deg,omitempty"`
	PrecipitationMM *float64  `json:"precipitation_mm,omitempty"`
	RainMM          *float64  `json:"rain_mm,omitempty"`
	SnowfallCM      *float64  `json:"snowfall_cm,omitempty"`
	PrecipProbPct   *float64  `json:"precip_prob_pct,omitempty"`
	PressureHPa     *float64  `json:"pressure_hpa,omitempty"`
	CloudCoverPct   *float64  `json:"cloud_cover_pct,omitempty"`
	WeatherCode     *int      `json:"weather_code,omitempty"`
	IconCode        string    `json:"icon_code,omitempty"`
}

// ForecastMeta holds lightweight metadata for a cached forecast without its hourly JSON payload.
type ForecastMeta struct {
	LocationKey string    `json:"location_key"`
	Lat         float64   `json:"lat"`
	Lon         float64   `json:"lon"`
	StationID   string    `json:"station_id,omitempty"`
	Name        string    `json:"name,omitempty"`
	Elevation   *float64  `json:"elevation,omitempty"`
	Model       string    `json:"model,omitempty"`
	FetchedAt   time.Time `json:"fetched_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// CachedForecast stores a fetched forecast bundle for a location in SQLite.
type CachedForecast struct {
	LocationKey string          `json:"location_key"`
	Lat         float64         `json:"lat"`
	Lon         float64         `json:"lon"`
	StationID   string          `json:"station_id,omitempty"`
	Name        string          `json:"name,omitempty"`
	Elevation   *float64        `json:"elevation,omitempty"`
	Model       string          `json:"model,omitempty"`
	FetchedAt   time.Time       `json:"fetched_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	Hourly      []ForecastPoint `json:"hourly"`
}

// SyncStatus tracks poller health per upstream source.
type SyncStatus struct {
	Source        string     `json:"source"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	RecordCount   int        `json:"record_count"`
}
