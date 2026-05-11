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
- `GET /v1/map/trip-live?tripId=...` (or `vehicleId=...`) - trip snapshot for detail screen with `upcoming_stops[]` (`scheduled_time`, nullable `estimated_time`, `eta_minutes`, `is_realtime`)
- `GET /v1/map/eta-between-stops?vehicleId=...&fromStopId=...&toStopId=...` (or `tripId=...`) - ETA to each stop and `between_stops_minutes` based on current upcoming trip segment
- `GET /v1/map/vehicles/live` - WebSocket stream of `vehicle.upsert` events (initial snapshot + incremental updates)
- `POST /v1/vehicle-position` - trusted ingest endpoint to upsert GPS and push live updates to WebSocket clients
- `GET /v1/map/routes-with-live-vehicles` - `{ routes, unassigned_vehicle_count, total_vehicles }` for route picker; add `?includeUnassignedHints=1` (optional `maxUnassignedHints=80`) to append `unassigned_hints` with lat/lon and **heuristic** `possible_route_ids` from the nearest stop (UI hint only). Full positions for all buses (including unassigned) remain on `GET /v1/map/vehicles`
- `GET /v1/map/routes-normalized?routeIds=...` - mobile-optimized relational map payload: `routes`/`stops` dictionaries, `junctions` (`route_id -> stop_id[]`), and `route_geometries` as encoded polylines
- `GET /v1/map/stops-normalized?lat=...&lon=...&limit=...&cursor=...` - stop dictionary (`stops`) plus ordered `stop_ids` for low-overhead map/list rendering
- `GET /v1/map/arrivals-normalized?stopIds=...&limit=500&cursor=...` - stop-indexed, pre-sorted arrivals for list rendering; cursor pagination via `meta.next_cursor`
- `GET /v1/map/arrivals-next?stopIds=...` - one upcoming arrival per stop for lightweight map badges/chips
- `GET /v1/gtfs/routes/{routeId}/shape-encoded` - map-optimized geometry as Google encoded polyline (smaller payload than raw point arrays)
- `GET /v1/realtime/trip-updates?limit=500&cursor=...` - live trip rows from `REALTIME_TRIP_UPDATES_TABLE` (default `trip_updates_current`); cursor pagination via `meta.next_cursor`
- `GET /v1/realtime/alerts?limit=500&cursor=...` - live alert rows from `REALTIME_ALERTS_TABLE` (default `service_alerts_current`); cursor pagination via `meta.next_cursor`
- `GET /v1/routes` - route list from `routes`
- `GET /v1/gtfs/routes/{routeId}/stops?directionId=0&lite=1` - compact stops payload (`stop_ids` + `stops` dictionary) for faster route-stop rendering
- `GET /v1/gtfs/calendar/timetable-lite?date=YYYY-MM-DD&routeId=...` - one lightweight call for route/day timetable rows (trip + first/last times + stop_count)
- `GET /v1/gtfs/calendar/timetable-trip-stops?tripId=...&routeId=...&date=...` - full ordered stop list for one trip (`stop_sequence`, times, names) for calendar detail; merge with `GET /v1/map/trip-live?tripId=...` for live ETAs
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
- `VEHICLE_INGEST_KEY` (optional but recommended) — shared secret for `POST /v1/vehicle-position` via `X-Vehicle-Ingest-Key`

If your ingest uses different table names, set these env vars or create SQL **views** with the default names that `SELECT` from your real tables.

## Run

```bash
go mod tidy
go run ./cmd/server
```

Then open Swagger (you need a Bearer token unless you use the public auth routes first):

- `http://localhost:8080/swagger`

## Realtime vehicle client examples

Ingest a GPS update (trusted caller):

```bash
curl -X POST "http://localhost:8080/v1/vehicle-position" \
  -H "Content-Type: application/json" \
  -H "X-Vehicle-Ingest-Key: ${VEHICLE_INGEST_KEY}" \
  -d '{
    "vehicle_id":"bus-42",
    "trip_id":"trip-123",
    "route_id":"M6",
    "lat":-26.2041,
    "lon":28.0473,
    "bearing":92.0,
    "speed":13.5,
    "updated_at":"2026-05-07T08:31:00Z"
  }'
```

Initial snapshot (REST):

```bash
curl -H "Authorization: Bearer <access_token>" \
  "http://localhost:8080/v1/map/vehicles"
```

React Native WebSocket subscribe/apply example:

