package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps SQLite persistence for stations, observations, warnings, forecasts, and sync state.
type Store struct {
	db *sql.DB
}

// Open initializes (or creates) the SQLite database at dbPath and applies schema migrations.
func Open(dbPath string) (*Store, error) {
	if dbPath != ":memory:" {
		if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create db dir %q: %w", dir, err)
			}
		}
	}

	dsn := dbPath
	if dbPath != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", dbPath)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}

	// SQLite works best with serialized writers; modernc handles concurrent readers well in WAL mode.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying SQLite connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS stations (
	source        TEXT NOT NULL,
	id            TEXT NOT NULL,
	name          TEXT NOT NULL,
	lat           REAL NOT NULL,
	lon           REAL NOT NULL,
	elevation     REAL,
	teryt         TEXT NOT NULL DEFAULT '',
	voivodeship   TEXT NOT NULL DEFAULT '',
	river         TEXT NOT NULL DEFAULT '',
	warning_level REAL,
	alarm_level   REAL,
	updated_at    INTEGER NOT NULL,
	PRIMARY KEY (source, id)
);

CREATE TABLE IF NOT EXISTS meteo_observations (
	source           TEXT NOT NULL,
	station_id       TEXT NOT NULL,
	observed_at      INTEGER NOT NULL,
	temperature_c    REAL,
	ground_temp_c    REAL,
	humidity_pct     REAL,
	wind_speed_ms    REAL,
	wind_gust_ms     REAL,
	wind_dir_deg     REAL,
	precipitation_mm REAL,
	pressure_hpa     REAL,
	fetched_at       INTEGER NOT NULL,
	PRIMARY KEY (source, station_id, observed_at)
);
CREATE INDEX IF NOT EXISTS idx_meteo_obs_station_time ON meteo_observations (source, station_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_meteo_obs_time ON meteo_observations (observed_at);

CREATE TABLE IF NOT EXISTS hydro_observations (
	station_id           TEXT NOT NULL,
	observed_at          INTEGER NOT NULL,
	water_level_cm       REAL,
	water_level_at       INTEGER,
	water_temp_c         REAL,
	water_temp_at        INTEGER,
	water_flow_m3s       REAL,
	water_flow_at        INTEGER,
	ice_phenomena        TEXT,
	ice_at               INTEGER,
	vegetation_phenomena TEXT,
	vegetation_at        INTEGER,
	fetched_at           INTEGER NOT NULL,
	PRIMARY KEY (station_id, observed_at)
);
CREATE INDEX IF NOT EXISTS idx_hydro_obs_station_time ON hydro_observations (station_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_hydro_obs_time ON hydro_observations (observed_at);

CREATE TABLE IF NOT EXISTS warnings (
	type         TEXT NOT NULL,
	id           TEXT NOT NULL,
	event        TEXT NOT NULL,
	level        TEXT NOT NULL,
	probability  INTEGER,
	valid_from   INTEGER,
	valid_to     INTEGER,
	published_at INTEGER,
	content      TEXT NOT NULL DEFAULT '',
	comment      TEXT NOT NULL DEFAULT '',
	office       TEXT NOT NULL DEFAULT '',
	teryts_json  TEXT NOT NULL DEFAULT '[]',
	areas_json   TEXT NOT NULL DEFAULT '[]',
	fetched_at   INTEGER NOT NULL,
	PRIMARY KEY (type, id)
);

CREATE TABLE IF NOT EXISTS forecasts (
	location_key TEXT PRIMARY KEY,
	lat          REAL NOT NULL,
	lon          REAL NOT NULL,
	station_id   TEXT NOT NULL DEFAULT '',
	name         TEXT NOT NULL DEFAULT '',
	elevation    REAL,
	model        TEXT NOT NULL DEFAULT '',
	fetched_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	payload_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_forecasts_fetched ON forecasts (fetched_at);

CREATE TABLE IF NOT EXISTS sync_status (
	source          TEXT PRIMARY KEY,
	last_success_at INTEGER,
	last_attempt_at INTEGER,
	last_error      TEXT NOT NULL DEFAULT '',
	record_count    INTEGER NOT NULL DEFAULT 0
);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply sqlite schema: %w", err)
	}
	return nil
}

// UpsertStations inserts or updates a batch of stations inside a single transaction.
func (s *Store) UpsertStations(ctx context.Context, stations []Station) error {
	if len(stations) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO stations (source, id, name, lat, lon, elevation, teryt, voivodeship, river, warning_level, alarm_level, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source, id) DO UPDATE SET
	name=excluded.name,
	lat=excluded.lat,
	lon=excluded.lon,
	elevation=COALESCE(excluded.elevation, stations.elevation),
	teryt=CASE WHEN excluded.teryt != '' THEN excluded.teryt ELSE stations.teryt END,
	voivodeship=CASE WHEN excluded.voivodeship != '' THEN excluded.voivodeship ELSE stations.voivodeship END,
	river=CASE WHEN excluded.river != '' THEN excluded.river ELSE stations.river END,
	warning_level=COALESCE(excluded.warning_level, stations.warning_level),
	alarm_level=COALESCE(excluded.alarm_level, stations.alarm_level),
	updated_at=excluded.updated_at
`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, st := range stations {
		updated := st.UpdatedAt
		if updated.IsZero() {
			updated = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx,
			st.Source, st.ID, st.Name, st.Lat, st.Lon,
			nullableFloat(st.Elevation), st.TERYT, st.Voivodeship, st.River,
			nullableFloat(st.WarningLevel), nullableFloat(st.AlarmLevel),
			updated.Unix(),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListStations returns all cached stations, optionally filtered by source ("synop", "meteo", "hydro").
func (s *Store) ListStations(ctx context.Context, source string) ([]Station, error) {
	query := `SELECT source, id, name, lat, lon, elevation, teryt, voivodeship, river, warning_level, alarm_level, updated_at FROM stations`
	var args []any
	if source != "" {
		query += ` WHERE source = ?`
		args = append(args, source)
	}
	query += ` ORDER BY source, id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Station
	for rows.Next() {
		var st Station
		var elev, warn, alarm sql.NullFloat64
		var updatedUnix int64
		if err := rows.Scan(
			&st.Source, &st.ID, &st.Name, &st.Lat, &st.Lon,
			&elev, &st.TERYT, &st.Voivodeship, &st.River,
			&warn, &alarm, &updatedUnix,
		); err != nil {
			return nil, err
		}
		st.Elevation = fromNullFloat(elev)
		st.WarningLevel = fromNullFloat(warn)
		st.AlarmLevel = fromNullFloat(alarm)
		st.UpdatedAt = time.Unix(updatedUnix, 0).UTC()
		out = append(out, st)
	}
	return out, rows.Err()
}

// UpsertMeteoObservations inserts/updates weather observations, deduplicated by (source, station_id, observed_at).
func (s *Store) UpsertMeteoObservations(ctx context.Context, obs []MeteoObservation) error {
	if len(obs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO meteo_observations (
	source, station_id, observed_at, temperature_c, ground_temp_c, humidity_pct,
	wind_speed_ms, wind_gust_ms, wind_dir_deg, precipitation_mm, pressure_hpa, fetched_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source, station_id, observed_at) DO UPDATE SET
	temperature_c=COALESCE(excluded.temperature_c, meteo_observations.temperature_c),
	ground_temp_c=COALESCE(excluded.ground_temp_c, meteo_observations.ground_temp_c),
	humidity_pct=COALESCE(excluded.humidity_pct, meteo_observations.humidity_pct),
	wind_speed_ms=COALESCE(excluded.wind_speed_ms, meteo_observations.wind_speed_ms),
	wind_gust_ms=COALESCE(excluded.wind_gust_ms, meteo_observations.wind_gust_ms),
	wind_dir_deg=COALESCE(excluded.wind_dir_deg, meteo_observations.wind_dir_deg),
	precipitation_mm=COALESCE(excluded.precipitation_mm, meteo_observations.precipitation_mm),
	pressure_hpa=COALESCE(excluded.pressure_hpa, meteo_observations.pressure_hpa),
	fetched_at=excluded.fetched_at
`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, o := range obs {
		fetched := o.FetchedAt
		if fetched.IsZero() {
			fetched = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx,
			o.Source, o.StationID, o.ObservedAt.UTC().Unix(),
			nullableFloat(o.TemperatureC), nullableFloat(o.GroundTempC), nullableFloat(o.HumidityPct),
			nullableFloat(o.WindSpeedMS), nullableFloat(o.WindGustMS), nullableFloat(o.WindDirDeg),
			nullableFloat(o.PrecipitationMM), nullableFloat(o.PressureHPa),
			fetched.UTC().Unix(),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LatestMeteoObservation returns the most recent observation for a given source and station.
func (s *Store) LatestMeteoObservation(ctx context.Context, source, stationID string) (*MeteoObservation, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT source, station_id, observed_at, temperature_c, ground_temp_c, humidity_pct,
       wind_speed_ms, wind_gust_ms, wind_dir_deg, precipitation_mm, pressure_hpa, fetched_at
FROM meteo_observations
WHERE source = ? AND station_id = ?
ORDER BY observed_at DESC
LIMIT 1
`, source, stationID)

	o, err := scanMeteoObservation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ListMeteoObservations returns observations for (source, stationID) within [from, to] ascending.
func (s *Store) ListMeteoObservations(ctx context.Context, source, stationID string, from, to time.Time) ([]MeteoObservation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT source, station_id, observed_at, temperature_c, ground_temp_c, humidity_pct,
       wind_speed_ms, wind_gust_ms, wind_dir_deg, precipitation_mm, pressure_hpa, fetched_at
FROM meteo_observations
WHERE source = ? AND station_id = ? AND observed_at >= ? AND observed_at <= ?
ORDER BY observed_at ASC
`, source, stationID, from.UTC().Unix(), to.UTC().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MeteoObservation
	for rows.Next() {
		o, err := scanMeteoObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UpsertHydroObservations inserts/updates hydrological measurements deduplicated by (station_id, observed_at).
func (s *Store) UpsertHydroObservations(ctx context.Context, obs []HydroObservation) error {
	if len(obs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO hydro_observations (
	station_id, observed_at, water_level_cm, water_level_at,
	water_temp_c, water_temp_at, water_flow_m3s, water_flow_at,
	ice_phenomena, ice_at, vegetation_phenomena, vegetation_at, fetched_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(station_id, observed_at) DO UPDATE SET
	water_level_cm=COALESCE(excluded.water_level_cm, hydro_observations.water_level_cm),
	water_level_at=COALESCE(excluded.water_level_at, hydro_observations.water_level_at),
	water_temp_c=COALESCE(excluded.water_temp_c, hydro_observations.water_temp_c),
	water_temp_at=COALESCE(excluded.water_temp_at, hydro_observations.water_temp_at),
	water_flow_m3s=COALESCE(excluded.water_flow_m3s, hydro_observations.water_flow_m3s),
	water_flow_at=COALESCE(excluded.water_flow_at, hydro_observations.water_flow_at),
	ice_phenomena=COALESCE(excluded.ice_phenomena, hydro_observations.ice_phenomena),
	ice_at=COALESCE(excluded.ice_at, hydro_observations.ice_at),
	vegetation_phenomena=COALESCE(excluded.vegetation_phenomena, hydro_observations.vegetation_phenomena),
	vegetation_at=COALESCE(excluded.vegetation_at, hydro_observations.vegetation_at),
	fetched_at=excluded.fetched_at
`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, o := range obs {
		fetched := o.FetchedAt
		if fetched.IsZero() {
			fetched = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx,
			o.StationID, o.ObservedAt.UTC().Unix(),
			nullableFloat(o.WaterLevelCM), nullableTime(o.WaterLevelAt),
			nullableFloat(o.WaterTempC), nullableTime(o.WaterTempAt),
			nullableFloat(o.WaterFlowM3S), nullableTime(o.WaterFlowAt),
			nullableString(o.IcePhenomena), nullableTime(o.IceAt),
			nullableString(o.VegetationPhenomena), nullableTime(o.VegetationAt),
			fetched.UTC().Unix(),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LatestHydroObservation returns the latest hydrological reading for a station.
func (s *Store) LatestHydroObservation(ctx context.Context, stationID string) (*HydroObservation, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT station_id, observed_at, water_level_cm, water_level_at,
       water_temp_c, water_temp_at, water_flow_m3s, water_flow_at,
       ice_phenomena, ice_at, vegetation_phenomena, vegetation_at, fetched_at
FROM hydro_observations
WHERE station_id = ?
ORDER BY observed_at DESC
LIMIT 1
`, stationID)

	o, err := scanHydroObservation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ReplaceWarnings atomically replaces all warnings of a given type ("meteo" or "hydro").
func (s *Store) ReplaceWarnings(ctx context.Context, warningType string, warnings []Warning) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM warnings WHERE type = ?`, warningType); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO warnings (
	type, id, event, level, probability, valid_from, valid_to, published_at,
	content, comment, office, teryts_json, areas_json, fetched_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, w := range warnings {
		terytsBytes, _ := json.Marshal(w.TERYTs)
		if w.TERYTs == nil {
			terytsBytes = []byte("[]")
		}
		areasBytes, _ := json.Marshal(w.Areas)
		if w.Areas == nil {
			areasBytes = []byte("[]")
		}
		fetched := w.FetchedAt
		if fetched.IsZero() {
			fetched = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx,
			warningType, w.ID, w.Event, w.Level, nullableInt(w.Probability),
			nullableTime(w.ValidFrom), nullableTime(w.ValidTo), nullableTime(w.PublishedAt),
			w.Content, w.Comment, w.Office, string(terytsBytes), string(areasBytes),
			fetched.UTC().Unix(),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListWarnings returns cached warnings, optionally filtered by type ("meteo", "hydro", or ""/"all").
func (s *Store) ListWarnings(ctx context.Context, warningType string) ([]Warning, error) {
	query := `
SELECT type, id, event, level, probability, valid_from, valid_to, published_at,
       content, comment, office, teryts_json, areas_json, fetched_at
FROM warnings`
	var args []any
	if warningType != "" && warningType != "all" {
		query += ` WHERE type = ?`
		args = append(args, warningType)
	}
	query += ` ORDER BY type, valid_from DESC, id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Warning
	for rows.Next() {
		var w Warning
		var prob, validFrom, validTo, pubAt sql.NullInt64
		var terytsJSON, areasJSON string
		var fetchedUnix int64
		if err := rows.Scan(
			&w.Type, &w.ID, &w.Event, &w.Level, &prob,
			&validFrom, &validTo, &pubAt,
			&w.Content, &w.Comment, &w.Office, &terytsJSON, &areasJSON, &fetchedUnix,
		); err != nil {
			return nil, err
		}
		w.Probability = fromNullInt(prob)
		w.ValidFrom = fromNullTime(validFrom)
		w.ValidTo = fromNullTime(validTo)
		w.PublishedAt = fromNullTime(pubAt)
		w.FetchedAt = time.Unix(fetchedUnix, 0).UTC()
		_ = json.Unmarshal([]byte(terytsJSON), &w.TERYTs)
		_ = json.Unmarshal([]byte(areasJSON), &w.Areas)
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpsertForecast stores or replaces a cached forecast bundle for a location key.
func (s *Store) UpsertForecast(ctx context.Context, fc CachedForecast) error {
	payload, err := json.Marshal(fc.Hourly)
	if err != nil {
		return fmt.Errorf("marshal forecast hourly: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO forecasts (location_key, lat, lon, station_id, name, elevation, model, fetched_at, expires_at, payload_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(location_key) DO UPDATE SET
	lat=excluded.lat,
	lon=excluded.lon,
	station_id=excluded.station_id,
	name=excluded.name,
	elevation=COALESCE(excluded.elevation, forecasts.elevation),
	model=excluded.model,
	fetched_at=excluded.fetched_at,
	expires_at=excluded.expires_at,
	payload_json=excluded.payload_json
`, fc.LocationKey, fc.Lat, fc.Lon, fc.StationID, fc.Name,
		nullableFloat(fc.Elevation), fc.Model,
		fc.FetchedAt.UTC().Unix(), fc.ExpiresAt.UTC().Unix(), string(payload),
	)
	return err
}

// GetForecast retrieves a cached forecast by exact location_key.
func (s *Store) GetForecast(ctx context.Context, locationKey string) (*CachedForecast, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT location_key, lat, lon, station_id, name, elevation, model, fetched_at, expires_at, payload_json
FROM forecasts
WHERE location_key = ?
`, locationKey)

	fc, err := scanForecast(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &fc, nil
}

// ListForecastMeta returns lightweight metadata for all cached forecasts without
// deserializing their hourly JSON payloads.
func (s *Store) ListForecastMeta(ctx context.Context) ([]ForecastMeta, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT location_key, lat, lon, station_id, name, elevation, model, fetched_at, expires_at
FROM forecasts
ORDER BY location_key
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ForecastMeta
	for rows.Next() {
		var m ForecastMeta
		var elev sql.NullFloat64
		var fetchedUnix, expiresUnix int64
		if err := rows.Scan(
			&m.LocationKey, &m.Lat, &m.Lon, &m.StationID, &m.Name,
			&elev, &m.Model, &fetchedUnix, &expiresUnix,
		); err != nil {
			return nil, err
		}
		m.Elevation = fromNullFloat(elev)
		m.FetchedAt = time.Unix(fetchedUnix, 0).UTC()
		m.ExpiresAt = time.Unix(expiresUnix, 0).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListForecasts returns all cached forecasts (used for nearest-cached lookup & health metrics).
func (s *Store) ListForecasts(ctx context.Context) ([]CachedForecast, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT location_key, lat, lon, station_id, name, elevation, model, fetched_at, expires_at, payload_json
FROM forecasts
ORDER BY location_key
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CachedForecast
	for rows.Next() {
		fc, err := scanForecast(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, fc)
	}
	return out, rows.Err()
}

// RecordSyncAttempt updates sync_status for a given upstream source.
func (s *Store) RecordSyncAttempt(ctx context.Context, source string, recordCount int, syncErr error) error {
	now := time.Now().UTC().Unix()
	if syncErr == nil {
		_, err := s.db.ExecContext(ctx, `
INSERT INTO sync_status (source, last_success_at, last_attempt_at, last_error, record_count)
VALUES (?, ?, ?, '', ?)
ON CONFLICT(source) DO UPDATE SET
	last_success_at=excluded.last_success_at,
	last_attempt_at=excluded.last_attempt_at,
	last_error='',
	record_count=excluded.record_count
`, source, now, now, recordCount)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sync_status (source, last_success_at, last_attempt_at, last_error, record_count)
VALUES (?, NULL, ?, ?, 0)
ON CONFLICT(source) DO UPDATE SET
	last_attempt_at=excluded.last_attempt_at,
	last_error=excluded.last_error
`, source, now, syncErr.Error())
	return err
}

// ListSyncStatuses returns health status for all recorded upstream pollers.
func (s *Store) ListSyncStatuses(ctx context.Context) ([]SyncStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT source, last_success_at, last_attempt_at, last_error, record_count
FROM sync_status
ORDER BY source
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SyncStatus
	for rows.Next() {
		var st SyncStatus
		var lastSuccess, lastAttempt sql.NullInt64
		if err := rows.Scan(&st.Source, &lastSuccess, &lastAttempt, &st.LastError, &st.RecordCount); err != nil {
			return nil, err
		}
		st.LastSuccessAt = fromNullTime(lastSuccess)
		st.LastAttemptAt = fromNullTime(lastAttempt)
		out = append(out, st)
	}
	return out, rows.Err()
}

// Stats returns row counts across cached tables for /healthz observability.
func (s *Store) Stats(ctx context.Context) (map[string]int64, error) {
	tables := []string{"stations", "meteo_observations", "hydro_observations", "warnings", "forecasts"}
	res := make(map[string]int64, len(tables))
	for _, tbl := range tables {
		var count int64
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&count); err != nil {
			return nil, err
		}
		res[tbl] = count
	}
	return res, nil
}

// PruneOlderThan removes observations and stale forecasts older than cutoff,
// plus expired ad-hoc grid forecasts older than 24h past expiry.
func (s *Store) PruneOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	cut := cutoff.UTC().Unix()
	gridExpiryCutoff := time.Now().UTC().Add(-24 * time.Hour).Unix()
	var total int64

	queries := []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM meteo_observations WHERE observed_at < ?`, cut},
		{`DELETE FROM hydro_observations WHERE observed_at < ?`, cut},
		{`DELETE FROM forecasts WHERE fetched_at < ?`, cut},
		{`DELETE FROM forecasts WHERE location_key LIKE 'grid:%' AND expires_at < ?`, gridExpiryCutoff},
	}
	for _, q := range queries {
		res, err := s.db.ExecContext(ctx, q.sql, q.arg)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMeteoObservation(sc scanner) (MeteoObservation, error) {
	var o MeteoObservation
	var obsUnix, fetchedUnix int64
	var temp, gtemp, hum, ws, wg, wd, precip, press sql.NullFloat64
	err := sc.Scan(
		&o.Source, &o.StationID, &obsUnix,
		&temp, &gtemp, &hum, &ws, &wg, &wd, &precip, &press, &fetchedUnix,
	)
	if err != nil {
		return o, err
	}
	o.ObservedAt = time.Unix(obsUnix, 0).UTC()
	o.FetchedAt = time.Unix(fetchedUnix, 0).UTC()
	o.TemperatureC = fromNullFloat(temp)
	o.GroundTempC = fromNullFloat(gtemp)
	o.HumidityPct = fromNullFloat(hum)
	o.WindSpeedMS = fromNullFloat(ws)
	o.WindGustMS = fromNullFloat(wg)
	o.WindDirDeg = fromNullFloat(wd)
	o.PrecipitationMM = fromNullFloat(precip)
	o.PressureHPa = fromNullFloat(press)
	return o, nil
}

func scanHydroObservation(sc scanner) (HydroObservation, error) {
	var o HydroObservation
	var obsUnix, fetchedUnix int64
	var wl, wt, wf sql.NullFloat64
	var wlat, wtat, wfat, iceAt, vegAt sql.NullInt64
	var ice, veg sql.NullString
	err := sc.Scan(
		&o.StationID, &obsUnix,
		&wl, &wlat, &wt, &wtat, &wf, &wfat,
		&ice, &iceAt, &veg, &vegAt, &fetchedUnix,
	)
	if err != nil {
		return o, err
	}
	o.ObservedAt = time.Unix(obsUnix, 0).UTC()
	o.FetchedAt = time.Unix(fetchedUnix, 0).UTC()
	o.WaterLevelCM = fromNullFloat(wl)
	o.WaterLevelAt = fromNullTime(wlat)
	o.WaterTempC = fromNullFloat(wt)
	o.WaterTempAt = fromNullTime(wtat)
	o.WaterFlowM3S = fromNullFloat(wf)
	o.WaterFlowAt = fromNullTime(wfat)
	o.IcePhenomena = fromNullString(ice)
	o.IceAt = fromNullTime(iceAt)
	o.VegetationPhenomena = fromNullString(veg)
	o.VegetationAt = fromNullTime(vegAt)
	return o, nil
}

func scanForecast(sc scanner) (CachedForecast, error) {
	var fc CachedForecast
	var elev sql.NullFloat64
	var fetchedUnix, expiresUnix int64
	var payloadJSON string
	err := sc.Scan(
		&fc.LocationKey, &fc.Lat, &fc.Lon, &fc.StationID, &fc.Name,
		&elev, &fc.Model, &fetchedUnix, &expiresUnix, &payloadJSON,
	)
	if err != nil {
		return fc, err
	}
	fc.Elevation = fromNullFloat(elev)
	fc.FetchedAt = time.Unix(fetchedUnix, 0).UTC()
	fc.ExpiresAt = time.Unix(expiresUnix, 0).UTC()
	if err := json.Unmarshal([]byte(payloadJSON), &fc.Hourly); err != nil {
		return fc, fmt.Errorf("unmarshal forecast payload for %s: %w", fc.LocationKey, err)
	}
	return fc, nil
}

func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Unix()
}

func fromNullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	x := v.Float64
	return &x
}

func fromNullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	x := int(v.Int64)
	return &x
}

func fromNullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	x := v.String
	return &x
}

func fromNullTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}
