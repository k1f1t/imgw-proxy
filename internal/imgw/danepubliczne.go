package imgw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"imgw-openmeteo-proxy/internal/geo"
	"imgw-openmeteo-proxy/internal/store"
)

var warsawLoc = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		return time.FixedZone("CET", 3600)
	}
	return loc
}()

// Client fetches data from IMGW-PIB endpoints (danepubliczne.imgw.pl & meteo.imgw.pl).
type Client struct {
	httpClient           *http.Client
	danepubliczneBaseURL string
	meteoForecastBaseURL string
	meteoForecastToken   string
	forecastModel        string
	userAgent            string
}

// NewClient constructs a new IMGW upstream HTTP client.
func NewClient(danepubliczneBaseURL, meteoForecastBaseURL, meteoForecastToken, forecastModel, userAgent string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if forecastModel == "" {
		forecastModel = "hybrid"
	}
	return &Client{
		httpClient:           &http.Client{Timeout: timeout},
		danepubliczneBaseURL: strings.TrimRight(danepubliczneBaseURL, "/"),
		meteoForecastBaseURL: strings.TrimRight(meteoForecastBaseURL, "/"),
		meteoForecastToken:   meteoForecastToken,
		forecastModel:        forecastModel,
		userAgent:            userAgent,
	}
}

func (c *Client) getBytes(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

type rawSynop struct {
	IDStacji           string  `json:"id_stacji"`
	Stacja             string  `json:"stacja"`
	DataPomiaru        string  `json:"data_pomiaru"`
	GodzinaPomiaru     string  `json:"godzina_pomiaru"`
	Temperatura        *string `json:"temperatura"`
	PredkoscWiatru     *string `json:"predkosc_wiatru"`
	KierunekWiatru     *string `json:"kierunek_wiatru"`
	WilgotnoscWzgledna *string `json:"wilgotnosc_wzgledna"`
	SumaOpadu          *string `json:"suma_opadu"`
	Cisnienie          *string `json:"cisnienie"`
}

// FetchSynop downloads all current synoptic observations from danepubliczne.imgw.pl/api/data/synop.
func (c *Client) FetchSynop(ctx context.Context) ([]store.Station, []store.MeteoObservation, error) {
	url := c.danepubliczneBaseURL + "/synop"
	body, status, err := c.getBytes(ctx, url)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch synop: %w", err)
	}
	if status != http.StatusOK {
		return nil, nil, fmt.Errorf("fetch synop: unexpected HTTP %d", status)
	}

	var rows []rawSynop
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, nil, fmt.Errorf("decode synop: %w", err)
	}

	_, catalogByID, err := SynopCatalog()
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	stations := make([]store.Station, 0, len(rows))
	obs := make([]store.MeteoObservation, 0, len(rows))

	for _, r := range rows {
		if r.IDStacji == "" {
			continue
		}
		meta, hasMeta := catalogByID[r.IDStacji]
		if hasMeta {
			elev := meta.Elevation
			stations = append(stations, store.Station{
				Source:      "synop",
				ID:          r.IDStacji,
				Name:        strings.TrimSpace(r.Stacja),
				Lat:         meta.Lat,
				Lon:         meta.Lon,
				Elevation:   &elev,
				TERYT:       meta.TERYT,
				Voivodeship: geo.VoivodeshipFromTERYT(meta.TERYT),
				UpdatedAt:   now,
			})
		}

		obsTime, ok := parseSynopDateTimeUTC(r.DataPomiaru, r.GodzinaPomiaru)
		if !ok {
			continue
		}
		obs = append(obs, store.MeteoObservation{
			Source:          "synop",
			StationID:       r.IDStacji,
			ObservedAt:      obsTime,
			TemperatureC:    parseOptFloat(r.Temperatura),
			HumidityPct:     parseOptFloat(r.WilgotnoscWzgledna),
			WindSpeedMS:     parseOptFloat(r.PredkoscWiatru),
			WindDirDeg:      parseOptFloat(r.KierunekWiatru),
			PrecipitationMM: parseOptFloat(r.SumaOpadu),
			PressureHPa:     parseOptFloat(r.Cisnienie),
			FetchedAt:       now,
		})
	}
	return stations, obs, nil
}

