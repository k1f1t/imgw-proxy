# IMGW Open-Meteo Proxy (Self-Hosted)

A lightweight, cached, **self-hosted** Go + SQLite proxy for Polish IMGW-PIB meteorological and hydrological servers that exposes a strict **Open-Meteo compatible `/v1/forecast` API** alongside dedicated `/v1/hydro`, `/v1/warnings`, and `/v1/stations` endpoints.

---

## Źródło danych i warunki prawne / Attribution & Legal Notice

> **„Źródłem pochodzenia danych jest Instytut Meteorologii i Gospodarki Wodnej – Państwowy Instytut Badawczy”**
>
> **„Dane Instytutu Meteorologii i Gospodarki Wodnej – Państwowego Instytutu Badawczego zostały przetworzone”**

### Self-Hosted / Private Use Only

Per the IMGW-PIB Data Sharing Terms (`Regulamin Udostępniania Danych IMGW-PIB`, § 3 and § 5):
- Free access to IMGW-PIB core-network data is permitted for **private, non-commercial use** (and High-Value Datasets per EU Regulation 2023/138).
- **Do not host this proxy as a public or commercial API** without a signed agreement with IMGW-PIB (`biznes@imgw.pl`).
- By default, the server binds to `127.0.0.1:8080`, disables CORS, and supports optional `API_KEY` authentication so your instance remains private on your LAN/homelab.
- Any downstream dashboard, Home Assistant card, or publication using data from this proxy must preserve the Polish source attribution above.

---

## Features

- **Strict Open-Meteo `/v1/forecast` compatibility** for Home Assistant and Open-Meteo clients:
  - Single & comma-separated multi-location `latitude`/`longitude` queries (returns JSON object or array).
  - Supports `current`, legacy `current_weather=true`, `hourly`, and `daily` variable lists.
  - Full unit conversion: `temperature_unit` (`celsius`, `fahrenheit`), `wind_speed_unit` (`kmh` default, `ms`, `mph`, `kn`), `precipitation_unit` (`mm`, `inch`), `timeformat` (`iso8601`, `unixtime`), and `timezone` (`GMT`, `UTC`, `auto` → `Europe/Warsaw`, or IANA names).
  - Synthesizes Open-Meteo fields not directly present in raw IMGW observations: WMO `weather_code` (from IMGW icon codes like `n4z61d` or measurements), Steadman `apparent_temperature`, `dew_point_2m`, NOAA solar `sunrise`/`sunset`/`daylight_duration`, and `is_day`.
  - Merges cached SQLite `synop` + `meteo` observation history (`past_days` up to 90 days) with high-resolution `meteo.imgw.pl` short-term (`HYBRID`: INCA + AROME) and medium-term (`GFS`) forecasts.
- **Zero-dependency embedded SQLite cache** (`modernc.org/sqlite`, WAL mode):
  - Deduplicates observations, persists history across restarts, prunes old rows automatically (`RETENTION_DAYS=90`), and serves stale cached data if IMGW upstream endpoints are temporarily unreachable.
- **Dual Forecast Collection Modes** (polite to IMGW servers):
  - `FORECAST_MODE=selected`: Polls forecasts *only* for your configured `FORECAST_LOCATIONS` on schedule — zero IMGW traffic on client requests.
  - `FORECAST_MODE=all`: Staggered background polling for all 62 IMGW synoptic stations plus spatial-grid (`0.05°`) on-demand TTL caching with singleflight deduplication.
- **Dedicated IMGW endpoints**:
  - `GET /v1/hydro` — Hydrological stations, water levels, warning/alarm threshold flags, flow, water temperature, ice & vegetation phenomena.
  - `GET /v1/warnings` — Active meteorological (by TERYT or `latitude`/`longitude`) and hydrological warnings (by voivodeship or `latitude`/`longitude`).
  - `GET /v1/stations` — Cached `synop`, `meteo`, and `hydro` stations with coordinates, elevation, TERYT, and optional distance sorting.
  - `GET /v1/attribution` — Required IMGW-PIB legal attribution strings.
  - `GET /healthz` — Poller status, last successful sync timestamps, and SQLite row counts.

---

## Quick Start

### Option 1: Docker Compose (Recommended)

```bash
# Selected locations mode (default in docker-compose.yml: Warszawa, Kraków, Gdańsk)
docker compose up -d --build

# Or with API key and custom locations:
API_KEY="my-secret-key" \
FORECAST_MODE="selected" \
FORECAST_LOCATIONS="Warszawa:52.2297,21.0122;12566" \
docker compose up -d --build
```

### Option 2: Run Go Binary Directly