```ts
type Vehicle = {
  vehicle_id: string;
  trip_id?: string;
  route_id?: string;
  route_short_name?: string;
  route_long_name?: string;
  route_inferred?: boolean;
  lat: number;
  lon: number;
  bearing?: number;
  speed?: number;
  updated_at: string;
  is_live: boolean;
};

type VehicleUpsertEvent = {
  type: "vehicle.upsert";
  ts: string;
  vehicle: Vehicle;
};

const vehiclesById = new Map<string, Vehicle>();

export function connectVehicleLive(token: string, onChange: (rows: Vehicle[]) => void) {
  const ws = new WebSocket("wss://gtfs-mobile-api-production.up.railway.app/v1/map/vehicles/live", undefined, {
    headers: { Authorization: `Bearer ${token}` },
  });

  ws.onmessage = (e) => {
    const msg = JSON.parse(e.data) as VehicleUpsertEvent;
    if (msg.type !== "vehicle.upsert") return;
    vehiclesById.set(msg.vehicle.vehicle_id, msg.vehicle);
    onChange(Array.from(vehiclesById.values()));
  };

  ws.onerror = () => ws.close();
  return () => ws.close();
}
```

React Native WebSocket with reconnect/backoff (production-safe):

```ts
type Vehicle = {
  vehicle_id: string;
  trip_id?: string;
  route_id?: string;
  route_short_name?: string;
  route_long_name?: string;
  route_inferred?: boolean;
  lat: number;
  lon: number;
  bearing?: number;
  speed?: number;
  updated_at: string;
  is_live: boolean;
};

type VehicleUpsertEvent = {
  type: "vehicle.upsert";
  ts: string;
  vehicle: Vehicle;
};

export function startVehicleLiveFeed(token: string, onChange: (rows: Vehicle[]) => void) {
  const vehiclesById = new Map<string, Vehicle>();
  let ws: WebSocket | null = null;
  let stopped = false;
  let attempt = 0;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;

  const scheduleReconnect = () => {
    if (stopped) return;
    const delayMs = Math.min(30_000, 1_000 * Math.pow(2, attempt)); // 1s,2s,4s...30s cap
    attempt += 1;
    retryTimer = setTimeout(connect, delayMs);
  };

  const connect = () => {
    if (stopped) return;
    ws = new WebSocket("wss://gtfs-mobile-api-production.up.railway.app/v1/map/vehicles/live", undefined, {
      headers: { Authorization: `Bearer ${token}` },
    });

    ws.onopen = () => {
      attempt = 0; // reset backoff after successful connect
    };

    ws.onmessage = (e) => {
      const msg = JSON.parse(e.data) as VehicleUpsertEvent;
      if (msg.type !== "vehicle.upsert") return;
      vehiclesById.set(msg.vehicle.vehicle_id, msg.vehicle);
      onChange(Array.from(vehiclesById.values()));
    };

    ws.onerror = () => {
      ws?.close();
    };

    ws.onclose = () => {
      ws = null;
      scheduleReconnect();
    };
  };

  connect();

  return () => {
    stopped = true;
    if (retryTimer) clearTimeout(retryTimer);
    ws?.close();
  };
}
```

React Native helper for `GET /v1/map/stops-normalized`:

```ts
type StopSummary = {
  stop_id: string;
  id?: string;
  gtfs_stop_id?: string;
  stop_name?: string;
  stop_code?: string;
  lat: number;
  lon: number;
};

type StopsNormalizedPayload = {
  stops: Record<string, StopSummary>;
  stop_ids: string[];
};

type StopsNormalizedResponse = {
  status: { mode: "normal" | "static_only"; last_successful_sync?: string };
  data: StopsNormalizedPayload;
  meta?: { total?: number; limit?: number; next_cursor?: string };
};

export async function fetchStopsNormalized(params: {
  baseUrl: string;
  token: string;
  lat: number;
  lon: number;
  radius?: number;
  limit?: number;
  cursor?: string;
  search?: string;
  routeIds?: string;
  routeIdsKind?: "route_id" | "short_name" | "short";
}) {
  const qs = new URLSearchParams({
    lat: String(params.lat),
    lon: String(params.lon),
    ...(params.radius ? { radius: String(params.radius) } : {}),
    ...(params.limit ? { limit: String(params.limit) } : {}),
    ...(params.cursor ? { cursor: params.cursor } : {}),
    ...(params.search ? { search: params.search } : {}),
    ...(params.routeIds ? { routeIds: params.routeIds } : {}),
    ...(params.routeIdsKind ? { routeIdsKind: params.routeIdsKind } : {}),
  });

  const res = await fetch(`${params.baseUrl}/v1/map/stops-normalized?${qs.toString()}`, {
    headers: { Authorization: `Bearer ${params.token}` },
  });
  if (!res.ok) throw new Error(`stops-normalized failed: ${res.status}`);
  const json = (await res.json()) as StopsNormalizedResponse;

  // Render-ready ordered list with O(1) dictionary retained for lookups.
  const orderedStops = json.data.stop_ids
    .map((id) => json.data.stops[id])
    .filter(Boolean);

  return {
    byId: json.data.stops,
    ordered: orderedStops,
    nextCursor: json.meta?.next_cursor ?? "",
  };
}
```

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