type rawMeteo struct {
	KodStacji               string  `json:"kod_stacji"`
	NazwaStacji             string  `json:"nazwa_stacji"`
	Lon                     *string `json:"lon"`
	Lat                     *string `json:"lat"`
	WysokoscNPM             *string `json:"wysokosc_npm"`
	TemperaturaGruntu       *string `json:"temperatura_gruntu"`
	TemperaturaGruntuData   *string `json:"temperatura_gruntu_data"`
	TemperaturaPowietrza    *string `json:"temperatura_powietrza"`
	TemperaturaPowietrzaDat *string `json:"temperatura_powietrza_data"`
	WiatrKierunek           *string `json:"wiatr_kierunek"`
	WiatrKierunekData       *string `json:"wiatr_kierunek_data"`
	WiatrSredniaPredkosc    *string `json:"wiatr_srednia_predkosc"`
	WiatrSredniaPredkoscDat *string `json:"wiatr_srednia_predkosc_data"`
	WiatrPredkoscMaks       *string `json:"wiatr_predkosc_maksymalna"`
	WiatrPredkoscMaksData   *string `json:"wiatr_predkosc_maksymalna_data"`
	WilgotnoscWzgledna      *string `json:"wilgotnosc_wzgledna"`
	WilgotnoscWzglednaData  *string `json:"wilgotnosc_wzgledna_data"`
	WiatrPoryw10Min         *string `json:"wiatr_poryw_10min"`
	WiatrPoryw10MinData     *string `json:"wiatr_poryw_10min_data"`
	Opad10Min               *string `json:"opad_10min"`
	Opad10MinData           *string `json:"opad_10min_data"`
}

// FetchMeteo downloads all current 10-min automatic station telemetry from danepubliczne.imgw.pl/api/data/meteo.
func (c *Client) FetchMeteo(ctx context.Context) ([]store.Station, []store.MeteoObservation, error) {
	url := c.danepubliczneBaseURL + "/meteo"
	body, status, err := c.getBytes(ctx, url)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch meteo: %w", err)
	}
	if status != http.StatusOK {
		return nil, nil, fmt.Errorf("fetch meteo: unexpected HTTP %d", status)
	}

	var rows []rawMeteo
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, nil, fmt.Errorf("decode meteo: %w", err)
	}

	now := time.Now().UTC()
	stations := make([]store.Station, 0, len(rows))
	obs := make([]store.MeteoObservation, 0, len(rows))

	for _, r := range rows {
		if r.KodStacji == "" {
			continue
		}
		lat := parseOptFloat(r.Lat)
		lon := parseOptFloat(r.Lon)
		if lat == nil || lon == nil {
			continue
		}
		stations = append(stations, store.Station{
			Source:    "meteo",
			ID:        r.KodStacji,
			Name:      strings.TrimSpace(r.NazwaStacji),
			Lat:       *lat,
			Lon:       *lon,
			Elevation: parseOptFloat(r.WysokoscNPM),
			UpdatedAt: now,
		})

		obsTime := latestValidTimeUTC(
			r.TemperaturaPowietrzaDat,
			r.WilgotnoscWzglednaData,
			r.WiatrSredniaPredkoscDat,
			r.Opad10MinData,
			r.TemperaturaGruntuData,
			r.WiatrKierunekData,
		)
		if obsTime.IsZero() {
			continue
		}

		// Use wind gust only if its timestamp is reasonably close to obsTime (within 30 min);
		// otherwise prefer 10-min max wind speed.
		gust := parseOptFloat(r.WiatrPredkoscMaks)
		if p10 := parseOptFloat(r.WiatrPoryw10Min); p10 != nil {
			if t := parseDateTimeUTC(r.WiatrPoryw10MinData); t != nil && obsTime.Sub(*t) <= 30*time.Minute && obsTime.Sub(*t) >= -30*time.Minute {
				gust = p10
			}
		}

		obs = append(obs, store.MeteoObservation{
			Source:          "meteo",
			StationID:       r.KodStacji,
			ObservedAt:      obsTime,
			TemperatureC:    freshVal(parseOptFloat(r.TemperaturaPowietrza), parseDateTimeUTC(r.TemperaturaPowietrzaDat), obsTime, 2*time.Hour),
			GroundTempC:     freshVal(parseOptFloat(r.TemperaturaGruntu), parseDateTimeUTC(r.TemperaturaGruntuData), obsTime, 2*time.Hour),
			HumidityPct:     freshVal(parseOptFloat(r.WilgotnoscWzgledna), parseDateTimeUTC(r.WilgotnoscWzglednaData), obsTime, 2*time.Hour),
			WindSpeedMS:     freshVal(parseOptFloat(r.WiatrSredniaPredkosc), parseDateTimeUTC(r.WiatrSredniaPredkoscDat), obsTime, 2*time.Hour),
			WindGustMS:      gust,
			WindDirDeg:      freshVal(parseOptFloat(r.WiatrKierunek), parseDateTimeUTC(r.WiatrKierunekData), obsTime, 2*time.Hour),
			PrecipitationMM: freshVal(parseOptFloat(r.Opad10Min), parseDateTimeUTC(r.Opad10MinData), obsTime, 2*time.Hour),
			FetchedAt:       now,
		})
	}
	return stations, obs, nil
}

