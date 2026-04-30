# Backend Mobile App (Go + Supabase)

Thin Go API layer for mobile clients that reads GTFS static + realtime data from Supabase and returns stripped, mobile-friendly JSON.

## Endpoints

- `GET /v1/map/vehicles` - active vehicles from `vehicle_positions_current`
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

## Run

```bash
go mod tidy
go run ./cmd/server
```

Then open:

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
