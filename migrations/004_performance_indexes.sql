-- =========================
-- 004_performance_indexes.sql
-- Indexes that match the query patterns used by the Go mobile API.
-- Apply this once on your Supabase database.
-- =========================

CREATE EXTENSION IF NOT EXISTS postgis;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ----- stops -----
CREATE INDEX IF NOT EXISTS idx_stops_geog
ON stops
USING gist ((ST_SetSRID(ST_MakePoint(stop_lon, stop_lat), 4326)::geography));

CREATE INDEX IF NOT EXISTS idx_stops_lower_name
ON stops (LOWER(stop_name) text_pattern_ops);

CREATE INDEX IF NOT EXISTS idx_stops_lower_name_trgm
ON stops
USING gin (LOWER(stop_name) gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_stops_lower_code
ON stops (LOWER(stop_code) text_pattern_ops);

-- ----- routes -----
CREATE INDEX IF NOT EXISTS idx_routes_lower_short_name
ON routes (LOWER(route_short_name) text_pattern_ops);

CREATE INDEX IF NOT EXISTS idx_routes_lower_long_name_trgm
ON routes
USING gin (LOWER(route_long_name) gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_routes_type
ON routes (route_type);

-- ----- trips -----
CREATE INDEX IF NOT EXISTS idx_trips_route_id
ON trips (route_id);

CREATE INDEX IF NOT EXISTS idx_trips_service_id
ON trips (service_id);

CREATE INDEX IF NOT EXISTS idx_trips_route_service
ON trips (route_id, service_id);

-- ----- stop_times -----
-- (idx_stop_times_trip already exists; we need stop_id for stop departures)
CREATE INDEX IF NOT EXISTS idx_stop_times_stop_id
ON stop_times (stop_id);

CREATE INDEX IF NOT EXISTS idx_stop_times_stop_arrival
ON stop_times (stop_id, arrival_time);

-- ----- calendar -----
CREATE INDEX IF NOT EXISTS idx_calendar_start_end
ON calendar (start_date, end_date);

-- ----- calendar_dates -----
CREATE INDEX IF NOT EXISTS idx_calendar_dates_date
ON calendar_dates (date);

CREATE INDEX IF NOT EXISTS idx_calendar_dates_service_date
ON calendar_dates (service_id, date);

-- ----- realtime trip updates -----
-- (idx_trip_update_stops_trip already exists; we filter by stop too)
CREATE INDEX IF NOT EXISTS idx_trip_update_stops_trip_stop
ON trip_update_stop_times_current (trip_id, stop_id);

-- ----- vehicle positions -----
CREATE INDEX IF NOT EXISTS idx_vehicle_positions_route
ON vehicle_positions_current (route_id);

CREATE INDEX IF NOT EXISTS idx_vehicle_positions_trip
ON vehicle_positions_current (trip_id);

-- Optional analyze to refresh planner statistics after creating indexes
ANALYZE stops;
ANALYZE routes;
ANALYZE trips;
ANALYZE stop_times;
ANALYZE calendar;
ANALYZE calendar_dates;
ANALYZE trip_update_stop_times_current;
ANALYZE vehicle_positions_current;
