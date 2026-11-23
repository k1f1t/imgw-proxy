package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"imgw-openmeteo-proxy/internal/config"
	"imgw-openmeteo-proxy/internal/geo"
	"imgw-openmeteo-proxy/internal/imgw"
	"imgw-openmeteo-proxy/internal/store"
)

// ForecastTarget is a resolved geographic point whose forecast should be polled.
type ForecastTarget struct {
	LocationKey string
	StationID   string
	Name        string
	Lat         float64
	Lon         float64
	Elevation   *float64
}

// Scheduler coordinates periodic IMGW data polling, retention pruning, and cached forecast lookups.
type Scheduler struct {
	cfg     config.Config
	client  *imgw.Client
	store   *store.Store
	logger  *slog.Logger
	targets []ForecastTarget

	inflightMu sync.Mutex
	inflight   map[string]*inflightCall
}

type inflightCall struct {
	done chan struct{}
	fc   *store.CachedForecast
	err  error
}

// New creates a Scheduler and resolves configured forecast targets for "all" or "selected" mode.
func New(cfg config.Config, client *imgw.Client, st *store.Store, logger *slog.Logger) (*Scheduler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	targets, err := resolveForecastTargets(cfg)
	if err != nil {
		return nil, err
	}
	return &Scheduler{
		cfg:      cfg,
		client:   client,
		store:    st,
		logger:   logger,
		targets:  targets,
		inflight: make(map[string]*inflightCall),
	}, nil
}

// Targets returns the resolved list of scheduled forecast targets.
func (s *Scheduler) Targets() []ForecastTarget {
	out := make([]ForecastTarget, len(s.targets))
	copy(out, s.targets)
	return out
}

// Start launches background polling loops and runs an immediate initial sync.
func (s *Scheduler) Start(ctx context.Context) {
	// Seed embedded synoptic stations into SQLite right away so /v1/stations & nearest lookups work immediately.
	s.seedSynopCatalog(ctx)

	// Run initial batch fetches concurrently at startup.
	go s.runPoller(ctx, "synop", s.cfg.SynopPollInterval, s.SyncSynop)
	go s.runPoller(ctx, "meteo", s.cfg.MeteoPollInterval, s.SyncMeteo)
	go s.runPoller(ctx, "hydro", s.cfg.HydroPollInterval, s.SyncHydro)
	go s.runPoller(ctx, "warnings", s.cfg.WarningsPollInterval, s.SyncWarnings)
	go s.runPoller(ctx, "forecasts", s.cfg.ForecastPollInterval, s.SyncForecasts)
	go s.runPoller(ctx, "prune", 12*time.Hour, s.Prune)
}

func (s *Scheduler) seedSynopCatalog(ctx context.Context) {
	list, _, err := imgw.SynopCatalog()
	if err != nil {
		s.logger.Warn("failed to load embedded synop catalog", "err", err)
		return
	}
	now := time.Now().UTC()
	stations := make([]store.Station, 0, len(list))
	for _, st := range list {
		elev := st.Elevation
		stations = append(stations, store.Station{
			Source:      "synop",
			ID:          st.ID,
			Name:        st.Name,
			Lat:         st.Lat,
			Lon:         st.Lon,
			Elevation:   &elev,
			TERYT:       st.TERYT,
			Voivodeship: geo.VoivodeshipFromTERYT(st.TERYT),
			UpdatedAt:   now,
		})
	}
	if err := s.store.UpsertStations(ctx, stations); err != nil {
		s.logger.Warn("failed to seed synop catalog", "err", err)
	}
}

func (s *Scheduler) runPoller(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) {
	if err := fn(ctx); err != nil && ctx.Err() == nil {
		s.logger.Warn("initial sync failed; will serve cached data and retry", "source", name, "err", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				s.logger.Warn("scheduled sync failed; keeping cached data", "source", name, "err", err)
			}
		}
	}
}

