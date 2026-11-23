package imgw

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientDanepubliczneAndForecast(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/data/synop", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id_stacji":"12375","stacja":"Warszawa","data_pomiaru":"2026-11-23","godzina_pomiaru":"16","temperatura":"2.0","predkosc_wiatru":"5","kierunek_wiatru":"270","wilgotnosc_wzgledna":"90.5","suma_opadu":"0.2","cisnienie":"1011.1"}
		]`))
	})
	mux.HandleFunc("/api/data/meteo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"kod_stacji":"352200375","nazwa_stacji":"WARSZAWA-OKĘCIE","lon":"20.961111","lat":"52.162778","wysokosc_npm":"106","temperatura_powietrza":"1.9","temperatura_powietrza_data":"2026-11-23 16:40:00","wiatr_srednia_predkosc":"5.4","wiatr_srednia_predkosc_data":"2026-11-23 16:40:00","wiatr_predkosc_maksymalna":"9.1","wiatr_predkosc_maksymalna_data":"2026-11-23 16:40:00","wiatr_kierunek":"272","wiatr_kierunek_data":"2026-11-23 16:40:00","wilgotnosc_wzgledna":"92.6","wilgotnosc_wzgledna_data":"2026-11-23 16:40:00","opad_10min":"0.1","opad_10min_data":"2026-11-23 16:40:00"}
		]`))
	})
	mux.HandleFunc("/api/data/hydro", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id_stacji":"151140030","stacja":"Przewoźniki","rzeka":"Skroda","wojewodztwo":"lubuskie","lon":"14.8217","lat":"51.5253","stan_alarmowy":"340","stan_ostrzegawczy":"300","stan_wody":"310","stan_wody_data_pomiaru":"2026-11-23 16:20:00","przeplyw":"1.2","przeplyw_data":"2026-11-23 16:20:00"}
		]`))
	})
	mux.HandleFunc("/api/data/warningsmeteo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":false,"message":"No products were found"}`))
	})
	mux.HandleFunc("/api/data/warningshydro", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"opublikowano":"2026-11-23 12:00:00","stopień":"2","data_od":"2026-11-23 13:00:00","data_do":"2026-11-24 13:00:00","prawdopodobienstwo":"85","numer":"10","biuro":"BPH","zdarzenie":"Wezbranie","przebieg":"Wzrost","komentarz":"Brak","obszary":[{"wojewodztwo":"mazowieckie","opis":"Wisła","kod_zlewni":["Z1"]}]}
		]`))
	})
	mux.HandleFunc("/api/v1/forecast/fcapi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"data": {
				"Model": "HYBRID",
				"Valid": true,
				"Location": {"Lat": 52.23, "Lon": 21.01},
				"Data": [
					{"Type":"Type_Hour","Date":"2026-11-23T17:00:00Z","Temperature":"275.15","Chill":"270.15","PressureMSL":"101200","Humidity":"90","Cloud":"80","Wind_Speed":"5.0","Wind_Gust":"9.0","Wind_Dir":"270","Precipitation10m":"0.1","Rain10m":"0.1","Snow10m":"0.0","Icon10":"n6z61n"},
					{"Type":"Type_Ten_Minutes","Date":"2026-11-23T17:10:00Z","Precipitation10m":"0.2","Rain10m":"0.2","Snow10m":"0.0"},
					{"Type":"Type_Hour","Date":"2026-11-23T18:00:00Z","Temperature":"273.15","Chill":"268.15","PressureMSL":"101300","Humidity":"95","Cloud":"100","Wind_Speed":"4.0","Wind_Gust":"7.0","Wind_Dir":"280","Precipitation":"2.0","Rain":"0.0","Snow":"2.0","Icon":"n8z71n"}
				]
			}
		}`))
	})
	mux.HandleFunc("/api/v1/forecast/forecastapi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"data": {
				"Model": "GFS",
				"Valid": true,
				"Location": {"Lat": 52.23, "Lon": 21.01},
				"Data": [
					{"Type":"Type_Hour","Date":"2026-11-23T18:00:00Z","Temperature":"280.15","PressureMSL":"100000","Humidity":"50","Cloud":"10","Wind_Speed":"1.0","Wind_Dir":"100","Precipitation":"0.0","Icon":"n1z00n"},
					{"Type":"Type_Hour","Date":"2026-11-24T00:00:00Z","Temperature":"274.15","PressureMSL":"101500","Humidity":"85","Cloud":"50","Wind_Speed":"3.0","Wind_Dir":"250","Precipitation":"0.0","Icon":"n4z00n"}
				]
			}
		}`))
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL+"/api/data", ts.URL, "test-token", "hybrid", "test-agent", 5*time.Second)
	ctx := context.Background()

	synopStations, synopObs, err := client.FetchSynop(ctx)
	if err != nil {
		t.Fatalf("FetchSynop error: %v", err)
	}
	if len(synopStations) != 1 || synopStations[0].TERYT != "1465" {
		t.Fatalf("unexpected synop stations: %+v", synopStations)
	}
	if len(synopObs) != 1 || *synopObs[0].TemperatureC != 2.0 || *synopObs[0].PressureHPa != 1011.1 {
		t.Fatalf("unexpected synop obs: %+v", synopObs)
	}

	meteoStations, meteoObs, err := client.FetchMeteo(ctx)
	if err != nil {
		t.Fatalf("FetchMeteo error: %v", err)
	}
	if len(meteoStations) != 1 || len(meteoObs) != 1 || *meteoObs[0].WindGustMS != 9.1 {
		t.Fatalf("unexpected meteo result: %+v %+v", meteoStations, meteoObs)
	}

	hydroStations, hydroObs, err := client.FetchHydro(ctx)
	if err != nil {
		t.Fatalf("FetchHydro error: %v", err)
	}
	if len(hydroStations) != 1 || *hydroStations[0].WarningLevel != 300 || len(hydroObs) != 1 || *hydroObs[0].WaterLevelCM != 310 {
		t.Fatalf("unexpected hydro result: %+v %+v", hydroStations, hydroObs)
	}

	mw, err := client.FetchWarningsMeteo(ctx)
	if err != nil || len(mw) != 0 {
		t.Fatalf("expected empty warningsmeteo for status:false object, got %v %v", mw, err)
	}

	hw, err := client.FetchWarningsHydro(ctx)
	if err != nil || len(hw) != 1 || hw[0].Level != "2" {
		t.Fatalf("unexpected hydro warnings: %+v err=%v", hw, err)
	}

	fc, err := client.FetchForecast(ctx, 52.23, 21.01)
	if err != nil {
		t.Fatalf("FetchForecast error: %v", err)
	}
	if fc.Model != "HYBRID+GFS" {
		t.Fatalf("expected blended model HYBRID+GFS, got %q", fc.Model)
	}
	if len(fc.Hourly) != 3 {
		t.Fatalf("expected 3 merged hourly points, got %d", len(fc.Hourly))
	}
	// Hour 0: 275.15K = 2.0°C, 101200 Pa = 1012 hPa, precip = 0.1 + 0.2 (10m sum) = 0.3 mm
	if math.Abs(*fc.Hourly[0].TemperatureC-2.0) > 0.01 {
		t.Errorf("expected 2.0°C, got %.2f", *fc.Hourly[0].TemperatureC)
	}
	if math.Abs(*fc.Hourly[0].PressureHPa-1012.0) > 0.01 {
		t.Errorf("expected 1012 hPa, got %.2f", *fc.Hourly[0].PressureHPa)
	}
	if math.Abs(*fc.Hourly[0].PrecipitationMM-0.3) > 0.001 {
		t.Errorf("expected 0.3mm summed precipitation, got %.3f", *fc.Hourly[0].PrecipitationMM)
	}
	// Hour 1: HYBRID (0°C, snow 2.0mm -> 1.4cm) MUST override GFS (7°C)
	if math.Abs(*fc.Hourly[1].TemperatureC-0.0) > 0.01 {
		t.Errorf("expected HYBRID override temp 0.0°C, got %.2f", *fc.Hourly[1].TemperatureC)
	}
	if math.Abs(*fc.Hourly[1].SnowfallCM-1.4) > 0.01 {
		t.Errorf("expected 1.4 cm snowfall (2mm * 0.7), got %.2f", *fc.Hourly[1].SnowfallCM)
	}
	// Hour 2: from GFS (274.15K = 1.0°C)
	if math.Abs(*fc.Hourly[2].TemperatureC-1.0) > 0.01 {
		t.Errorf("expected GFS temp 1.0°C, got %.2f", *fc.Hourly[2].TemperatureC)
	}
}
