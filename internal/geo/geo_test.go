package geo

import (
	"math"
	"testing"
	"time"
)

func TestHaversineKm(t *testing.T) {
	// Warszawa (52.2297, 21.0122) to Kraków (50.0647, 19.9450) is ~252 km.
	d := HaversineKm(52.2297, 21.0122, 50.0647, 19.9450)
	if math.Abs(d-252.0) > 3.0 {
		t.Fatalf("expected ~252 km between Warszawa and Kraków, got %.2f km", d)
	}
	if zero := HaversineKm(52.0, 21.0, 52.0, 21.0); zero != 0 {
		t.Fatalf("expected 0 km for identical coords, got %f", zero)
	}
}

func TestApparentTemperatureC(t *testing.T) {
	// Cold windy weather should feel colder than actual air temperature.
	coldWind := ApparentTemperatureC(2.0, 80.0, 8.0)
	if coldWind >= 2.0 {
		t.Fatalf("expected apparent temp < 2.0°C in wind, got %.2f", coldWind)
	}
	// Warm humid calm weather should feel warmer than actual air temperature.
	hotHumid := ApparentTemperatureC(30.0, 80.0, 0.5)
	if hotHumid <= 30.0 {
		t.Fatalf("expected apparent temp > 30.0°C in humid heat, got %.2f", hotHumid)
	}
}

func TestSunriseSunsetAndIsDay(t *testing.T) {
	// Warszawa on 2026-11-23: sunrise ~06:10 UTC, sunset ~14:34 UTC.
	sr, ss, ok := SunriseSunset(2026, time.November, 23, 52.23, 21.01)
	if !ok {
		t.Fatal("expected valid sunrise/sunset for Warszawa")
	}
	if sr.Hour() != 6 || sr.Minute() < 0 || sr.Minute() > 25 {
		t.Fatalf("unexpected sunrise UTC for Warszawa in Nov: %s", sr.Format(time.RFC3339))
	}
	if ss.Hour() != 14 || ss.Minute() < 20 || ss.Minute() > 50 {
		t.Fatalf("unexpected sunset UTC for Warszawa in Nov: %s", ss.Format(time.RFC3339))
	}
	if !IsDay(time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC), 52.23, 21.01) {
		t.Fatal("expected 11:00 UTC to be daytime in Warszawa")
	}
	if IsDay(time.Date(2026, 11, 23, 20, 0, 0, 0, time.UTC), 52.23, 21.01) {
		t.Fatal("expected 20:00 UTC to be nighttime in Warszawa")
	}
}

func TestVoivodeshipFromTERYT(t *testing.T) {
	if got := VoivodeshipFromTERYT("1465"); got != "mazowieckie" {
		t.Fatalf("expected mazowieckie, got %q", got)
	}
	if got := VoivodeshipFromTERYT("1261"); got != "małopolskie" {
		t.Fatalf("expected małopolskie, got %q", got)
	}
	if got := VoivodeshipFromTERYT(""); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}
