package geo

import (
	"math"
	"time"
)

const earthRadiusKm = 6371.0088

// HaversineKm returns the great-circle distance in kilometers between two WGS84 points.
func HaversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	dLat := degToRad(lat2 - lat1)
	dLon := degToRad(lon2 - lon1)
	rLat1 := degToRad(lat1)
	rLat2 := degToRad(lat2)

	sinLat := math.Sin(dLat / 2)
	sinLon := math.Sin(dLon / 2)
	a := sinLat*sinLat + math.Cos(rLat1)*math.Cos(rLat2)*sinLon*sinLon
	if a > 1 {
		a = 1
	}
	return 2 * earthRadiusKm * math.Asin(math.Sqrt(a))
}

func degToRad(d float64) float64 { return d * math.Pi / 180.0 }
func radToDeg(r float64) float64 { return r * 180.0 / math.Pi }

// ApparentTemperatureC computes Steadman's apparent temperature in °C from
// air temperature (°C), relative humidity (%), and wind speed at 10m (m/s).
// This matches the standard Australian Bureau of Meteorology / Open-Meteo approximation.
func ApparentTemperatureC(tempC, relHumidityPct, windSpeedMS float64) float64 {
	rh := math.Max(0, math.Min(100, relHumidityPct))
	ws := math.Max(0, windSpeedMS)
	// Water vapour pressure (hPa)
	e := (rh / 100.0) * 6.105 * math.Exp((17.27*tempC)/(237.7+tempC))
	return tempC + 0.33*e - 0.70*ws - 4.00
}

// SunriseSunset computes approximate UTC sunrise and sunset times for the given calendar date
// and WGS84 coordinates using standard NOAA solar equations.
// Returns (sunrise, sunset, ok). ok is false during polar day/night (not applicable in Poland).
func SunriseSunset(year int, month time.Month, day int, lat, lon float64) (time.Time, time.Time, bool) {
	noonUTC := time.Date(year, month, day, 12, 0, 0, 0, time.UTC)
	jd := julianDay(noonUTC)
	jc := (jd - 2451545.0) / 36525.0

	geomMeanLongSun := math.Mod(280.46646+jc*(36000.76983+jc*0.0003032), 360.0)
	if geomMeanLongSun < 0 {
		geomMeanLongSun += 360.0
	}
	geomMeanAnomSun := 357.52911 + jc*(35999.05029-0.0001537*jc)
	eccentEarthOrbit := 0.016708634 - jc*(0.000042037+0.0000001267*jc)

	sunEqOfCtr := math.Sin(degToRad(geomMeanAnomSun))*(1.914602-jc*(0.004817+0.000014*jc)) +
		math.Sin(degToRad(2*geomMeanAnomSun))*(0.019993-0.000101*jc) +
		math.Sin(degToRad(3*geomMeanAnomSun))*0.000289

	sunTrueLong := geomMeanLongSun + sunEqOfCtr
	sunAppLong := sunTrueLong - 0.00569 - 0.00478*math.Sin(degToRad(125.04-1934.136*jc))

	meanObliqEcliptic := 23.0 + (26.0+((21.448-jc*(46.815+jc*(0.00059-jc*0.001813)))/60.0))/60.0
	obliqCorr := meanObliqEcliptic + 0.00256*math.Cos(degToRad(125.04-1934.136*jc))

	sunDeclin := radToDeg(math.Asin(math.Sin(degToRad(obliqCorr)) * math.Sin(degToRad(sunAppLong))))

	varY := math.Tan(degToRad(obliqCorr/2.0)) * math.Tan(degToRad(obliqCorr/2.0))
	eqOfTime := 4.0 * radToDeg(
		varY*math.Sin(2.0*degToRad(geomMeanLongSun))-
			2.0*eccentEarthOrbit*math.Sin(degToRad(geomMeanAnomSun))+
			4.0*eccentEarthOrbit*varY*math.Sin(degToRad(geomMeanAnomSun))*math.Cos(2.0*degToRad(geomMeanLongSun))-
			0.5*varY*varY*math.Sin(4.0*degToRad(geomMeanLongSun))-
			1.25*eccentEarthOrbit*eccentEarthOrbit*math.Sin(2.0*degToRad(geomMeanAnomSun)),
	)

	cosHA := (math.Cos(degToRad(90.833)) / (math.Cos(degToRad(lat)) * math.Cos(degToRad(sunDeclin)))) -
		math.Tan(degToRad(lat))*math.Tan(degToRad(sunDeclin))
	if cosHA > 1.0 || cosHA < -1.0 {
		return time.Time{}, time.Time{}, false
	}

	haDeg := radToDeg(math.Acos(cosHA))
	solarNoonMin := 720.0 - 4.0*lon - eqOfTime
	sunriseMin := solarNoonMin - haDeg*4.0
	sunsetMin := solarNoonMin + haDeg*4.0

	dayStart := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	sunrise := dayStart.Add(time.Duration(math.Round(sunriseMin*60)) * time.Second)
	sunset := dayStart.Add(time.Duration(math.Round(sunsetMin*60)) * time.Second)
	return sunrise, sunset, true
}

// IsDay reports whether the sun is above the horizon at timestamp t for (lat, lon).
func IsDay(t time.Time, lat, lon float64) bool {
	utc := t.UTC()
	sr, ss, ok := SunriseSunset(utc.Year(), utc.Month(), utc.Day(), lat, lon)
	if !ok {
		return lat > 0 && (utc.Month() >= time.April && utc.Month() <= time.September)
	}
	return !utc.Before(sr) && utc.Before(ss)
}

func julianDay(t time.Time) float64 {
	return float64(t.Unix())/86400.0 + 2440587.5
}

// VoivodeshipFromTERYT maps the 2-digit Polish TERYT voivodeship prefix to its Polish name.
func VoivodeshipFromTERYT(teryt string) string {
	if len(teryt) < 2 {
		return ""
	}
	switch teryt[:2] {
	case "02":
		return "dolnośląskie"
	case "04":
		return "kujawsko-pomorskie"
	case "06":
		return "lubelskie"
	case "08":
		return "lubuskie"
	case "10":
		return "łódzkie"
	case "12":
		return "małopolskie"
	case "14":
		return "mazowieckie"
	case "16":
		return "opolskie"
	case "18":
		return "podkarpackie"
	case "20":
		return "podlaskie"
	case "22":
		return "pomorskie"
	case "24":
		return "śląskie"
	case "26":
		return "świętokrzyskie"
	case "28":
		return "warmińsko-mazurskie"
	case "30":
		return "wielkopolskie"
	case "32":
		return "zachodniopomorskie"
	default:
		return ""
	}
}
