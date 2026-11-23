package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"imgw-openmeteo-proxy/internal/config"
	"imgw-openmeteo-proxy/internal/geo"
	"imgw-openmeteo-proxy/internal/openmeteo"
	"imgw-openmeteo-proxy/internal/scheduler"
	"imgw-openmeteo-proxy/internal/store"
)

var (
	errMissingCoordPair = errors.New("both latitude and longitude must be provided together")
	errInvalidCoords    = errors.New("invalid latitude/longitude")
	errCoordsOutOfRange = errors.New("latitude/longitude out of range")
)

const (
	// AttributionSourcePL is the mandatory IMGW-PIB data source attribution (§ 5 ust. 3 Regulaminu).
	AttributionSourcePL = "Źródłem pochodzenia danych jest Instytut Meteorologii i Gospodarki Wodnej – Państwowy Instytut Badawczy"
	// AttributionProcessedPL is the mandatory IMGW-PIB processed data notice (§ 5 ust. 4 Regulaminu).
	AttributionProcessedPL = "Dane Instytutu Meteorologii i Gospodarki Wodnej – Państwowego Instytutu Badawczego zostały przetworzone"
)

// Server wires all HTTP endpoints and middleware.
type Server struct {
	cfg      config.Config
	store    *store.Store
	sched    *scheduler.Scheduler
	om       *openmeteo.Handler
	mux      *http.ServeMux
	nowFunc  func() time.Time
}

// NewServer creates a configured HTTP handler tree.
func NewServer(cfg config.Config, st *store.Store, sched *scheduler.Scheduler, provider openmeteo.ForecastProvider) *Server {
	s := &Server{
		cfg:     cfg,
		store:   st,
		sched:   sched,
		om:      openmeteo.NewHandler(st, provider),
		mux:     http.NewServeMux(),
		nowFunc: func() time.Time { return time.Now().UTC() },
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.Handle("GET /v1/forecast", s.withAuth(s.om))
	s.mux.Handle("GET /v1/hydro", s.withAuth(http.HandlerFunc(s.handleHydro)))
	s.mux.Handle("GET /v1/warnings", s.withAuth(http.HandlerFunc(s.handleWarnings)))
	s.mux.Handle("GET /v1/stations", s.withAuth(http.HandlerFunc(s.handleStations)))
	s.mux.Handle("GET /v1/attribution", s.withAuth(http.HandlerFunc(s.handleAttribution)))
}

// ServeHTTP dispatches HTTP requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.cfg.APIKey == "" {
		return next
	}
	expected := []byte(s.cfg.APIKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractAPIKey(r)
		if len(token) == 0 || subtle.ConstantTimeCompare([]byte(token), expected) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":  true,
				"reason": "unauthorized: valid API key required",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func extractAPIKey(r *http.Request) string {
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return strings.TrimSpace(auth[7:])
		}
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	if key := strings.TrimSpace(r.URL.Query().Get("apikey")); key != "" {
		return key
	}
	return ""
}

func (s *Server) handleAttribution(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"source":           AttributionSourcePL,
		"processed_notice": AttributionProcessedPL,
		"self_hosted_only": true,
		"upstream_terms":   "https://danepubliczne.imgw.pl/regulations",
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.store.ListSyncStatuses(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}
	targetCount := 0
	if s.sched != nil {
		targetCount = len(s.sched.Targets())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"forecast_mode":    s.cfg.ForecastMode,
		"forecast_targets": targetCount,
		"auth_enabled":     s.cfg.APIKey != "",
		"cache_counts":     stats,
		"sync_status":      statuses,
		"attribution":      AttributionSourcePL,
		"processed_notice": AttributionProcessedPL,
	})
}

type hydroEntry struct {
	Station      store.Station           `json:"station"`
	DistanceKm   *float64                `json:"distance_km,omitempty"`
	Observation  *store.HydroObservation `json:"observation,omitempty"`
	WarningState *bool                   `json:"flood_warning,omitempty"`
	AlarmState   *bool                   `json:"flood_alarm,omitempty"`
}

