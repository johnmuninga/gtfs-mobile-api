-- Precompute route <-> stop relationships for faster route-filtered stop searches.
-- Refresh when GTFS static data changes.
CREATE MATERIALIZED VIEW IF NOT EXISTS mv_route_stop_map AS
SELECT DISTINCT
    t.route_id,
    st.stop_id
FROM trips t
JOIN stop_times st ON st.trip_id = t.trip_id
WHERE t.route_id IS NOT NULL
  AND t.route_id <> ''
  AND st.stop_id IS NOT NULL
  AND st.stop_id <> '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_mv_route_stop_map_route_stop
ON mv_route_stop_map (route_id, stop_id);

CREATE INDEX IF NOT EXISTS idx_mv_route_stop_map_stop
ON mv_route_stop_map (stop_id);

-- Extra indexes for hot map/shape/departure queries.
CREATE INDEX IF NOT EXISTS idx_trips_route_direction_shape
ON trips (route_id, direction_id, shape_id, trip_id);

CREATE INDEX IF NOT EXISTS idx_stop_times_trip_sequence_stop
ON stop_times (trip_id, stop_sequence, stop_id);

-- For periodic refresh jobs:
-- REFRESH MATERIALIZED VIEW CONCURRENTLY mv_route_stop_map;

ANALYZE trips;
ANALYZE stop_times;
ANALYZE mv_route_stop_map;