// SyncSynop polls danepubliczne.imgw.pl/api/data/synop and stores stations + observations.
func (s *Scheduler) SyncSynop(ctx context.Context) error {
	stations, obs, err := s.client.FetchSynop(ctx)
	if err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "synop", 0, err)
		return err
	}
	if err := s.store.UpsertStations(ctx, stations); err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "synop", 0, err)
		return err
	}
	if err := s.store.UpsertMeteoObservations(ctx, obs); err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "synop", 0, err)
		return err
	}
	_ = s.store.RecordSyncAttempt(ctx, "synop", len(obs), nil)
	s.logger.Info("synced IMGW synop", "stations", len(stations), "observations", len(obs))
	return nil
}

// SyncMeteo polls danepubliczne.imgw.pl/api/data/meteo and stores stations + 10-min observations.
func (s *Scheduler) SyncMeteo(ctx context.Context) error {
	stations, obs, err := s.client.FetchMeteo(ctx)
	if err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "meteo", 0, err)
		return err
	}
	if err := s.store.UpsertStations(ctx, stations); err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "meteo", 0, err)
		return err
	}
	if err := s.store.UpsertMeteoObservations(ctx, obs); err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "meteo", 0, err)
		return err
	}
	_ = s.store.RecordSyncAttempt(ctx, "meteo", len(obs), nil)
	s.logger.Info("synced IMGW meteo", "stations", len(stations), "observations", len(obs))
	return nil
}

// SyncHydro polls danepubliczne.imgw.pl/api/data/hydro and stores stations + hydrological observations.
func (s *Scheduler) SyncHydro(ctx context.Context) error {
	stations, obs, err := s.client.FetchHydro(ctx)
	if err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "hydro", 0, err)
		return err
	}
	if err := s.store.UpsertStations(ctx, stations); err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "hydro", 0, err)
		return err
	}
	if err := s.store.UpsertHydroObservations(ctx, obs); err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "hydro", 0, err)
		return err
	}
	_ = s.store.RecordSyncAttempt(ctx, "hydro", len(obs), nil)
	s.logger.Info("synced IMGW hydro", "stations", len(stations), "observations", len(obs))
	return nil
}

// SyncWarnings polls warningsmeteo and warningshydro and refreshes SQLite.
func (s *Scheduler) SyncWarnings(ctx context.Context) error {
	var firstErr error

	meteoWarnings, err := s.client.FetchWarningsMeteo(ctx)
	if err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "warnings_meteo", 0, err)
		firstErr = err
	} else {
		if err := s.store.ReplaceWarnings(ctx, "meteo", meteoWarnings); err != nil {
			_ = s.store.RecordSyncAttempt(ctx, "warnings_meteo", 0, err)
			firstErr = err
		} else {
			_ = s.store.RecordSyncAttempt(ctx, "warnings_meteo", len(meteoWarnings), nil)
			s.logger.Info("synced IMGW meteo warnings", "count", len(meteoWarnings))
		}
	}

	hydroWarnings, err := s.client.FetchWarningsHydro(ctx)
	if err != nil {
		_ = s.store.RecordSyncAttempt(ctx, "warnings_hydro", 0, err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		if err := s.store.ReplaceWarnings(ctx, "hydro", hydroWarnings); err != nil {
			_ = s.store.RecordSyncAttempt(ctx, "warnings_hydro", 0, err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			_ = s.store.RecordSyncAttempt(ctx, "warnings_hydro", len(hydroWarnings), nil)
			s.logger.Info("synced IMGW hydro warnings", "count", len(hydroWarnings))
		}
	}

	return firstErr
}

// SyncForecasts fetches meteo.imgw.pl forecasts for all configured targets (respecting mode & polite pacing).
func (s *Scheduler) SyncForecasts(ctx context.Context) error {
	if len(s.targets) == 0 {
		s.logger.Info("no forecast targets configured; skipping scheduled forecast sync", "mode", s.cfg.ForecastMode)
		return nil
	}

	succeeded := 0
	var lastErr error

	for i, t := range s.targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Skip target if we already have a fresh cached forecast (e.g. across quick restarts).
		if existing, err := s.store.GetForecast(ctx, t.LocationKey); err == nil && existing != nil && time.Now().UTC().Before(existing.ExpiresAt) {
			succeeded++
			continue
		}

		fc, err := s.client.FetchForecast(ctx, t.Lat, t.Lon)
		if err != nil {
			lastErr = err
			s.logger.Warn("forecast fetch failed for target", "target", t.Name, "key", t.LocationKey, "err", err)
		} else {
			fc.LocationKey = t.LocationKey
			fc.StationID = t.StationID
			fc.Name = t.Name
			fc.Lat = t.Lat
			fc.Lon = t.Lon
			fc.Elevation = t.Elevation
			fc.ExpiresAt = fc.FetchedAt.Add(s.cfg.ForecastTTL)
			if err := s.store.UpsertForecast(ctx, *fc); err != nil {
				lastErr = err
				s.logger.Warn("failed to store forecast", "target", t.Name, "err", err)
			} else {
				succeeded++
			}
		}

		if i < len(s.targets)-1 && s.cfg.ForecastRequestDelay > 0 {
			timer := time.NewTimer(s.cfg.ForecastRequestDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}

	if succeeded == 0 && lastErr != nil {
		_ = s.store.RecordSyncAttempt(ctx, "forecasts", 0, lastErr)
		return lastErr
	}
	_ = s.store.RecordSyncAttempt(ctx, "forecasts", succeeded, nil)
	s.logger.Info("synced IMGW forecasts", "mode", s.cfg.ForecastMode, "targets", len(s.targets), "succeeded", succeeded)
	return nil
}

// Prune removes historical observations and stale forecasts older than RetentionDays.
func (s *Scheduler) Prune(ctx context.Context) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -s.cfg.RetentionDays)
	deleted, err := s.store.PruneOlderThan(ctx, cutoff)
	if err != nil {
		return err
	}
	if deleted > 0 {
		s.logger.Info("pruned expired cache rows", "deleted", deleted, "cutoff", cutoff.Format(time.RFC3339))
	}
	return nil
}