type rawHydro struct {
	IDStacji               string  `json:"id_stacji"`
	Stacja                 string  `json:"stacja"`
	Rzeka                  string  `json:"rzeka"`
	Wojewodztwo            *string `json:"wojewodztwo"`
	Lon                    *string `json:"lon"`
	Lat                    *string `json:"lat"`
	RzednaZero             *string `json:"rzedna_zerawodowskazu"`
	StanAlarmowy           *string `json:"stan_alarmowy"`
	StanOstrzegawczy       *string `json:"stan_ostrzegawczy"`
	StanWody               *string `json:"stan_wody"`
	StanWodyData           *string `json:"stan_wody_data_pomiaru"`
	TemperaturaWody        *string `json:"temperatura_wody"`
	TemperaturaWodyData    *string `json:"temperatura_wody_data_pomiaru"`
	Przeplyw               *string `json:"przeplyw"`
	PrzeplywData           *string `json:"przeplyw_data"`
	ZjawiskoLodowe         *string `json:"zjawisko_lodowe"`
	ZjawiskoLodoweData     *string `json:"zjawisko_lodowe_data_pomiaru"`
	ZjawiskoZarastania     *string `json:"zjawisko_zarastania"`
	ZjawiskoZarastaniaData *string `json:"zjawisko_zarastania_data_pomiaru"`
}

// FetchHydro downloads all current hydrological readings from danepubliczne.imgw.pl/api/data/hydro.
func (c *Client) FetchHydro(ctx context.Context) ([]store.Station, []store.HydroObservation, error) {
	url := c.danepubliczneBaseURL + "/hydro"
	body, status, err := c.getBytes(ctx, url)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch hydro: %w", err)
	}
	if status != http.StatusOK {
		return nil, nil, fmt.Errorf("fetch hydro: unexpected HTTP %d", status)
	}

	var rows []rawHydro
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, nil, fmt.Errorf("decode hydro: %w", err)
	}

	now := time.Now().UTC()
	stations := make([]store.Station, 0, len(rows))
	obs := make([]store.HydroObservation, 0, len(rows))

	for _, r := range rows {
		if r.IDStacji == "" {
			continue
		}
		lat := parseOptFloat(r.Lat)
		lon := parseOptFloat(r.Lon)
		if lat == nil || lon == nil {
			continue
		}
		voiv := ""
		if r.Wojewodztwo != nil {
			voiv = strings.TrimSpace(*r.Wojewodztwo)
		}
		stations = append(stations, store.Station{
			Source:       "hydro",
			ID:           r.IDStacji,
			Name:         strings.TrimSpace(r.Stacja),
			Lat:          *lat,
			Lon:          *lon,
			Elevation:    parseOptFloat(r.RzednaZero),
			Voivodeship:  voiv,
			River:        strings.TrimSpace(r.Rzeka),
			WarningLevel: parseOptFloat(r.StanOstrzegawczy),
			AlarmLevel:   parseOptFloat(r.StanAlarmowy),
			UpdatedAt:    now,
		})

		obsTime := latestValidTimeUTC(r.StanWodyData, r.PrzeplywData, r.TemperaturaWodyData)
		if obsTime.IsZero() {
			obsTime = now.Truncate(time.Minute)
		}
		obs = append(obs, store.HydroObservation{
			StationID:           r.IDStacji,
			ObservedAt:          obsTime,
			WaterLevelCM:        parseOptFloat(r.StanWody),
			WaterLevelAt:        parseDateTimeUTC(r.StanWodyData),
			WaterTempC:          parseOptFloat(r.TemperaturaWody),
			WaterTempAt:         parseDateTimeUTC(r.TemperaturaWodyData),
			WaterFlowM3S:        parseOptFloat(r.Przeplyw),
			WaterFlowAt:         parseDateTimeUTC(r.PrzeplywData),
			IcePhenomena:        cleanOptString(r.ZjawiskoLodowe),
			IceAt:               parseDateTimeUTC(r.ZjawiskoLodoweData),
			VegetationPhenomena: cleanOptString(r.ZjawiskoZarastania),
			VegetationAt:        parseDateTimeUTC(r.ZjawiskoZarastaniaData),
			FetchedAt:           now,
		})
	}
	return stations, obs, nil
}

