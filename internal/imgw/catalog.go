package imgw

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"imgw-openmeteo-proxy/internal/geo"
)

//go:embed synop_stations.json
var synopStationsJSON []byte

// SynopStationMeta holds static coordinates, elevation, and TERYT for an IMGW synoptic station.
type SynopStationMeta struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Elevation float64 `json:"elevation"`
	TERYT     string  `json:"teryt"`
}

var (
	synopCatalogOnce sync.Once
	synopCatalogList []SynopStationMeta
	synopCatalogByID map[string]SynopStationMeta
	synopCatalogErr  error
)

// SynopCatalog returns all embedded IMGW synoptic station metadata entries.
func SynopCatalog() ([]SynopStationMeta, map[string]SynopStationMeta, error) {
	synopCatalogOnce.Do(func() {
		var list []SynopStationMeta
		if err := json.Unmarshal(synopStationsJSON, &list); err != nil {
			synopCatalogErr = fmt.Errorf("parse embedded synop_stations.json: %w", err)
			return
		}
		byID := make(map[string]SynopStationMeta, len(list))
		for _, st := range list {
			byID[st.ID] = st
		}
		synopCatalogList = list
		synopCatalogByID = byID
	})
	return synopCatalogList, synopCatalogByID, synopCatalogErr
}

// NearestSynopMeta finds the closest embedded synoptic station to (lat, lon).
func NearestSynopMeta(lat, lon float64) (SynopStationMeta, float64, error) {
	list, _, err := SynopCatalog()
	if err != nil {
		return SynopStationMeta{}, 0, err
	}
	if len(list) == 0 {
		return SynopStationMeta{}, 0, fmt.Errorf("synop catalog is empty")
	}
	best := list[0]
	bestDist := geo.HaversineKm(lat, lon, best.Lat, best.Lon)
	for _, st := range list[1:] {
		d := geo.HaversineKm(lat, lon, st.Lat, st.Lon)
		if d < bestDist {
			best = st
			bestDist = d
		}
	}
	return best, bestDist, nil
}
