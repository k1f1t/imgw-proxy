package openmeteo

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// UnitPrefs defines the requested Open-Meteo output units.
type UnitPrefs struct {
	Temperature   string // "celsius" (default) | "fahrenheit"
	WindSpeed     string // "kmh" (default) | "ms" | "mph" | "kn"
	Precipitation string // "mm" (default) | "inch"
	TimeFormat    string // "iso8601" (default) | "unixtime"
}

func DefaultUnitPrefs() UnitPrefs {
	return UnitPrefs{
		Temperature:   "celsius",
		WindSpeed:     "kmh",
		Precipitation: "mm",
		TimeFormat:    "iso8601",
	}
}

func (u UnitPrefs) Validate() error {
	switch u.Temperature {
	case "celsius", "fahrenheit":
	default:
		return fmt.Errorf("invalid temperature_unit %q (expected celsius or fahrenheit)", u.Temperature)
	}
	switch u.WindSpeed {
	case "kmh", "ms", "mph", "kn":
	default:
		return fmt.Errorf("invalid wind_speed_unit %q (expected kmh, ms, mph, or kn)", u.WindSpeed)
	}
	switch u.Precipitation {
	case "mm", "inch":
	default:
		return fmt.Errorf("invalid precipitation_unit %q (expected mm or inch)", u.Precipitation)
	}
	switch u.TimeFormat {
	case "iso8601", "unixtime":
	default:
		return fmt.Errorf("invalid timeformat %q (expected iso8601 or unixtime)", u.TimeFormat)
	}
	return nil
}

func (u UnitPrefs) TempUnitLabel() string {
	if u.Temperature == "fahrenheit" {
		return "°F"
	}
	return "°C"
}

func (u UnitPrefs) WindUnitLabel() string {
	switch u.WindSpeed {
	case "ms":
		return "m/s"
	case "mph":
		return "mp/h"
	case "kn":
		return "kn"
	default:
		return "km/h"
	}
}

func (u UnitPrefs) PrecipUnitLabel() string {
	if u.Precipitation == "inch" {
		return "inch"
	}
	return "mm"
}

func (u UnitPrefs) SnowUnitLabel() string {
	if u.Precipitation == "inch" {
		return "inch"
	}
	return "cm"
}

func (u UnitPrefs) ConvertTemp(c *float64) *float64 {
	if c == nil {
		return nil
	}
	v := *c
	if u.Temperature == "fahrenheit" {
		v = v*9.0/5.0 + 32.0
	}
	r := round1(v)
	return &r
}

func (u UnitPrefs) ConvertWind(ms *float64) *float64 {
	if ms == nil {
		return nil
	}
	v := *ms
	switch u.WindSpeed {
	case "kmh":
		v *= 3.6
	case "mph":
		v *= 2.2369362921
	case "kn":
		v *= 1.9438444924
	case "ms":
		// already m/s
	}
	r := round1(v)
	return &r
}

func (u UnitPrefs) ConvertPrecipMM(mm *float64) *float64 {
	if mm == nil {
		return nil
	}
	v := *mm
	if u.Precipitation == "inch" {
		v /= 25.4
		r := round3(v)
		return &r
	}
	r := round2(v)
	return &r
}

func (u UnitPrefs) ConvertSnowCM(cm *float64) *float64 {
	if cm == nil {
		return nil
	}
	v := *cm
	if u.Precipitation == "inch" {
		v /= 2.54
	}
	r := round2(v)
	return &r
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// WeatherCodeFromIMGWIcon maps IMGW icon strings (e.g. "n4z61d", "n0z00d", "n8z71n",
// or short condition labels) to Open-Meteo WMO weather interpretation codes (0–99).
func WeatherCodeFromIMGWIcon(icon string) (int, bool) {
	s := strings.TrimSpace(strings.ToLower(icon))
	if s == "" {
		return 0, false
	}

	// Standard IMGW format: n{octas}z{wmo2digits}{d|n} e.g. n4z61d or n8z00n
	if len(s) >= 5 && s[0] == 'n' && s[2] == 'z' {
		cloud, err1 := strconv.Atoi(s[1:2])
		precipStr := s[3:5]
		precip, err2 := strconv.Atoi(precipStr)
		if err1 == nil && err2 == nil {
			if precip > 0 {
				return normalizeWMOCode(precip), true
			}
			return cloudOctasToWMO(cloud), true
		}
	}

	// Some IMGW endpoints return pure numeric WMO codes as strings.
	if code, err := strconv.Atoi(s); err == nil && code >= 0 && code <= 99 {
		return normalizeWMOCode(code), true
	}

	return 0, false
}

// SynthesizeWeatherCode derives a reasonable WMO weather_code when no explicit IMGW icon is present.
func SynthesizeWeatherCode(tempC, precipMM, rainMM, snowCM, cloudPct, humidityPct *float64) int {
	p := 0.0
	if precipMM != nil {
		p = *precipMM
	}
	rain := 0.0
	if rainMM != nil {
		rain = *rainMM
	}
	snow := 0.0
	if snowCM != nil {
		snow = *snowCM
	}

	if snow > 0 || (p > 0 && tempC != nil && *tempC <= 0.5 && rain == 0) {
		switch {
		case snow >= 2.0 || p >= 4.0:
			return 75 // Heavy snow fall
		case snow >= 0.5 || p >= 1.0:
			return 73 // Moderate snow fall
		default:
			return 71 // Slight snow fall
		}
	}

	if rain > 0 || p > 0 {
		amt := math.Max(rain, p)
		switch {
		case amt >= 7.5:
			return 65 // Heavy rain
		case amt >= 2.5:
			return 63 // Moderate rain
		case amt >= 0.3:
			return 61 // Slight rain
		default:
			return 51 // Light drizzle
		}
	}

	if cloudPct != nil {
		switch {
		case *cloudPct >= 85:
			return 3 // Overcast
		case *cloudPct >= 45:
			return 2 // Partly cloudy
		case *cloudPct >= 15:
			return 1 // Mainly clear
		default:
			return 0 // Clear sky
		}
	}

	if humidityPct != nil && *humidityPct >= 97 {
		return 45 // Fog
	}
	return 1 // Mainly clear fallback when cloud cover is unavailable
}

func cloudOctasToWMO(octas int) int {
	switch {
	case octas <= 1:
		return 0 // Clear sky
	case octas <= 3:
		return 1 // Mainly clear
	case octas <= 5:
		return 2 // Partly cloudy
	default:
		return 3 // Overcast
	}
}

func normalizeWMOCode(code int) int {
	switch {
	case code >= 95:
		if code == 96 || code == 99 {
			return code
		}
		return 95
	case code >= 85 && code <= 86:
		return code
	case code >= 80 && code <= 82:
		return code
	case code >= 83 && code <= 84:
		return 80
	case code >= 77 && code <= 79:
		return 77
	case code >= 75:
		return 75
	case code >= 73:
		return 73
	case code >= 70:
		return 71
	case code >= 66 && code <= 67:
		return code
	case code >= 65:
		return 65
	case code >= 63:
		return 63
	case code >= 60:
		return 61
	case code >= 56 && code <= 57:
		return code
	case code >= 55:
		return 55
	case code >= 53:
		return 53
	case code >= 50:
		return 51
	case code >= 45 && code <= 49:
		if code == 48 {
			return 48
		}
		return 45
	case code >= 40 && code <= 44:
		return 45
	case code >= 3:
		return 3
	case code >= 0:
		return code
	default:
		return 0
	}
}