type rawWarningMeteo struct {
	ID                 string   `json:"id"`
	NazwaZdarzenia     string   `json:"nazwa_zdarzenia"`
	Stopien            string   `json:"stopien"`
	Prawdopodobienstwo string   `json:"prawdopodobienstwo"`
	ObowiazujeOd       string   `json:"obowiazuje_od"`
	ObowiazujeDo       string   `json:"obowiazuje_do"`
	Opublikowano       string   `json:"opublikowano"`
	Tresc              string   `json:"tresc"`
	Komentarz          string   `json:"komentarz"`
	Biuro              string   `json:"biuro"`
	Teryt              []string `json:"teryt"`
}

// FetchWarningsMeteo downloads active meteorological warnings from danepubliczne.imgw.pl/api/data/warningsmeteo.
func (c *Client) FetchWarningsMeteo(ctx context.Context) ([]store.Warning, error) {
	url := c.danepubliczneBaseURL + "/warningsmeteo"
	body, status, err := c.getBytes(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch warningsmeteo: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("fetch warningsmeteo: unexpected HTTP %d", status)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] == '{' {
		// IMGW returns {"status":false,"message":"No products were found"} when there are no active warnings.
		return nil, nil
	}

	var rows []rawWarningMeteo
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return nil, fmt.Errorf("decode warningsmeteo: %w", err)
	}

	now := time.Now().UTC()
	out := make([]store.Warning, 0, len(rows))
	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			id = fmt.Sprintf("%s-%s", r.NazwaZdarzenia, r.Opublikowano)
		}
		out = append(out, store.Warning{
			Type:        "meteo",
			ID:          id,
			Event:       strings.TrimSpace(r.NazwaZdarzenia),
			Level:       strings.TrimSpace(r.Stopien),
			Probability: parseOptInt(r.Prawdopodobienstwo),
			ValidFrom:   parseDateTimeWarsaw(r.ObowiazujeOd),
			ValidTo:     parseDateTimeWarsaw(r.ObowiazujeDo),
			PublishedAt: parseDateTimeWarsaw(r.Opublikowano),
			Content:     strings.TrimSpace(r.Tresc),
			Comment:     strings.TrimSpace(r.Komentarz),
			Office:      strings.TrimSpace(r.Biuro),
			TERYTs:      r.Teryt,
			FetchedAt:   now,
		})
	}
	return out, nil
}

type rawWarningHydroArea struct {
	Wojewodztwo string   `json:"wojewodztwo"`
	Opis        string   `json:"opis"`
	KodZlewni   []string `json:"kod_zlewni"`
}