const maxStaleFallbackDistanceKm = 100.0

// GetForecastForCoords implements openmeteo.ForecastProvider.
// Behavior by ForecastMode:
//   - "selected": returns the nearest pre-fetched/configured location's cached forecast (zero IMGW traffic on requests
//     unless ForecastAllowOnDemand is explicitly set to true).
//   - "all": snaps to a 0.05° spatial grid (~5km) or nearest cached station within 8km; returns cached forecast if fresh,
//     otherwise fetches on-demand (singleflight-deduplicated) with bounded stale-while-error fallback.
func (s *Scheduler) GetForecastForCoords(ctx context.Context, lat, lon float64) (*store.CachedForecast, error) {
	now := time.Now().UTC()

	// 1. Scan lightweight forecast metadata (without deserializing hourly JSON payloads).
	metas, err := s.store.ListForecastMeta(ctx)
	if err != nil {
		return nil, err
	}

	var nearestMeta *store.ForecastMeta
	nearestDist := math.MaxFloat64
	for i := range metas {
		d := geo.HaversineKm(lat, lon, metas[i].Lat, metas[i].Lon)
		if d < nearestDist {
			nearestDist = d
			m := metas[i]
			nearestMeta = &m
		}
	}

	// In "selected" mode with on-demand disabled, always serve the nearest configured cached forecast
	// (no upstream call triggered by client traffic).
	if !s.cfg.AllowOnDemandForecasts() {
		if nearestMeta == nil {
			return nil, nil
		}
		return s.store.GetForecast(ctx, nearestMeta.LocationKey)
	}

	// If a cached forecast is within 8 km and still fresh, load and serve it directly.
	if nearestMeta != nil && nearestDist <= 8.0 && now.Before(nearestMeta.ExpiresAt) {
		return s.store.GetForecast(ctx, nearestMeta.LocationKey)
	}

	// Snap arbitrary coordinates to a ~0.05° grid (~5km) to maximize cache hits across nearby requests.
	snapLat := math.Round(lat*20.0) / 20.0
	snapLon := math.Round(lon*20.0) / 20.0
	locKey := fmt.Sprintf("grid:%.2f,%.2f", snapLat, snapLon)

	existing, _ := s.store.GetForecast(ctx, locKey)
	if existing != nil && now.Before(existing.ExpiresAt) {
		return existing, nil
	}

	// Deduplicate concurrent on-demand fetches for the same grid key.
	fc, fetchErr := s.fetchSingleflight(ctx, locKey, snapLat, snapLon)
	if fetchErr == nil && fc != nil {
		return fc, nil
	}

	// Stale-while-error fallback: prefer stale entry for the exact grid key,
	// otherwise fall back to nearest cached station only if within maxStaleFallbackDistanceKm.
	if existing != nil {
		s.logger.Warn("on-demand forecast fetch failed; serving stale grid cache", "key", locKey, "err", fetchErr)
		return existing, nil
	}
	if nearestMeta != nil && nearestDist <= maxStaleFallbackDistanceKm {
		if nearest, err := s.store.GetForecast(ctx, nearestMeta.LocationKey); err == nil && nearest != nil {
			s.logger.Warn("on-demand forecast fetch failed; serving nearest stale cache", "key", locKey, "fallback", nearestMeta.LocationKey, "distance_km", math.Round(nearestDist*10)/10, "err", fetchErr)
			return nearest, nil
		}
	}
	return nil, fetchErr
}