func (s *Server) handleHydro(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stationID := strings.TrimSpace(q.Get("station_id"))
	riverFilter := strings.ToLower(strings.TrimSpace(q.Get("river")))
	voivFilter := strings.ToLower(strings.TrimSpace(q.Get("voivodeship")))

	reqLat, reqLon, hasCoords, err := parseCoordPair(q.Get("latitude"), q.Get("longitude"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": err.Error()})
		return
	}

	limit := 50
	if hasCoords && stationID == "" && riverFilter == "" && voivFilter == "" {
		limit = 1
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": "limit must be between 1 and 1000"})
			return
		}
		limit = n
	}

	stations, err := s.store.ListStations(r.Context(), "hydro")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": true, "reason": "failed to query hydro stations"})
		return
	}

	entries := make([]hydroEntry, 0)
	for _, st := range stations {
		if stationID != "" && st.ID != stationID {
			continue
		}
		if riverFilter != "" && !strings.Contains(strings.ToLower(st.River), riverFilter) {
			continue
		}
		if voivFilter != "" && !strings.Contains(strings.ToLower(st.Voivodeship), voivFilter) {
			continue
		}
		e := hydroEntry{Station: st}
		if hasCoords {
			d := math.Round(geo.HaversineKm(reqLat, reqLon, st.Lat, st.Lon)*100) / 100
			e.DistanceKm = &d
		}
		entries = append(entries, e)
	}

	if hasCoords {
		sort.Slice(entries, func(i, j int) bool {
			return *entries[i].DistanceKm < *entries[j].DistanceKm
		})
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}

	for i := range entries {
		obs, err := s.store.LatestHydroObservation(r.Context(), entries[i].Station.ID)
		if err == nil && obs != nil {
			entries[i].Observation = obs
			if obs.WaterLevelCM != nil {
				if entries[i].Station.WarningLevel != nil {
					b := *obs.WaterLevelCM >= *entries[i].Station.WarningLevel
					entries[i].WarningState = &b
				}
				if entries[i].Station.AlarmLevel != nil {
					b := *obs.WaterLevelCM >= *entries[i].Station.AlarmLevel
					entries[i].AlarmState = &b
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"attribution":      AttributionSourcePL,
		"processed_notice": AttributionProcessedPL,
		"count":            len(entries),
		"stations":         entries,
	})
}

type warningEntry struct {
	store.Warning
	Status string `json:"status"` // "active", "upcoming", or "expired"
}

func (s *Server) handleWarnings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	wType := strings.ToLower(strings.TrimSpace(q.Get("type")))
	if wType == "" {
		wType = "all"
	}
	if wType != "all" && wType != "meteo" && wType != "hydro" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": "type must be all, meteo, or hydro"})
		return
	}

	teryt := strings.TrimSpace(q.Get("teryt"))
	voiv := strings.ToLower(strings.TrimSpace(q.Get("voivodeship")))
	activeOnly := true
	if v := strings.TrimSpace(q.Get("active_only")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": "invalid active_only boolean"})
			return
		}
		activeOnly = b
	}
	includeUpcoming := true
	if v := strings.TrimSpace(q.Get("include_upcoming")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": "invalid include_upcoming boolean"})
			return
		}
		includeUpcoming = b
	}

	var matchedStation *store.Station
	lat, lon, hasCoords, err := parseCoordPair(q.Get("latitude"), q.Get("longitude"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": err.Error()})
		return
	}
	if hasCoords {
		synops, err := s.store.ListStations(r.Context(), "synop")
		if err == nil && len(synops) > 0 {
			best := synops[0]
			bestDist := geo.HaversineKm(lat, lon, best.Lat, best.Lon)
			for _, st := range synops[1:] {
				if d := geo.HaversineKm(lat, lon, st.Lat, st.Lon); d < bestDist {
					bestDist = d
					best = st
				}
			}
			matchedStation = &best
			if teryt == "" && best.TERYT != "" {
				teryt = best.TERYT
			}
			if voiv == "" && best.Voivodeship != "" {
				voiv = strings.ToLower(best.Voivodeship)
			}
		}
	}

	if voiv == "" && len(teryt) >= 2 {
		voiv = strings.ToLower(geo.VoivodeshipFromTERYT(teryt))
	}

	all, err := s.store.ListWarnings(r.Context(), wType)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": true, "reason": "failed to query warnings"})
		return
	}

	now := s.nowFunc().UTC()
	filtered := make([]warningEntry, 0, len(all))
	for _, wItem := range all {
		status := "active"
		if wItem.ValidTo != nil && now.After(*wItem.ValidTo) {
			status = "expired"
		} else if wItem.ValidFrom != nil && now.Before(*wItem.ValidFrom) {
			status = "upcoming"
		}

		if activeOnly && status == "expired" {
			continue
		}
		if !includeUpcoming && status == "upcoming" {
			continue
		}
		if wItem.Type == "meteo" && teryt != "" {
			if !matchesTERYT(wItem.TERYTs, teryt) {
				continue
			}
		}
		if wItem.Type == "hydro" && voiv != "" {
			if !matchesVoivodeship(wItem.Areas, voiv) {
				continue
			}
		}
		filtered = append(filtered, warningEntry{Warning: wItem, Status: status})
	}

	resp := map[string]any{
		"attribution":      AttributionSourcePL,
		"processed_notice": AttributionProcessedPL,
		"count":            len(filtered),
		"warnings":         filtered,
	}
	if teryt != "" {
		resp["teryt"] = teryt
	}
	if voiv != "" {
		resp["voivodeship"] = voiv
	}
	if matchedStation != nil {
		resp["nearest_station"] = map[string]any{
			"id":   matchedStation.ID,
			"name": matchedStation.Name,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type stationEntry struct {
	store.Station
	DistanceKm *float64 `json:"distance_km,omitempty"`
}

func (s *Server) handleStations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source := strings.ToLower(strings.TrimSpace(q.Get("source")))
	if source == "all" {
		source = ""
	}
	if source != "" && source != "synop" && source != "meteo" && source != "hydro" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": "source must be synop, meteo, hydro, or all"})
		return
	}

	reqLat, reqLon, hasCoords, err := parseCoordPair(q.Get("latitude"), q.Get("longitude"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": err.Error()})
		return
	}

	limit := 0
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": true, "reason": "limit must be >= 1"})
			return
		}
		limit = n
	}

	stations, err := s.store.ListStations(r.Context(), source)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": true, "reason": "failed to list stations"})
		return
	}

	entries := make([]stationEntry, 0, len(stations))
	for _, st := range stations {
		e := stationEntry{Station: st}
		if hasCoords {
			d := math.Round(geo.HaversineKm(reqLat, reqLon, st.Lat, st.Lon)*100) / 100
			e.DistanceKm = &d
		}
		entries = append(entries, e)
	}

	if hasCoords {
		sort.Slice(entries, func(i, j int) bool {
			return *entries[i].DistanceKm < *entries[j].DistanceKm
		})
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"attribution":      AttributionSourcePL,
		"processed_notice": AttributionProcessedPL,
		"count":            len(entries),
		"stations":         entries,
	})
}

func parseCoordPair(latRaw, lonRaw string) (float64, float64, bool, error) {
	latStr := strings.TrimSpace(latRaw)
	lonStr := strings.TrimSpace(lonRaw)
	if latStr == "" && lonStr == "" {
		return 0, 0, false, nil
	}
	if latStr == "" || lonStr == "" {
		return 0, 0, false, errMissingCoordPair
	}
	lat, err1 := strconv.ParseFloat(latStr, 64)
	lon, err2 := strconv.ParseFloat(lonStr, 64)
	if err1 != nil || err2 != nil || math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
		return 0, 0, false, errInvalidCoords
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false, errCoordsOutOfRange
	}
	return lat, lon, true, nil
}

func matchesTERYT(teryts []string, target string) bool {
	for _, t := range teryts {
		if t == target || (len(target) == 2 && strings.HasPrefix(t, target)) {
			return true
		}
	}
	return false
}

func matchesVoivodeship(areas []store.HydroArea, voiv string) bool {
	for _, a := range areas {
		if strings.EqualFold(strings.TrimSpace(a.Voivodeship), voiv) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
