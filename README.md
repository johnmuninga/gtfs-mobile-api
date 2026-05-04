# Backend Mobile App (Go + Supabase)

Thin Go API layer for mobile clients that reads GTFS static + realtime data from Supabase and returns stripped, mobile-friendly JSON.

## Authentication

All **`/v1/*`** routes require `Authorization: Bearer <supabase_access_token>` except:

- `POST /v1/auth/signup`
- `POST /v1/auth/login`
- `POST /v1/auth/verify-otp`

**Docs (no token):** `GET /`, `GET /swagger`, and `GET /openapi.yaml` so you can open Swagger in the browser, run login/signup, then use **Authorize** with the returned `access_token` for every other call.

**Health:** `GET /healthz` still requires a Bearer token unless you configure `HEALTHCHECK_SECRET` and send `X-Healthcheck-Secret`.

Optional: set `HEALTHCHECK_SECRET` in the environment so uptime checks can call `GET /healthz` with header `X-Healthcheck-Secret: <same value>` without a user JWT.

Access control is not a substitute for safe SQL: this service uses parameterized queries against Postgres to avoid SQL injection.

## Endpoints

- `GET /v1/map/vehicles` - active vehicles; canonical `route_id` (realtime row or `trips.route_id`), plus `route_short_name` / `route_long_name` from static `routes` when the route exists
- `GET /v1/map/routes-with-live-vehicles` - `{ routes, unassigned_vehicle_count, total_vehicles }` for route picker; add `?includeUnassignedHints=1` (optional `maxUnassignedHints=80`) to append `unassigned_hints` with lat/lon and **heuristic** `possible_route_ids` from the nearest stop (UI hint only). Full positions for all buses (including unassigned) remain on `GET /v1/map/vehicles`
- `GET /v1/realtime/trip-updates?limit=500` - live trip rows from `REALTIME_TRIP_UPDATES_TABLE` (default `trip_updates_current`); `data` is a JSON array matching your DB columns
- `GET /v1/realtime/alerts?limit=500` - live alert rows from `REALTIME_ALERTS_TABLE` (default `service_alerts_current`); `data` is a JSON array matching your DB columns
- `GET /v1/routes` - route list from `routes`
- `GET /v1/stops/{id}/schedule` - next 5 departures, realtime fallback to static
- `GET /v1/stops/nearby?lat={lat}&lon={lon}&radius_meters={r}` - nearby stop search
- `GET /swagger` - Swagger UI
- `GET /openapi.yaml` - OpenAPI spec

Each endpoint returns:

```json
{
  "status": {
    "mode": "normal | static_only",
    "last_successful_sync": "2026-04-28T14:00:00Z"
  },
  "data": []
}
```

`mode` becomes `static_only` if `feed_state.last_successful_sync` is older than `API_DEGRADED_MINUTES` (default 15 minutes).

## Required Environment Variables

Set:

- `SUPABASE_DB_URL` (required)
- `PORT` (default `8080`)
- `MAX_DB_CONNS` (default `20`)
- `API_DEGRADED_MINUTES` (default `15`)
- `NEARBY_DEFAULT_RADIUS_METERS` (default `1000`)
- `HEALTHCHECK_SECRET` (optional, for `GET /healthz` probes without a user token)
- `REALTIME_TRIP_UPDATES_TABLE` (optional, default `trip_updates_current`) — Postgres table or view for live trip updates
- `REALTIME_ALERTS_TABLE` (optional, default `service_alerts_current`) — Postgres table or view for service alerts

If your ingest uses different table names, set these env vars or create SQL **views** with the default names that `SELECT` from your real tables.

## Run

```bash
go mod tidy
go run ./cmd/server
```

Then open Swagger (you need a Bearer token unless you use the public auth routes first):

- `http://localhost:8080/swagger`

## Recommended SQL indexes

```sql
create extension if not exists postgis;

create index if not exists idx_vehicle_positions_current_vehicle_id
on vehicle_positions_current (vehicle_id);

create index if not exists idx_stop_times_stop_id_arrival
on stop_times (stop_id, arrival_time);

create index if not exists idx_trip_update_stop_times_current_trip_stop
on trip_update_stop_times_current (trip_id, stop_id);

create index if not exists idx_stops_geog
on stops using gist ((st_setsrid(st_makepoint(stop_lon, stop_lat), 4326)::geography));
```

## Materialized view refresh

`migrations/008_route_stop_map_and_hot_indexes.sql` adds `mv_route_stop_map` used by route-filtered stop queries.
After GTFS static imports, refresh it:

```sql
REFRESH MATERIALIZED VIEW CONCURRENTLY mv_route_stop_map;
```

If the view is not present yet, API queries automatically fall back to the base `trips + stop_times` join.