func (s *Scheduler) fetchSingleflight(ctx context.Context, locKey string, lat, lon float64) (*store.CachedForecast, error) {
	s.inflightMu.Lock()
	if call, ok := s.inflight[locKey]; ok {
		s.inflightMu.Unlock()
		select {
		case <-call.done:
			return call.fc, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	s.inflight[locKey] = call
	s.inflightMu.Unlock()

	defer func() {
		s.inflightMu.Lock()
		delete(s.inflight, locKey)
		s.inflightMu.Unlock()
		close(call.done)
	}()

	fetchCtx, cancel := context.WithTimeout(context.Background(), s.cfg.UpstreamTimeout*2)
	defer cancel()

	fc, err := s.client.FetchForecast(fetchCtx, lat, lon)
	if err != nil {
		call.err = err
		return nil, err
	}
	fc.LocationKey = locKey
	fc.Name = locKey
	fc.Lat = lat
	fc.Lon = lon
	fc.ExpiresAt = fc.FetchedAt.Add(s.cfg.ForecastTTL)
	if st, _, err := imgw.NearestSynopMeta(lat, lon); err == nil {
		elev := st.Elevation
		fc.Elevation = &elev
	}
	if err := s.store.UpsertForecast(fetchCtx, *fc); err != nil {
		s.logger.Warn("failed to persist on-demand forecast", "key", locKey, "err", err)
	}
	call.fc = fc
	return fc, nil
}

func resolveForecastTargets(cfg config.Config) ([]ForecastTarget, error) {
	catalog, byID, err := imgw.SynopCatalog()
	if err != nil {
		return nil, err
	}

	if cfg.ForecastMode == config.ForecastModeAll {
		targets := make([]ForecastTarget, 0, len(catalog))
		for _, st := range catalog {
			elev := st.Elevation
			targets = append(targets, ForecastTarget{
				LocationKey: "synop:" + st.ID,
				StationID:   st.ID,
				Name:        st.Name,
				Lat:         st.Lat,
				Lon:         st.Lon,
				Elevation:   &elev,
			})
		}
		return targets, nil
	}

	// Selected mode: resolve user-configured station IDs or explicit coordinates.
	targets := make([]ForecastTarget, 0, len(cfg.ForecastLocations))
	for _, loc := range cfg.ForecastLocations {
		if loc.StationID != "" {
			st, ok := byID[loc.StationID]
			if !ok {
				return nil, fmt.Errorf("unknown synoptic station_id %q in forecast_locations", loc.StationID)
			}
			elev := st.Elevation
			name := loc.Name
			if name == "" || name == loc.StationID {
				name = st.Name
			}
			targets = append(targets, ForecastTarget{
				LocationKey: "synop:" + st.ID,
				StationID:   st.ID,
				Name:        name,
				Lat:         st.Lat,
				Lon:         st.Lon,
				Elevation:   &elev,
			})
		} else {
			var elev *float64
			if st, _, err := imgw.NearestSynopMeta(loc.Lat, loc.Lon); err == nil {
				e := st.Elevation
				elev = &e
			}
			targets = append(targets, ForecastTarget{
				LocationKey: fmt.Sprintf("custom:%.4f,%.4f", loc.Lat, loc.Lon),
				Name:        loc.Name,
				Lat:         loc.Lat,
				Lon:         loc.Lon,
				Elevation:   elev,
			})
		}
	}
	return targets, nil
}