```bash
go build -o imgw-openmeteo-proxy ./cmd/server

# Selected mode (Warszawa station 12375 + custom coordinates)
FORECAST_MODE=selected \
FORECAST_LOCATIONS="12375;Home:52.23,21.01" \
./imgw-openmeteo-proxy

# Or using a YAML config file:
cp config.example.yaml config.yaml
CONFIG_FILE=./config.yaml ./imgw-openmeteo-proxy
```

---

## Configuration

All settings can be configured via `CONFIG_FILE` (YAML) or environment variables (env vars take precedence):

| Environment Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `127.0.0.1:8080` | HTTP bind address |
| `DB_PATH` | `./data/imgw.db` | SQLite database file path |
| `API_KEY` | *(empty)* | Optional auth key for `/v1/*` (`Authorization: Bearer`, `X-API-Key`, or `?apikey=`) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `SYNOP_POLL_INTERVAL` | `20m` | Polling interval for `/api/data/synop` |
| `METEO_POLL_INTERVAL` | `10m` | Polling interval for `/api/data/meteo` |
| `HYDRO_POLL_INTERVAL` | `15m` | Polling interval for `/api/data/hydro` |
| `WARNINGS_POLL_INTERVAL` | `10m` | Polling interval for `/api/data/warningsmeteo` & `warningshydro` |
| `FORECAST_MODE` | `all` | `all` (all 62 synoptic stations + on-demand TTL) or `selected` (only `FORECAST_LOCATIONS`) |
| `FORECAST_LOCATIONS` | *(empty)* | Semicolon-separated list for `selected` mode: `12375;52.23,21.01;Home:50.06,19.94` |
| `FORECAST_POLL_INTERVAL` | `2h` | Scheduled forecast refresh cadence |
| `FORECAST_TTL` | `2h` | Cache freshness TTL for stored forecasts |
| `FORECAST_REQUEST_DELAY` | `750ms` | Delay between individual station forecast fetches |
| `FORECAST_ALLOW_ONDEMAND` | `true` (`all`) / `false` (`selected`) | Allow uncached client lat/lon requests to trigger an on-demand cached fetch |
| `FORECAST_MODEL` | `hybrid` | `meteo.imgw.pl` short-term model (`hybrid`, `alaro`, `arome`) |
| `RETENTION_DAYS` | `90` | Days of historical observations kept in SQLite |
| `UPSTREAM_TIMEOUT` | `15s` | HTTP client timeout per upstream IMGW call |

---

## API Examples

### Open-Meteo Compatible Forecast (`/v1/forecast`)

```bash
curl "http://127.0.0.1:8080/v1/forecast?latitude=52.23&longitude=21.01&timezone=auto&current=temperature_2m,relative_humidity_2m,apparent_temperature,precipitation,weather_code,wind_speed_10m,wind_direction_10m,wind_gusts_10m,surface_pressure,is_day&hourly=temperature_2m,precipitation,weather_code,wind_speed_10m&daily=weather_code,temperature_2m_max,temperature_2m_min,precipitation_sum,sunrise,sunset&past_days=1&forecast_days=5"
```

With `API_KEY` enabled:
```bash
curl -H "Authorization: Bearer my-secret-key" \
  "http://127.0.0.1:8080/v1/forecast?latitude=52.23&longitude=21.01&current_weather=true"
```

### Hydrological Observations (`/v1/hydro`)

```bash
# Nearest river gauge to coordinates
curl "http://127.0.0.1:8080/v1/hydro?latitude=52.23&longitude=21.01&limit=3"

# Filter by river or station_id
curl "http://127.0.0.1:8080/v1/hydro?river=Wisła&limit=10"
```

### Active IMGW Warnings (`/v1/warnings`)

```bash
# Warnings matching nearest station's TERYT & voivodeship
curl "http://127.0.0.1:8080/v1/warnings?latitude=50.06&longitude=19.94"

# Explicit TERYT code or warning type
curl "http://127.0.0.1:8080/v1/warnings?type=meteo&teryt=1465"
```

### Stations & Health

```bash
curl "http://127.0.0.1:8080/v1/stations?source=synop&latitude=52.23&longitude=21.01&limit=5"
curl "http://127.0.0.1:8080/v1/attribution"
curl "http://127.0.0.1:8080/healthz"
```

---

## Upstream Data Sources

- Official IMGW-PIB Public Data API (`https://danepubliczne.imgw.pl/api/data/`): `/synop`, `/meteo`, `/hydro`, `/warningsmeteo`, `/warningshydro`.
- Official IMGW-PIB Weather Portal (`https://meteo.imgw.pl/api/v1/forecast/`): `/fcapi` (short-term HYBRID / ALARO / AROME) and `/forecastapi` (medium-term GFS).