type rawWarningHydro struct {
	Numer              string                `json:"numer"`
	Zdarzenie          string                `json:"zdarzenie"`
	StopienDiacritic   string                `json:"stopień"`
	StopienASCII       string                `json:"stopien"`
	Prawdopodobienstwo string                `json:"prawdopodobienstwo"`
	DataOd             string                `json:"data_od"`
	DataDo             string                `json:"data_do"`
	Opublikowano       string                `json:"opublikowano"`
	Przebieg           string                `json:"przebieg"`
	Komentarz          string                `json:"komentarz"`
	Biuro              string                `json:"biuro"`
	Obszary            []rawWarningHydroArea `json:"obszary"`
}

// FetchWarningsHydro downloads active hydrological warnings from danepubliczne.imgw.pl/api/data/warningshydro.
func (c *Client) FetchWarningsHydro(ctx context.Context) ([]store.Warning, error) {
	url := c.danepubliczneBaseURL + "/warningshydro"
	body, status, err := c.getBytes(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch warningshydro: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("fetch warningshydro: unexpected HTTP %d", status)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] == '{' {
		return nil, nil
	}

	var rows []rawWarningHydro
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return nil, fmt.Errorf("decode warningshydro: %w", err)
	}

	now := time.Now().UTC()
	out := make([]store.Warning, 0, len(rows))
	for i, r := range rows {
		level := strings.TrimSpace(r.StopienDiacritic)
		if level == "" {
			level = strings.TrimSpace(r.StopienASCII)
		}
		areas := make([]store.HydroArea, 0, len(r.Obszary))
		for _, a := range r.Obszary {
			areas = append(areas, store.HydroArea{
				Voivodeship:    strings.TrimSpace(a.Wojewodztwo),
				Description:    strings.TrimSpace(a.Opis),
				CatchmentCodes: a.KodZlewni,
			})
		}
		id := fmt.Sprintf("%s-%s-%d", strings.TrimSpace(r.Numer), strings.TrimSpace(r.Opublikowano), i)
		out = append(out, store.Warning{
			Type:        "hydro",
			ID:          id,
			Event:       strings.TrimSpace(r.Zdarzenie),
			Level:       level,
			Probability: parseOptInt(r.Prawdopodobienstwo),
			ValidFrom:   parseDateTimeWarsaw(r.DataOd),
			ValidTo:     parseDateTimeWarsaw(r.DataDo),
			PublishedAt: parseDateTimeWarsaw(r.Opublikowano),
			Content:     strings.TrimSpace(r.Przebieg),
			Comment:     strings.TrimSpace(r.Komentarz),
			Office:      strings.TrimSpace(r.Biuro),
			Areas:       areas,
			FetchedAt:   now,
		})
	}
	return out, nil
}

func parseOptFloat(s *string) *float64 {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" || strings.EqualFold(v, "null") {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	return &f
}

func parseOptInt(s string) *int {
	v := strings.TrimSpace(s)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

func cleanOptString(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}

func parseSynopDateTimeUTC(dateStr, hourStr string) (time.Time, bool) {
	d := strings.TrimSpace(dateStr)
	h := strings.TrimSpace(hourStr)
	if d == "" || h == "" {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(h)
	if err != nil || hour < 0 || hour > 23 {
		return time.Time{}, false
	}
	base, err := time.ParseInLocation("2006-01-02", d, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return base.Add(time.Duration(hour) * time.Hour), true
}

func parseDateTimeUTC(s *string) *time.Time {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			u := t.UTC()
			return &u
		}
	}
	return nil
}

func parseDateTimeWarsaw(s string) *time.Time {
	v := strings.TrimSpace(s)
	if v == "" || strings.HasPrefix(v, "9999-") {
		return nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, v, warsawLoc); err == nil {
			u := t.UTC()
			return &u
		}
	}
	return nil
}

func latestValidTimeUTC(vals ...*string) time.Time {
	var best time.Time
	for _, s := range vals {
		if t := parseDateTimeUTC(s); t != nil {
			if t.After(best) {
				best = *t
			}
		}
	}
	return best
}

func freshVal(val *float64, ts *time.Time, ref time.Time, maxAge time.Duration) *float64 {
	if val == nil || ts == nil {
		return nil
	}
	diff := ref.Sub(*ts)
	if diff < 0 {
		diff = -diff
	}
	if diff > maxAge {
		return nil
	}
	return val
}
