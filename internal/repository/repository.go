package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"backend_mobile_app_go/internal/models"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// GetAPIStatus returns the freshest sync time across all feeds and decides
// whether the API is in degraded ("static_only") mode.
func (r *Repository) GetAPIStatus(ctx context.Context, degradedAfterMinutes int) (models.APIStatus, error) {
	const q = `SELECT MAX(last_successful_sync) FROM feed_state`

	var lastSync *time.Time
	if err := r.pool.QueryRow(ctx, q).Scan(&lastSync); err != nil {
		return models.APIStatus{}, fmt.Errorf("query feed_state: %w", err)
	}

	status := models.APIStatus{
		Mode:               models.ModeNormal,
		LastSuccessfulSync: lastSync,
	}
	if lastSync == nil || time.Since(*lastSync) > time.Duration(degradedAfterMinutes)*time.Minute {
		status.Mode = models.ModeStaticOnly
	}
	return status, nil
}

// ============================================================================
// Stops
// ============================================================================

type StopSearchParams struct {
	Search       string
	Lat          *float64
	Lon          *float64
	RadiusMeters int
	Limit        int
	Page         int
	RouteIDs     []string // optional: only stops that appear on these routes (via stop_times/trips)
}

func (r *Repository) SearchStops(ctx context.Context, p StopSearchParams) ([]models.StopSummary, int, error) {
	return r.searchStops(ctx, p, true)
}

func (r *Repository) searchStops(ctx context.Context, p StopSearchParams, useRouteStopMV bool) ([]models.StopSummary, int, error) {
	var (
		filters []string
		args    []any
		lonIdx  int
		latIdx  int
	)

	if p.Search != "" {
		args = append(args, "%"+strings.ToLower(p.Search)+"%")
		filters = append(filters, fmt.Sprintf("(LOWER(stop_name) LIKE $%d OR LOWER(stop_code) LIKE $%d)", len(args), len(args)))
	}

	useGeo := p.Lat != nil && p.Lon != nil
	if useGeo {
		args = append(args, *p.Lon)
		lonIdx = len(args)
		args = append(args, *p.Lat)
		latIdx = len(args)

		if p.RadiusMeters > 0 {
			args = append(args, p.RadiusMeters)
			filters = append(filters, fmt.Sprintf(
				"ST_DWithin(ST_SetSRID(ST_MakePoint(stop_lon, stop_lat),4326)::geography, ST_SetSRID(ST_MakePoint($%d,$%d),4326)::geography, $%d)",
				lonIdx, latIdx, len(args),
			))
		}
	}

	if len(p.RouteIDs) > 0 {
		args = append(args, p.RouteIDs)
		if useRouteStopMV {
			filters = append(filters, fmt.Sprintf(
				`EXISTS (
					SELECT 1
					FROM mv_route_stop_map rsm
					WHERE rsm.stop_id = stops.stop_id
					  AND rsm.route_id = ANY($%d::text[])
				)`,
				len(args),
			))
		} else {
			filters = append(filters, fmt.Sprintf(
				`EXISTS (
					SELECT 1 FROM stop_times st
					JOIN trips t ON t.trip_id = st.trip_id
					WHERE st.stop_id = stops.stop_id AND t.route_id = ANY($%d::text[])
				)`,
				len(args),
			))
		}
	}

	where := ""
	if len(filters) > 0 {
		where = "WHERE " + strings.Join(filters, " AND ")
	}

	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM stops %s`, where)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		if useRouteStopMV && len(p.RouteIDs) > 0 && isUndefinedTableErr(err, "mv_route_stop_map") {
			return r.searchStops(ctx, p, false)
		}
		return nil, 0, fmt.Errorf("count search stops: %w", err)
	}

	orderBy := "ORDER BY stop_name NULLS LAST"
	if useGeo {
		orderBy = fmt.Sprintf(
			"ORDER BY ST_Distance(ST_SetSRID(ST_MakePoint(stop_lon, stop_lat),4326)::geography, ST_SetSRID(ST_MakePoint($%d,$%d),4326)::geography)",
			lonIdx, latIdx,
		)
	}

	offset := (p.Page - 1) * p.Limit
	args = append(args, p.Limit, offset)

	q := fmt.Sprintf(`
SELECT stop_id, COALESCE(stop_name,''), COALESCE(stop_code,''), COALESCE(stop_lat,0), COALESCE(stop_lon,0)
FROM stops
%s
%s
LIMIT $%d OFFSET $%d
`, where, orderBy, len(args)-1, len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		if useRouteStopMV && len(p.RouteIDs) > 0 && isUndefinedTableErr(err, "mv_route_stop_map") {
			return r.searchStops(ctx, p, false)
		}
		return nil, 0, fmt.Errorf("query search stops: %w", err)
	}
	defer rows.Close()

	out := make([]models.StopSummary, 0, p.Limit)
	for rows.Next() {
		var s models.StopSummary
		if err = rows.Scan(&s.StopID, &s.StopName, &s.StopCode, &s.Lat, &s.Lon); err != nil {
			return nil, 0, fmt.Errorf("scan stop: %w", err)
		}
		out = append(out, s)
	}
	return out, total, rows.Err()
}

func isUndefinedTableErr(err error, relation string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42P01" && (relation == "" || strings.Contains(pgErr.Message, relation))
	}
	return false
}

// ListAllStops returns every stop as a lightweight summary.
// Intended for the in-memory snapshot loaded at startup.
func (r *Repository) ListAllStops(ctx context.Context) ([]models.StopSummary, error) {
	const q = `
SELECT stop_id, COALESCE(stop_name,''), COALESCE(stop_code,''), COALESCE(stop_lat,0), COALESCE(stop_lon,0)
FROM stops
WHERE stop_lat IS NOT NULL AND stop_lon IS NOT NULL
`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query all stops: %w", err)
	}
	defer rows.Close()

	out := make([]models.StopSummary, 0, 4096)
	for rows.Next() {
		var s models.StopSummary
		if err = rows.Scan(&s.StopID, &s.StopName, &s.StopCode, &s.Lat, &s.Lon); err != nil {
			return nil, fmt.Errorf("scan stop: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListAllRoutes returns every route, ordered.
// Intended for the in-memory snapshot loaded at startup.
func (r *Repository) ListAllRoutes(ctx context.Context) ([]models.Route, error) {
	const q = `
SELECT route_id,
	COALESCE(agency_id,''),
	COALESCE(route_short_name,''),
	COALESCE(route_long_name,''),
	COALESCE(route_type,''),
	COALESCE(route_color,''),
	COALESCE(route_text_color,'')
FROM routes
ORDER BY route_short_name NULLS LAST, route_long_name NULLS LAST
`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query all routes: %w", err)
	}
	defer rows.Close()

	out := make([]models.Route, 0, 256)
	for rows.Next() {
		var rt models.Route
		if err = rows.Scan(&rt.RouteID, &rt.AgencyID, &rt.ShortName, &rt.LongName, &rt.RouteType, &rt.RouteColor, &rt.RouteTextColor); err != nil {
			return nil, fmt.Errorf("scan route: %w", err)
		}
		out = append(out, rt)
	}
	return out, rows.Err()
}

func (r *Repository) GetStop(ctx context.Context, stopID string) (*models.Stop, error) {
	const q = `
SELECT stop_id,
	COALESCE(stop_code,''), COALESCE(stop_name,''), COALESCE(stop_desc,''),
	COALESCE(stop_lat,0), COALESCE(stop_lon,0),
	COALESCE(zone_id,''), COALESCE(location_type,''),
	COALESCE(parent_station,''), COALESCE(wheelchair_boarding,''),
	COALESCE(platform_code,'')
FROM stops
WHERE stop_id = $1
`
	var s models.Stop
	err := r.pool.QueryRow(ctx, q, stopID).Scan(
		&s.StopID, &s.StopCode, &s.StopName, &s.StopDesc, &s.Lat, &s.Lon,
		&s.ZoneID, &s.LocationType, &s.ParentStation, &s.WheelchairBoarding, &s.PlatformCode,
	)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetStopDepartures returns scheduled departures for a stop within [at, at+window].
// Handles GTFS times stored as TEXT, including > 24:00:00 (next-day services).
func (r *Repository) GetStopDepartures(ctx context.Context, stopID string, at time.Time, windowMinutes, limit int) ([]models.Departure, error) {
	const q = `
WITH at_ts AS (
	SELECT $1::timestamptz AS at
),
candidate_dates AS (
	SELECT (at::date)     AS d FROM at_ts
	UNION SELECT (at::date - INTERVAL '1 day')::date FROM at_ts
),
active_services AS (
	SELECT cd.d AS service_date, c.service_id
	FROM candidate_dates cd
	JOIN calendar c
	  ON to_date(c.start_date, 'YYYYMMDD') <= cd.d
	 AND to_date(c.end_date,   'YYYYMMDD') >= cd.d
	 AND CASE EXTRACT(ISODOW FROM cd.d)
			WHEN 1 THEN c.monday
			WHEN 2 THEN c.tuesday
			WHEN 3 THEN c.wednesday
			WHEN 4 THEN c.thursday
			WHEN 5 THEN c.friday
			WHEN 6 THEN c.saturday
			WHEN 7 THEN c.sunday
		 END = '1'
	WHERE NOT EXISTS (
		SELECT 1 FROM calendar_dates ex
		WHERE ex.service_id = c.service_id
		  AND ex.date = to_char(cd.d, 'YYYYMMDD')
		  AND ex.exception_type = '2'
	)
	UNION
	SELECT cd.d AS service_date, ex.service_id
	FROM candidate_dates cd
	JOIN calendar_dates ex
	  ON ex.date = to_char(cd.d, 'YYYYMMDD')
	 AND ex.exception_type = '1'
),
active_services_dedup AS (
	SELECT DISTINCT service_date, service_id FROM active_services
),
candidates AS (
	SELECT DISTINCT ON (st.trip_id, a.service_date)
		st.trip_id,
		t.route_id,
		t.trip_headsign,
		a.service_date,
		(a.service_date::timestamp + st.arrival_time::interval) AT TIME ZONE current_setting('TimeZone') AS scheduled_time
	FROM stop_times st
	JOIN trips t ON t.trip_id = st.trip_id
	JOIN active_services_dedup a ON a.service_id = t.service_id
	WHERE st.stop_id = $2
	ORDER BY st.trip_id, a.service_date
)
SELECT
	c.trip_id,
	c.route_id,
	COALESCE(r.route_short_name, ''),
	COALESCE(c.trip_headsign, ''),
	c.scheduled_time,
	COALESCE(
		c.scheduled_time + (rt.arrival_delay || ' seconds')::interval,
		c.scheduled_time
	) AS estimated_time,
	(rt.trip_id IS NOT NULL) AS is_realtime
FROM candidates c
LEFT JOIN routes r ON r.route_id = c.route_id
LEFT JOIN LATERAL (
	SELECT rt_inner.trip_id, rt_inner.arrival_delay
	FROM trip_update_stop_times_current rt_inner
	WHERE rt_inner.trip_id = c.trip_id AND rt_inner.stop_id = $2
	LIMIT 1
) rt ON true
WHERE c.scheduled_time >= (SELECT at FROM at_ts)
  AND c.scheduled_time <= (SELECT at FROM at_ts) + ($3::int || ' minutes')::interval
ORDER BY c.scheduled_time
LIMIT $4
`

	rows, err := r.pool.Query(ctx, q, at, stopID, windowMinutes, limit)
	if err != nil {
		return nil, fmt.Errorf("query stop departures: %w", err)
	}
	defer rows.Close()

	out := make([]models.Departure, 0, limit)
	for rows.Next() {
		var d models.Departure
		if err = rows.Scan(
			&d.TripID, &d.RouteID, &d.RouteShortName, &d.Headsign,
			&d.ScheduledTime, &d.EstimatedTime, &d.IsRealtime,
		); err != nil {
			return nil, fmt.Errorf("scan departure: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetNormalizedArrivals returns flattened rows for multiple stops sorted by scheduled_time, then stop_id/trip_id.
// offset is applied on the flattened stream so the API can paginate with cursor tokens.
func (r *Repository) GetNormalizedArrivals(ctx context.Context, stopIDs []string, at time.Time, windowMinutes, limit, offset int) ([]string, []models.StopArrivalLite, error) {
	if len(stopIDs) == 0 {
		return nil, nil, nil
	}
	if limit <= 0 {
		limit = 250
	}
	if limit > 2000 {
		limit = 2000
	}
	if offset < 0 {
		offset = 0
	}
	const q = `
WITH at_ts AS (
	SELECT $1::timestamptz AS at
),
candidate_dates AS (
	SELECT (at::date) AS d FROM at_ts
	UNION SELECT (at::date - INTERVAL '1 day')::date FROM at_ts
),
active_services AS (
	SELECT cd.d AS service_date, c.service_id
	FROM candidate_dates cd
	JOIN calendar c
	  ON to_date(c.start_date, 'YYYYMMDD') <= cd.d
	 AND to_date(c.end_date,   'YYYYMMDD') >= cd.d
	 AND CASE EXTRACT(ISODOW FROM cd.d)
			WHEN 1 THEN c.monday
			WHEN 2 THEN c.tuesday
			WHEN 3 THEN c.wednesday
			WHEN 4 THEN c.thursday
			WHEN 5 THEN c.friday
			WHEN 6 THEN c.saturday
			WHEN 7 THEN c.sunday
		 END = '1'
	WHERE NOT EXISTS (
		SELECT 1 FROM calendar_dates ex
		WHERE ex.service_id = c.service_id
		  AND ex.date = to_char(cd.d, 'YYYYMMDD')
		  AND ex.exception_type = '2'
	)
	UNION
	SELECT cd.d AS service_date, ex.service_id
	FROM candidate_dates cd
	JOIN calendar_dates ex
	  ON ex.date = to_char(cd.d, 'YYYYMMDD')
	 AND ex.exception_type = '1'
),
active_services_dedup AS (
	SELECT DISTINCT service_date, service_id FROM active_services
),
candidates AS (
	SELECT DISTINCT ON (st.stop_id, st.trip_id, a.service_date)
		st.stop_id,
		st.trip_id,
		t.route_id,
		t.trip_headsign,
		a.service_date,
		(a.service_date::timestamp + st.arrival_time::interval) AT TIME ZONE current_setting('TimeZone') AS scheduled_time
	FROM stop_times st
	JOIN trips t ON t.trip_id = st.trip_id
	JOIN active_services_dedup a ON a.service_id = t.service_id
	WHERE st.stop_id = ANY($2::text[])
	ORDER BY st.stop_id, st.trip_id, a.service_date
)
SELECT
	c.stop_id,
	c.trip_id,
	c.route_id,
	COALESCE(r.route_short_name, ''),
	COALESCE(c.trip_headsign, ''),
	c.scheduled_time,
	COALESCE(
		c.scheduled_time + (rt.arrival_delay || ' seconds')::interval,
		c.scheduled_time
	) AS estimated_time,
	(rt.trip_id IS NOT NULL) AS is_realtime
FROM candidates c
LEFT JOIN routes r ON r.route_id = c.route_id
LEFT JOIN LATERAL (
	SELECT rt_inner.trip_id, rt_inner.arrival_delay
	FROM trip_update_stop_times_current rt_inner
	WHERE rt_inner.trip_id = c.trip_id AND rt_inner.stop_id = c.stop_id
	LIMIT 1
) rt ON true
WHERE c.scheduled_time >= (SELECT at FROM at_ts)
  AND c.scheduled_time <= (SELECT at FROM at_ts) + ($3::int || ' minutes')::interval
ORDER BY c.scheduled_time, c.stop_id, c.trip_id
LIMIT $4 OFFSET $5
`
	rows, err := r.pool.Query(ctx, q, at, stopIDs, windowMinutes, limit, offset)
	if err != nil {
		return nil, nil, fmt.Errorf("query normalized arrivals: %w", err)
	}
	defer rows.Close()

	stopOut := make([]string, 0, limit)
	arrOut := make([]models.StopArrivalLite, 0, limit)
	for rows.Next() {
		var (
			stopID string
			a      models.StopArrivalLite
		)
		if err = rows.Scan(
			&stopID, &a.TripID, &a.RouteID, &a.RouteShortName, &a.Headsign,
			&a.ScheduledTime, &a.EstimatedTime, &a.IsRealtime,
		); err != nil {
			return nil, nil, fmt.Errorf("scan normalized arrival: %w", err)
		}
		stopOut = append(stopOut, stopID)
		arrOut = append(arrOut, a)
	}
	return stopOut, arrOut, rows.Err()
}

// GetNextArrivalsPerStop returns at most one next arrival row per stop_id in stopIDs.
func (r *Repository) GetNextArrivalsPerStop(ctx context.Context, stopIDs []string, at time.Time, windowMinutes int) (map[string]models.StopArrivalLite, error) {
	if len(stopIDs) == 0 {
		return map[string]models.StopArrivalLite{}, nil
	}
	const q = `
WITH at_ts AS (
	SELECT $1::timestamptz AS at
),
candidate_dates AS (
	SELECT (at::date) AS d FROM at_ts
	UNION SELECT (at::date - INTERVAL '1 day')::date FROM at_ts
),
active_services AS (
	SELECT cd.d AS service_date, c.service_id
	FROM candidate_dates cd
	JOIN calendar c
	  ON to_date(c.start_date, 'YYYYMMDD') <= cd.d
	 AND to_date(c.end_date,   'YYYYMMDD') >= cd.d
	 AND CASE EXTRACT(ISODOW FROM cd.d)
			WHEN 1 THEN c.monday
			WHEN 2 THEN c.tuesday
			WHEN 3 THEN c.wednesday
			WHEN 4 THEN c.thursday
			WHEN 5 THEN c.friday
			WHEN 6 THEN c.saturday
			WHEN 7 THEN c.sunday
		 END = '1'
	WHERE NOT EXISTS (
		SELECT 1 FROM calendar_dates ex
		WHERE ex.service_id = c.service_id
		  AND ex.date = to_char(cd.d, 'YYYYMMDD')
		  AND ex.exception_type = '2'
	)
	UNION
	SELECT cd.d AS service_date, ex.service_id
	FROM candidate_dates cd
	JOIN calendar_dates ex
	  ON ex.date = to_char(cd.d, 'YYYYMMDD')
	 AND ex.exception_type = '1'
),
active_services_dedup AS (
	SELECT DISTINCT service_date, service_id FROM active_services
),
candidates AS (
	SELECT DISTINCT ON (st.stop_id, st.trip_id, a.service_date)
		st.stop_id,
		st.trip_id,
		t.route_id,
		t.trip_headsign,
		a.service_date,
		(a.service_date::timestamp + st.arrival_time::interval) AT TIME ZONE current_setting('TimeZone') AS scheduled_time
	FROM stop_times st
	JOIN trips t ON t.trip_id = st.trip_id
	JOIN active_services_dedup a ON a.service_id = t.service_id
	WHERE st.stop_id = ANY($2::text[])
	ORDER BY st.stop_id, st.trip_id, a.service_date
),
ranked AS (
SELECT
	c.stop_id,
	c.trip_id,
	c.route_id,
	COALESCE(r.route_short_name, '') AS route_short_name,
	COALESCE(c.trip_headsign, '') AS headsign,
	c.scheduled_time,
	COALESCE(
		c.scheduled_time + (rt.arrival_delay || ' seconds')::interval,
		c.scheduled_time
	) AS estimated_time,
	(rt.trip_id IS NOT NULL) AS is_realtime,
	ROW_NUMBER() OVER (PARTITION BY c.stop_id ORDER BY c.scheduled_time, c.trip_id) AS rn
FROM candidates c
LEFT JOIN routes r ON r.route_id = c.route_id
LEFT JOIN LATERAL (
	SELECT rt_inner.trip_id, rt_inner.arrival_delay
	FROM trip_update_stop_times_current rt_inner
	WHERE rt_inner.trip_id = c.trip_id AND rt_inner.stop_id = c.stop_id
	LIMIT 1
) rt ON true
WHERE c.scheduled_time >= (SELECT at FROM at_ts)
  AND c.scheduled_time <= (SELECT at FROM at_ts) + ($3::int || ' minutes')::interval
)
SELECT stop_id, trip_id, route_id, route_short_name, headsign, scheduled_time, estimated_time, is_realtime
FROM ranked
WHERE rn = 1
ORDER BY stop_id
`
	rows, err := r.pool.Query(ctx, q, at, stopIDs, windowMinutes)
	if err != nil {
		return nil, fmt.Errorf("query next arrivals per stop: %w", err)
	}
	defer rows.Close()

	out := make(map[string]models.StopArrivalLite, len(stopIDs))
	for rows.Next() {
		var (
			stopID string
			a      models.StopArrivalLite
		)
		if err = rows.Scan(&stopID, &a.TripID, &a.RouteID, &a.RouteShortName, &a.Headsign, &a.ScheduledTime, &a.EstimatedTime, &a.IsRealtime); err != nil {
			return nil, fmt.Errorf("scan next arrival per stop: %w", err)
		}
		out[stopID] = a
	}
	return out, rows.Err()
}

// ============================================================================
// Routes
// ============================================================================

type RouteSearchParams struct {
	Search    string
	RouteType string
	Limit     int
	Page      int
}

func (r *Repository) SearchRoutes(ctx context.Context, p RouteSearchParams) ([]models.Route, int, error) {
	var (
		filters []string
		args    []any
	)

	if p.Search != "" {
		args = append(args, "%"+strings.ToLower(p.Search)+"%")
		filters = append(filters, fmt.Sprintf("(LOWER(route_short_name) LIKE $%d OR LOWER(route_long_name) LIKE $%d)", len(args), len(args)))
	}

	if p.RouteType != "" {
		args = append(args, p.RouteType)
		filters = append(filters, fmt.Sprintf("route_type = $%d", len(args)))
	}

	where := ""
	if len(filters) > 0 {
		where = "WHERE " + strings.Join(filters, " AND ")
	}

	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM routes %s`, where)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count routes: %w", err)
	}

	offset := (p.Page - 1) * p.Limit
	args = append(args, p.Limit, offset)

	q := fmt.Sprintf(`
SELECT route_id,
	COALESCE(agency_id,''),
	COALESCE(route_short_name,''),
	COALESCE(route_long_name,''),
	COALESCE(route_type,''),
	COALESCE(route_color,''),
	COALESCE(route_text_color,'')
FROM routes
%s
ORDER BY route_short_name NULLS LAST, route_long_name NULLS LAST
LIMIT $%d OFFSET $%d
`, where, len(args)-1, len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query search routes: %w", err)
	}
	defer rows.Close()

	out := make([]models.Route, 0, p.Limit)
	for rows.Next() {
		var rt models.Route
		if err = rows.Scan(&rt.RouteID, &rt.AgencyID, &rt.ShortName, &rt.LongName, &rt.RouteType, &rt.RouteColor, &rt.RouteTextColor); err != nil {
			return nil, 0, fmt.Errorf("scan route: %w", err)
		}
		out = append(out, rt)
	}
	return out, total, rows.Err()
}

func (r *Repository) GetRoute(ctx context.Context, routeID string) (*models.Route, error) {
	const q = `
SELECT route_id,
	COALESCE(agency_id,''),
	COALESCE(route_short_name,''),
	COALESCE(route_long_name,''),
	COALESCE(route_type,''),
	COALESCE(route_color,''),
	COALESCE(route_text_color,'')
FROM routes
WHERE route_id = $1
`
	var rt models.Route
	err := r.pool.QueryRow(ctx, q, routeID).Scan(
		&rt.RouteID, &rt.AgencyID, &rt.ShortName, &rt.LongName, &rt.RouteType, &rt.RouteColor, &rt.RouteTextColor,
	)
	if err != nil {
		return nil, err
	}
	return &rt, nil
}

// LookupRouteIDByShortName returns route_id for an exact GTFS route_short_name (trimmed).
// ErrNoRows if none; error if more than one row matches (ambiguous).
func (r *Repository) LookupRouteIDByShortName(ctx context.Context, shortName string) (string, error) {
	const q = `
SELECT route_id
FROM routes
WHERE TRIM(COALESCE(route_short_name, '')) = $1
LIMIT 2
`
	rows, err := r.pool.Query(ctx, q, shortName)
	if err != nil {
		return "", fmt.Errorf("lookup route by short name: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", pgx.ErrNoRows
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("ambiguous route_short_name %q", shortName)
	}
}

func (r *Repository) GetRouteStops(ctx context.Context, routeID, directionID string) ([]models.StopSummary, error) {
	// When direction is specified, prefer a representative trip pattern so the
	// stops list aligns with the route shape shown on the map.
	if directionID != "" {
		const q = `
WITH representative_trip AS (
	SELECT t.trip_id
	FROM trips t
	JOIN stop_times st ON st.trip_id = t.trip_id
	WHERE t.route_id = $1
	  AND COALESCE(t.direction_id, '') = $2
	GROUP BY t.trip_id
	ORDER BY COUNT(*) DESC, t.trip_id
	LIMIT 1
)
SELECT s.stop_id, COALESCE(s.stop_name,''), COALESCE(s.stop_code,''), COALESCE(s.stop_lat,0), COALESCE(s.stop_lon,0)
FROM stop_times st
JOIN representative_trip rt ON rt.trip_id = st.trip_id
JOIN stops s ON s.stop_id = st.stop_id
ORDER BY st.stop_sequence
`
		rows, err := r.pool.Query(ctx, q, routeID, directionID)
		if err != nil {
			return nil, fmt.Errorf("query route stops by representative trip: %w", err)
		}
		defer rows.Close()

		out := make([]models.StopSummary, 0, 64)
		for rows.Next() {
			var s models.StopSummary
			if err = rows.Scan(&s.StopID, &s.StopName, &s.StopCode, &s.Lat, &s.Lon); err != nil {
				return nil, fmt.Errorf("scan route stop: %w", err)
			}
			out = append(out, s)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		if len(out) == 0 {
			return nil, pgx.ErrNoRows
		}
		return out, nil
	}

	const q = `
SELECT DISTINCT s.stop_id, COALESCE(s.stop_name,''), COALESCE(s.stop_code,''), COALESCE(s.stop_lat,0), COALESCE(s.stop_lon,0)
FROM stops s
JOIN stop_times st ON st.stop_id = s.stop_id
JOIN trips t ON t.trip_id = st.trip_id
WHERE t.route_id = $1
ORDER BY 2 NULLS LAST
`
	rows, err := r.pool.Query(ctx, q, routeID)
	if err != nil {
		return nil, fmt.Errorf("query route stops: %w", err)
	}
	defer rows.Close()

	out := make([]models.StopSummary, 0, 64)
	for rows.Next() {
		var s models.StopSummary
		if err = rows.Scan(&s.StopID, &s.StopName, &s.StopCode, &s.Lat, &s.Lon); err != nil {
			return nil, fmt.Errorf("scan route stop: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetRouteStopsAlongShapeTrip returns stops for the same representative trip used
// by GetRouteShape (must have a non-empty shape_id). Use this for map overlays so
// pins align with the drawn polyline.
func (r *Repository) GetRouteStopsAlongShapeTrip(ctx context.Context, routeID, directionID string) ([]models.StopSummary, error) {
	const q = `
WITH representative_trip AS (
	SELECT t.trip_id
	FROM trips t
	JOIN stop_times st ON st.trip_id = t.trip_id
	WHERE t.route_id = $1
	  AND t.shape_id IS NOT NULL
	  AND t.shape_id <> ''
	  AND ($2 = '' OR COALESCE(t.direction_id, '') = $2)
	GROUP BY t.trip_id, t.shape_id
	ORDER BY COUNT(*) DESC, t.trip_id
	LIMIT 1
)
SELECT s.stop_id, COALESCE(s.stop_name,''), COALESCE(s.stop_code,''), COALESCE(s.stop_lat,0), COALESCE(s.stop_lon,0)
FROM stop_times st
JOIN representative_trip rt ON rt.trip_id = st.trip_id
JOIN stops s ON s.stop_id = st.stop_id
WHERE s.stop_lat IS NOT NULL AND s.stop_lon IS NOT NULL
ORDER BY st.stop_sequence
`
	rows, err := r.pool.Query(ctx, q, routeID, directionID)
	if err != nil {
		return nil, fmt.Errorf("query route stops along shape trip: %w", err)
	}
	defer rows.Close()

	out := make([]models.StopSummary, 0, 64)
	for rows.Next() {
		var s models.StopSummary
		if err = rows.Scan(&s.StopID, &s.StopName, &s.StopCode, &s.Lat, &s.Lon); err != nil {
			return nil, fmt.Errorf("scan route stop: %w", err)
		}
		out = append(out, s)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetRouteShape returns one representative polyline for the route. maxPoints
// caps how many vertices are returned (uniform sampling in SQL). Use 0 for no
// cap (full GTFS resolution — can be slow and very large JSON).
func (r *Repository) GetRouteShape(ctx context.Context, routeID, directionID string, maxPoints int) (*models.RouteShape, error) {
	const q = `
WITH representative_trip AS (
	SELECT t.trip_id, t.shape_id
	FROM trips t
	JOIN stop_times st ON st.trip_id = t.trip_id
	WHERE t.route_id = $1
	  AND t.shape_id IS NOT NULL
	  AND t.shape_id <> ''
	  AND ($2 = '' OR COALESCE(t.direction_id, '') = $2)
	GROUP BY t.trip_id, t.shape_id
	ORDER BY COUNT(*) DESC, t.trip_id
	LIMIT 1
),
scoped AS (
	SELECT
		s.shape_id,
		s.shape_pt_sequence,
		s.shape_pt_lat,
		s.shape_pt_lon,
		ROW_NUMBER() OVER (ORDER BY s.shape_pt_sequence) AS rn,
		COUNT(*) OVER ()::int AS tot
	FROM shapes s
	JOIN representative_trip rt ON rt.shape_id = s.shape_id
	WHERE s.shape_pt_lat IS NOT NULL AND s.shape_pt_lon IS NOT NULL
)
SELECT shape_id, shape_pt_sequence, shape_pt_lat, shape_pt_lon, tot
FROM scoped
WHERE $3::int <= 0
   OR tot <= $3::int
   OR rn = 1
   OR rn = tot
   OR ($3::int = 1 AND rn = 1)
   OR (
        $3::int >= 2 AND tot > $3::int
        AND (rn - 1) % GREATEST(1, ((tot - 1) + ($3::int - 2)) / NULLIF($3::int - 1, 0)) = 0
      )
ORDER BY shape_pt_sequence
`
	dir := directionID
	rows, err := r.pool.Query(ctx, q, routeID, dir, maxPoints)
	if err != nil {
		return nil, fmt.Errorf("query route shape: %w", err)
	}
	defer rows.Close()

	preCap := maxPoints
	if preCap <= 0 {
		preCap = 1024
	}
	shape := &models.RouteShape{
		RouteID: routeID,
		Points:  make([]models.RouteShapePoint, 0, preCap),
	}
	for rows.Next() {
		var (
			shapeID string
			pt      models.RouteShapePoint
			tot     int
		)
		if err = rows.Scan(&shapeID, &pt.Sequence, &pt.Lat, &pt.Lon, &tot); err != nil {
			return nil, fmt.Errorf("scan route shape point: %w", err)
		}
		shape.ShapeID = shapeID
		shape.TotalPoints = tot
		shape.Points = append(shape.Points, pt)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(shape.Points) == 0 {
		return nil, pgx.ErrNoRows
	}
	return shape, nil
}

func (r *Repository) GetRouteDirections(ctx context.Context, routeID string) ([]models.RouteDirection, error) {
	const q = `
SELECT
	COALESCE(direction_id, '') AS direction_id,
	COALESCE(NULLIF(MAX(trip_headsign), ''), '') AS headsign,
	COUNT(*)::int AS trip_count
FROM trips
WHERE route_id = $1
GROUP BY COALESCE(direction_id, '')
ORDER BY COALESCE(direction_id, '')
`
	rows, err := r.pool.Query(ctx, q, routeID)
	if err != nil {
		return nil, fmt.Errorf("query route directions: %w", err)
	}
	defer rows.Close()

	out := make([]models.RouteDirection, 0, 2)
	for rows.Next() {
		var d models.RouteDirection
		if err = rows.Scan(&d.DirectionID, &d.Headsign, &d.TripCount); err != nil {
			return nil, fmt.Errorf("scan route direction: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// overlayDirectionKeep returns false if this direction row should be skipped when
// the client asked for a single direction. Feeds often leave direction_id blank for
// all trips; those rows must still match directionId=0 or 1 on the client.
func overlayDirectionKeep(onlyDirection, rowDirectionID string) bool {
	if onlyDirection == "" {
		return true
	}
	if rowDirectionID == onlyDirection {
		return true
	}
	// Unlabeled direction in GTFS: still show when user picked one direction
	// (there is no separate inbound/outbound in the data).
	if rowDirectionID == "" {
		return true
	}
	return false
}

// GetMapRouteWithGeometry loads route display fields, then for each GTFS direction
// loads the representative shape (capped) and matching stop sequence.
func (r *Repository) GetMapRouteWithGeometry(ctx context.Context, routeID string, maxShapePoints int, onlyDirection string) (*models.MapRouteWithGeometry, error) {
	rt, err := r.GetRoute(ctx, routeID)
	if err != nil {
		return nil, err
	}
	dirs, err := r.GetRouteDirections(ctx, routeID)
	if err != nil {
		return nil, err
	}

	out := &models.MapRouteWithGeometry{
		RouteID:        rt.RouteID,
		ShortName:      rt.ShortName,
		LongName:       rt.LongName,
		RouteColor:     rt.RouteColor,
		RouteTextColor: rt.RouteTextColor,
		Legs:           make([]models.MapRouteLeg, 0, len(dirs)),
	}

	for _, d := range dirs {
		if !overlayDirectionKeep(onlyDirection, d.DirectionID) {
			continue
		}
		dirID := d.DirectionID
		leg := models.MapRouteLeg{
			DirectionID: d.DirectionID,
			Headsign:    d.Headsign,
			Points:      []models.RouteShapePoint{},
			Stops:       []models.StopSummary{},
		}

		shape, errSh := r.GetRouteShape(ctx, routeID, dirID, maxShapePoints)
		if errSh == nil && shape != nil {
			leg.ShapeID = shape.ShapeID
			leg.Points = shape.Points
			leg.TotalPoints = shape.TotalPoints
		}

		// Stops must come from the same shape-backed trip as the polyline when possible.
		stops, errSt := r.GetRouteStopsAlongShapeTrip(ctx, routeID, dirID)
		if errSt != nil || len(stops) == 0 {
			stops, errSt = r.GetRouteStops(ctx, routeID, dirID)
		}
		if errSt != nil || len(stops) == 0 {
			stops, _ = r.GetRouteStops(ctx, routeID, "")
		}
		if stops != nil {
			leg.Stops = stops
		}

		if len(leg.Points) == 0 && len(leg.Stops) == 0 {
			continue
		}
		models.EnrichStopSummariesForMap(leg.Stops)
		out.Legs = append(out.Legs, leg)
	}

	if len(out.Legs) == 0 {
		return nil, pgx.ErrNoRows
	}

	seen := make(map[string]struct{}, 128)
	union := make([]models.StopSummary, 0, 128)
	for i := range out.Legs {
		for _, s := range out.Legs[i].Stops {
			if s.StopID == "" {
				continue
			}
			if _, ok := seen[s.StopID]; ok {
				continue
			}
			seen[s.StopID] = struct{}{}
			union = append(union, s)
		}
	}
	if len(union) > 0 {
		out.AllStops = union
		out.Stops = union
	}

	return out, nil
}

// ============================================================================
// Trips
// ============================================================================

func (r *Repository) GetTrip(ctx context.Context, tripID string) (*models.Trip, error) {
	const q = `
SELECT trip_id,
	COALESCE(route_id,''), COALESCE(service_id,''),
	COALESCE(trip_headsign,''), COALESCE(trip_short_name,''),
	COALESCE(direction_id,''), COALESCE(block_id,''), COALESCE(shape_id,''),
	COALESCE(wheelchair_accessible,''), COALESCE(bikes_allowed,'')
FROM trips
WHERE trip_id = $1
`
	var t models.Trip
	err := r.pool.QueryRow(ctx, q, tripID).Scan(
		&t.TripID, &t.RouteID, &t.ServiceID, &t.Headsign, &t.ShortName,
		&t.DirectionID, &t.BlockID, &t.ShapeID, &t.Wheelchair, &t.BikesAllowed,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *Repository) GetTripStopTimes(ctx context.Context, tripID string) ([]models.TripStopTime, error) {
	const q = `
SELECT st.stop_id, COALESCE(s.stop_name,''), st.stop_sequence,
	COALESCE(st.arrival_time,''), COALESCE(st.departure_time,''),
	COALESCE(st.stop_headsign,''), COALESCE(st.pickup_type,''), COALESCE(st.drop_off_type,'')
FROM stop_times st
LEFT JOIN stops s ON s.stop_id = st.stop_id
WHERE st.trip_id = $1
ORDER BY st.stop_sequence
`
	rows, err := r.pool.Query(ctx, q, tripID)
	if err != nil {
		return nil, fmt.Errorf("query trip stop times: %w", err)
	}
	defer rows.Close()

	out := make([]models.TripStopTime, 0, 32)
	for rows.Next() {
		var st models.TripStopTime
		if err = rows.Scan(&st.StopID, &st.StopName, &st.StopSequence, &st.ArrivalTime, &st.DepartureTime, &st.StopHeadsign, &st.PickupType, &st.DropOffType); err != nil {
			return nil, fmt.Errorf("scan trip stop time: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ResolveTripIDForVehicle returns latest trip_id for a vehicle row.
func (r *Repository) ResolveTripIDForVehicle(ctx context.Context, vehicleID string) (string, error) {
	const q = `
SELECT COALESCE(v.trip_id, '')
FROM vehicle_positions_current v
WHERE v.vehicle_id = $1
ORDER BY COALESCE(v.updated_at, NOW()) DESC
LIMIT 1
`
	var tripID string
	if err := r.pool.QueryRow(ctx, q, vehicleID).Scan(&tripID); err != nil {
		return "", err
	}
	if strings.TrimSpace(tripID) == "" {
		return "", pgx.ErrNoRows
	}
	return tripID, nil
}

// GetTripLiveSnapshot returns route/trip metadata + upcoming stops ETA rows for one trip.
func (r *Repository) GetTripLiveSnapshot(ctx context.Context, tripID string, at time.Time, limit int) (*models.TripLivePayload, error) {
	if limit <= 0 {
		limit = 12
	}
	if limit > 50 {
		limit = 50
	}

	const qMeta = `
SELECT
	t.trip_id,
	COALESCE(t.route_id, ''),
	COALESCE(r.route_short_name, ''),
	COALESCE(r.route_long_name, ''),
	COALESCE(t.direction_id, ''),
	COALESCE(t.trip_headsign, ''),
	v.vehicle_id,
	v.latitude,
	v.longitude,
	v.bearing,
	COALESCE(v.updated_at, NOW())
FROM trips t
LEFT JOIN routes r ON r.route_id = t.route_id
LEFT JOIN LATERAL (
	SELECT vehicle_id, latitude, longitude, bearing, updated_at
	FROM vehicle_positions_current
	WHERE trip_id = t.trip_id
	  AND latitude IS NOT NULL
	  AND longitude IS NOT NULL
	ORDER BY COALESCE(updated_at, NOW()) DESC
	LIMIT 1
) v ON true
WHERE t.trip_id = $1
`
	out := &models.TripLivePayload{TripID: tripID, UpcomingStops: make([]models.UpcomingStopETA, 0, limit)}
	var (
		vehicleID sql.NullString
		lat, lon  sql.NullFloat64
		bearing   sql.NullFloat64
		updated   time.Time
	)
	if err := r.pool.QueryRow(ctx, qMeta, tripID).Scan(
		&out.TripID, &out.RouteID, &out.RouteShortName, &out.RouteLongName, &out.DirectionID, &out.Headsign,
		&vehicleID, &lat, &lon, &bearing, &updated,
	); err != nil {
		return nil, err
	}
	if vehicleID.Valid {
		out.VehicleID = vehicleID.String
	}
	if lat.Valid {
		v := lat.Float64
		out.Lat = &v
	}
	if lon.Valid {
		v := lon.Float64
		out.Lon = &v
	}
	if bearing.Valid {
		v := bearing.Float64
		out.Bearing = &v
	}
	out.Timestamp = &updated
	out.UpdatedAt = &updated

	const qStops = `
WITH at_ts AS (
	SELECT $2::timestamptz AS at
),
candidate_dates AS (
	SELECT (at::date) AS d FROM at_ts
	UNION ALL
	SELECT (at::date - INTERVAL '1 day')::date FROM at_ts
),
expanded AS (
	SELECT
		st.stop_id,
		COALESCE(s.stop_name, '') AS stop_name,
		st.stop_sequence,
		(cd.d::timestamp + st.arrival_time::interval) AT TIME ZONE current_setting('TimeZone') AS scheduled_time,
		rt.arrival_delay
	FROM stop_times st
	JOIN candidate_dates cd ON true
	LEFT JOIN stops s ON s.stop_id = st.stop_id
	LEFT JOIN LATERAL (
		SELECT rt_inner.arrival_delay
		FROM trip_update_stop_times_current rt_inner
		WHERE rt_inner.trip_id = st.trip_id AND rt_inner.stop_id = st.stop_id
		LIMIT 1
	) rt ON true
	WHERE st.trip_id = $1
),
future AS (
	SELECT *
	FROM expanded
	WHERE scheduled_time >= (SELECT at FROM at_ts)
	ORDER BY scheduled_time, stop_sequence
	LIMIT $3
)
SELECT
	stop_id,
	stop_name,
	stop_sequence,
	scheduled_time,
	CASE WHEN arrival_delay IS NULL THEN NULL ELSE scheduled_time + (arrival_delay || ' seconds')::interval END AS estimated_time,
	arrival_delay
FROM future
ORDER BY scheduled_time, stop_sequence
`
	rows, err := r.pool.Query(ctx, qStops, tripID, at, limit)
	if err != nil {
		return nil, fmt.Errorf("query trip live stops: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			u       models.UpcomingStopETA
			est     sql.NullTime
			delay   sql.NullInt64
			baseETA time.Time
		)
		if err = rows.Scan(&u.StopID, &u.StopName, &u.Sequence, &u.ScheduledTime, &est, &delay); err != nil {
			return nil, fmt.Errorf("scan trip live stop: %w", err)
		}
		if est.Valid {
			t := est.Time
			u.EstimatedTime = &t
			u.IsRealtime = true
			baseETA = t
		} else {
			u.IsRealtime = false
			baseETA = u.ScheduledTime
		}
		if delay.Valid {
			d := int(delay.Int64)
			out.DelaySeconds = &d
		}
		u.ETAMinutes = int(time.Until(baseETA).Minutes())
		if u.ETAMinutes < 0 {
			u.ETAMinutes = 0
		}
		out.UpcomingStops = append(out.UpcomingStops, u)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out.UpcomingStops) > 0 {
		out.NextStopID = out.UpcomingStops[0].StopID
		out.NextStopName = out.UpcomingStops[0].StopName
	}
	return out, nil
}

func (r *Repository) ListTrips(ctx context.Context, routeID, serviceDate string, limit int) ([]models.Trip, error) {
	var (
		filters []string
		args    []any
	)

	args = append(args, serviceDate)
	q := `
WITH active_services AS (
	SELECT c.service_id
	FROM calendar c
	WHERE to_date(c.start_date, 'YYYYMMDD') <= $1::date
	  AND to_date(c.end_date,   'YYYYMMDD') >= $1::date
	  AND CASE EXTRACT(ISODOW FROM $1::date)
			WHEN 1 THEN c.monday
			WHEN 2 THEN c.tuesday
			WHEN 3 THEN c.wednesday
			WHEN 4 THEN c.thursday
			WHEN 5 THEN c.friday
			WHEN 6 THEN c.saturday
			WHEN 7 THEN c.sunday
		 END = '1'
	  AND NOT EXISTS (
		SELECT 1 FROM calendar_dates ex
		WHERE ex.service_id = c.service_id
		  AND ex.date = to_char($1::date, 'YYYYMMDD')
		  AND ex.exception_type = '2'
	  )
	UNION
	SELECT ex.service_id
	FROM calendar_dates ex
	WHERE ex.date = to_char($1::date, 'YYYYMMDD')
	  AND ex.exception_type = '1'
)
SELECT t.trip_id,
	COALESCE(t.route_id,''), COALESCE(t.service_id,''),
	COALESCE(t.trip_headsign,''), COALESCE(t.trip_short_name,''),
	COALESCE(t.direction_id,''), COALESCE(t.block_id,''), COALESCE(t.shape_id,''),
	COALESCE(t.wheelchair_accessible,''), COALESCE(t.bikes_allowed,'')
FROM trips t
JOIN active_services a ON a.service_id = t.service_id
`

	if routeID != "" {
		args = append(args, routeID)
		filters = append(filters, fmt.Sprintf("t.route_id = $%d", len(args)))
	}

	if len(filters) > 0 {
		q += "WHERE " + strings.Join(filters, " AND ") + "\n"
	}

	args = append(args, limit)
	q += fmt.Sprintf("ORDER BY t.trip_id\nLIMIT $%d\n", len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query list trips: %w", err)
	}
	defer rows.Close()

	out := make([]models.Trip, 0, limit)
	for rows.Next() {
		var t models.Trip
		if err = rows.Scan(&t.TripID, &t.RouteID, &t.ServiceID, &t.Headsign, &t.ShortName, &t.DirectionID, &t.BlockID, &t.ShapeID, &t.Wheelchair, &t.BikesAllowed); err != nil {
			return nil, fmt.Errorf("scan trip: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTimetableTripsLite returns compact trip rows for a route on a service date.
func (r *Repository) ListTimetableTripsLite(ctx context.Context, routeID, serviceDate, directionID string, limit, offset int) ([]models.TimetableTripLite, error) {
	if limit <= 0 {
		limit = 80
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	args := []any{serviceDate, routeID}
	fDir := ""
	if strings.TrimSpace(directionID) != "" {
		args = append(args, directionID)
		fDir = fmt.Sprintf("AND COALESCE(t.direction_id, '') = $%d", len(args))
	}
	args = append(args, limit, offset)
	limitIdx := len(args) - 1
	offsetIdx := len(args)
	q := fmt.Sprintf(`
WITH active_services AS (
	SELECT c.service_id
	FROM calendar c
	WHERE to_date(c.start_date, 'YYYYMMDD') <= $1::date
	  AND to_date(c.end_date,   'YYYYMMDD') >= $1::date
	  AND CASE EXTRACT(ISODOW FROM $1::date)
			WHEN 1 THEN c.monday
			WHEN 2 THEN c.tuesday
			WHEN 3 THEN c.wednesday
			WHEN 4 THEN c.thursday
			WHEN 5 THEN c.friday
			WHEN 6 THEN c.saturday
			WHEN 7 THEN c.sunday
		 END = '1'
	  AND NOT EXISTS (
		SELECT 1 FROM calendar_dates ex
		WHERE ex.service_id = c.service_id
		  AND ex.date = to_char($1::date, 'YYYYMMDD')
		  AND ex.exception_type = '2'
	  )
	UNION
	SELECT ex.service_id
	FROM calendar_dates ex
	WHERE ex.date = to_char($1::date, 'YYYYMMDD')
	  AND ex.exception_type = '1'
),
scoped AS (
	SELECT
		t.trip_id,
		COALESCE(t.direction_id, '') AS direction_id,
		COALESCE(t.trip_headsign, '') AS headsign,
		COALESCE(t.trip_short_name, '') AS short_name
	FROM trips t
	JOIN active_services a ON a.service_id = t.service_id
	WHERE t.route_id = $2
	%s
	ORDER BY COALESCE(t.direction_id, ''), COALESCE(t.trip_headsign, ''), t.trip_id
	LIMIT $%d OFFSET $%d
),
first_last AS (
	SELECT
		s.trip_id,
		MIN(st.stop_sequence) AS first_seq,
		MAX(st.stop_sequence) AS last_seq,
		COUNT(*)::int AS stop_count
	FROM scoped s
	JOIN stop_times st ON st.trip_id = s.trip_id
	GROUP BY s.trip_id
)
SELECT
	s.trip_id,
	s.direction_id,
	s.headsign,
	s.short_name,
	COALESCE(stf.stop_id, ''),
	COALESCE(stf.departure_time, stf.arrival_time, ''),
	COALESCE(stl.stop_id, ''),
	COALESCE(stl.departure_time, stl.arrival_time, ''),
	COALESCE(fl.stop_count, 0)
FROM scoped s
LEFT JOIN first_last fl ON fl.trip_id = s.trip_id
LEFT JOIN stop_times stf ON stf.trip_id = s.trip_id AND stf.stop_sequence = fl.first_seq
LEFT JOIN stop_times stl ON stl.trip_id = s.trip_id AND stl.stop_sequence = fl.last_seq
ORDER BY s.direction_id, s.headsign, s.trip_id
`, fDir, limitIdx, offsetIdx)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query timetable trips lite: %w", err)
	}
	defer rows.Close()

	out := make([]models.TimetableTripLite, 0, limit)
	for rows.Next() {
		var t models.TimetableTripLite
		if err = rows.Scan(&t.TripID, &t.DirectionID, &t.Headsign, &t.ShortName, &t.FirstStopID, &t.FirstTime, &t.LastStopID, &t.LastTime, &t.StopCount); err != nil {
			return nil, fmt.Errorf("scan timetable trip lite: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTimetableTripsLiteStatic returns compact trip rows without calendar/day filtering.
// Use as fallback when a route has static stop_times but no active services for the requested date.
func (r *Repository) ListTimetableTripsLiteStatic(ctx context.Context, routeID, directionID string, limit, offset int) ([]models.TimetableTripLite, error) {
	if limit <= 0 {
		limit = 80
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	args := []any{routeID}
	fDir := ""
	if strings.TrimSpace(directionID) != "" {
		args = append(args, directionID)
		fDir = fmt.Sprintf("AND COALESCE(t.direction_id, '') = $%d", len(args))
	}
	args = append(args, limit, offset)
	limitIdx := len(args) - 1
	offsetIdx := len(args)

	q := fmt.Sprintf(`
WITH scoped AS (
	SELECT
		t.trip_id,
		COALESCE(t.direction_id, '') AS direction_id,
		COALESCE(t.trip_headsign, '') AS headsign,
		COALESCE(t.trip_short_name, '') AS short_name
	FROM trips t
	WHERE t.route_id = $1
	%s
	ORDER BY COALESCE(t.direction_id, ''), COALESCE(t.trip_headsign, ''), t.trip_id
	LIMIT $%d OFFSET $%d
),
first_last AS (
	SELECT
		s.trip_id,
		MIN(st.stop_sequence) AS first_seq,
		MAX(st.stop_sequence) AS last_seq,
		COUNT(*)::int AS stop_count
	FROM scoped s
	JOIN stop_times st ON st.trip_id = s.trip_id
	GROUP BY s.trip_id
)
SELECT
	s.trip_id,
	s.direction_id,
	s.headsign,
	s.short_name,
	COALESCE(stf.stop_id, ''),
	COALESCE(stf.departure_time, stf.arrival_time, ''),
	COALESCE(stl.stop_id, ''),
	COALESCE(stl.departure_time, stl.arrival_time, ''),
	COALESCE(fl.stop_count, 0)
FROM scoped s
LEFT JOIN first_last fl ON fl.trip_id = s.trip_id
LEFT JOIN stop_times stf ON stf.trip_id = s.trip_id AND stf.stop_sequence = fl.first_seq
LEFT JOIN stop_times stl ON stl.trip_id = s.trip_id AND stl.stop_sequence = fl.last_seq
ORDER BY s.direction_id, s.headsign, s.trip_id
`, fDir, limitIdx, offsetIdx)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query timetable trips lite static: %w", err)
	}
	defer rows.Close()

	out := make([]models.TimetableTripLite, 0, limit)
	for rows.Next() {
		var t models.TimetableTripLite
		if err = rows.Scan(&t.TripID, &t.DirectionID, &t.Headsign, &t.ShortName, &t.FirstStopID, &t.FirstTime, &t.LastStopID, &t.LastTime, &t.StopCount); err != nil {
			return nil, fmt.Errorf("scan timetable trip lite static: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ============================================================================
// Calendar
// ============================================================================

func (r *Repository) ListCalendarServices(ctx context.Context, page, limit int) ([]models.CalendarService, int, error) {
	offset := (page - 1) * limit
	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM calendar`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count calendar: %w", err)
	}
	const q = `
SELECT service_id,
	COALESCE(monday::text,'0'), COALESCE(tuesday::text,'0'), COALESCE(wednesday::text,'0'),
	COALESCE(thursday::text,'0'), COALESCE(friday::text,'0'), COALESCE(saturday::text,'0'), COALESCE(sunday::text,'0'),
	to_char(to_date(start_date, 'YYYYMMDD'), 'YYYY-MM-DD'),
	to_char(to_date(end_date, 'YYYYMMDD'), 'YYYY-MM-DD')
FROM calendar
ORDER BY service_id
LIMIT $1 OFFSET $2
`
	rows, err := r.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query calendar list: %w", err)
	}
	defer rows.Close()

	out := make([]models.CalendarService, 0, limit)
	for rows.Next() {
		var c models.CalendarService
		if err = rows.Scan(
			&c.ServiceID, &c.Monday, &c.Tuesday, &c.Wednesday, &c.Thursday, &c.Friday, &c.Saturday, &c.Sunday,
			&c.StartDate, &c.EndDate,
		); err != nil {
			return nil, 0, fmt.Errorf("scan calendar: %w", err)
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

func (r *Repository) GetCalendarService(ctx context.Context, serviceID string) (*models.CalendarService, error) {
	const q = `
SELECT service_id,
	COALESCE(monday::text,'0'), COALESCE(tuesday::text,'0'), COALESCE(wednesday::text,'0'),
	COALESCE(thursday::text,'0'), COALESCE(friday::text,'0'), COALESCE(saturday::text,'0'), COALESCE(sunday::text,'0'),
	to_char(to_date(start_date, 'YYYYMMDD'), 'YYYY-MM-DD'),
	to_char(to_date(end_date, 'YYYYMMDD'), 'YYYY-MM-DD')
FROM calendar
WHERE service_id = $1
`
	var c models.CalendarService
	err := r.pool.QueryRow(ctx, q, serviceID).Scan(
		&c.ServiceID, &c.Monday, &c.Tuesday, &c.Wednesday, &c.Thursday, &c.Friday, &c.Saturday, &c.Sunday,
		&c.StartDate, &c.EndDate,
	)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Repository) GetServiceCalendarOnDate(ctx context.Context, date string) ([]models.ServiceCalendarDay, error) {
	// Weekday flags as text (int/bool/smallint). Exception 1 = service runs; 2 = removed for that date.
	const q = `
SELECT c.service_id, 'calendar' AS source
FROM calendar c
WHERE to_date(c.start_date, 'YYYYMMDD') <= $1::date
  AND to_date(c.end_date,   'YYYYMMDD') >= $1::date
  AND lower(trim(both from (
	CASE EXTRACT(ISODOW FROM $1::date)::int
		WHEN 1 THEN COALESCE(c.monday::text, '0')
		WHEN 2 THEN COALESCE(c.tuesday::text, '0')
		WHEN 3 THEN COALESCE(c.wednesday::text, '0')
		WHEN 4 THEN COALESCE(c.thursday::text, '0')
		WHEN 5 THEN COALESCE(c.friday::text, '0')
		WHEN 6 THEN COALESCE(c.saturday::text, '0')
		WHEN 7 THEN COALESCE(c.sunday::text, '0')
	END
  ))) IN ('1', 't', 'true')
  AND NOT EXISTS (
	SELECT 1 FROM calendar_dates ex
	WHERE ex.service_id = c.service_id
	  AND ex.date = to_char($1::date, 'YYYYMMDD')
	  AND trim(both from COALESCE(ex.exception_type::text, '')) IN ('2')
  )
UNION
SELECT ex.service_id, 'exception_added' AS source
FROM calendar_dates ex
WHERE ex.date = to_char($1::date, 'YYYYMMDD')
  AND trim(both from COALESCE(ex.exception_type::text, '')) IN ('1')
ORDER BY service_id
`
	rows, err := r.pool.Query(ctx, q, date)
	if err != nil {
		return nil, fmt.Errorf("query service calendar: %w", err)
	}
	defer rows.Close()

	out := make([]models.ServiceCalendarDay, 0, 16)
	for rows.Next() {
		var d models.ServiceCalendarDay
		if err = rows.Scan(&d.ServiceID, &d.Source); err != nil {
			return nil, fmt.Errorf("scan service calendar: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *Repository) GetCalendarExceptions(ctx context.Context, fromDate, toDate string) ([]models.CalendarException, error) {
	const q = `
SELECT service_id,
	to_char(to_date(date, 'YYYYMMDD'), 'YYYY-MM-DD'),
	trim(both from COALESCE(exception_type::text, ''))
FROM calendar_dates
WHERE to_date(date, 'YYYYMMDD') >= $1::date
  AND to_date(date, 'YYYYMMDD') <= $2::date
ORDER BY to_date(date, 'YYYYMMDD'), service_id
`
	rows, err := r.pool.Query(ctx, q, fromDate, toDate)
	if err != nil {
		return nil, fmt.Errorf("query calendar exceptions: %w", err)
	}
	defer rows.Close()

	out := make([]models.CalendarException, 0, 64)
	for rows.Next() {
		var e models.CalendarException
		if err = rows.Scan(&e.ServiceID, &e.Date, &e.ExceptionType); err != nil {
			return nil, fmt.Errorf("scan calendar exception: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetFeedCalendarWindow returns min/max calendar coverage from calendar + calendar_dates tables.
func (r *Repository) GetFeedCalendarWindow(ctx context.Context) (*models.FeedCalendarWindow, error) {
	const q = `
SELECT
	to_char(MIN(x.d), 'YYYY-MM-DD'),
	to_char(MAX(x.d), 'YYYY-MM-DD')
FROM (
	SELECT to_date(start_date, 'YYYYMMDD') AS d FROM calendar WHERE start_date ~ '^[0-9]{8}$'
	UNION ALL
	SELECT to_date(end_date, 'YYYYMMDD') FROM calendar WHERE end_date ~ '^[0-9]{8}$'
	UNION ALL
	SELECT to_date(date, 'YYYYMMDD') FROM calendar_dates WHERE date ~ '^[0-9]{8}$'
) x
WHERE x.d IS NOT NULL
`
	var minD, maxD sql.NullString
	if err := r.pool.QueryRow(ctx, q).Scan(&minD, &maxD); err != nil {
		return nil, fmt.Errorf("feed calendar window: %w", err)
	}
	if !minD.Valid || !maxD.Valid || minD.String == "" || maxD.String == "" {
		return &models.FeedCalendarWindow{}, nil
	}
	return &models.FeedCalendarWindow{MinDate: minD.String, MaxDate: maxD.String}, nil
}

// CountTripsForService returns how many trips reference service_id (static timetable).
func (r *Repository) CountTripsForService(ctx context.Context, serviceID string) (int, error) {
	const q = `SELECT COUNT(*)::int FROM trips WHERE service_id = $1`
	var n int
	if err := r.pool.QueryRow(ctx, q, serviceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count trips for service: %w", err)
	}
	return n, nil
}

// RouteShortNamesForService returns distinct route short names (or route_id fallback) for trips on service_id.
func (r *Repository) RouteShortNamesForService(ctx context.Context, serviceID string) ([]string, error) {
	const q = `
SELECT DISTINCT COALESCE(NULLIF(trim(both from r.route_short_name), ''), t.route_id)
FROM trips t
JOIN routes r ON r.route_id = t.route_id
WHERE t.service_id = $1
ORDER BY 1
LIMIT 48
`
	rows, err := r.pool.Query(ctx, q, serviceID)
	if err != nil {
		return nil, fmt.Errorf("route names for service: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, 16)
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListFavoriteRoutes returns favorited route ids with lightweight display fields.
func (r *Repository) ListFavoriteRoutes(ctx context.Context, userID string) ([]models.FavoriteRoute, error) {
	const q = `
WITH live_by_route AS (
	SELECT
		COALESCE(
			NULLIF(TRIM(COALESCE(v.route_id, '')), ''),
			NULLIF(TRIM(COALESCE(t.route_id, '')), ''),
			''
		) AS route_id,
		COUNT(*)::int AS live_vehicle_count,
		MAX(COALESCE(v.updated_at, NOW())) AS last_live_updated_at
	FROM vehicle_positions_current v
	LEFT JOIN trips t ON t.trip_id = v.trip_id
	WHERE v.latitude IS NOT NULL
	  AND v.longitude IS NOT NULL
	  AND COALESCE(v.updated_at, NOW()) >= NOW() - INTERVAL '2 minutes'
	GROUP BY 1
)
SELECT
	f.route_id,
	COALESCE(NULLIF(TRIM(r.route_short_name), ''), ''),
	COALESCE(NULLIF(TRIM(r.route_long_name), ''), ''),
	COALESCE(NULLIF(TRIM(r.route_color), ''), ''),
	COALESCE(NULLIF(TRIM(r.route_text_color), ''), ''),
	COALESCE(l.live_vehicle_count, 0) AS live_vehicle_count,
	CASE WHEN COALESCE(l.live_vehicle_count, 0) > 0 THEN true ELSE false END AS has_live_vehicles,
	l.last_live_updated_at
FROM user_favorite_routes f
LEFT JOIN routes r ON r.route_id = f.route_id
LEFT JOIN live_by_route l ON l.route_id = f.route_id
WHERE f.user_id = $1::uuid
ORDER BY f.created_at DESC, f.route_id
`
	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list favorite routes: %w", err)
	}
	defer rows.Close()

	out := make([]models.FavoriteRoute, 0, 16)
	routeIDs := make([]string, 0, 16)
	for rows.Next() {
		var f models.FavoriteRoute
		var lastLiveUpdatedAt sql.NullTime
		if err = rows.Scan(
			&f.RouteID,
			&f.ShortName,
			&f.LongName,
			&f.RouteColor,
			&f.RouteTextColor,
			&f.LiveVehicleCount,
			&f.HasLiveVehicles,
			&lastLiveUpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan favorite route: %w", err)
		}
		if lastLiveUpdatedAt.Valid {
			t := lastLiveUpdatedAt.Time
			f.LastLiveUpdatedAt = &t
		}
		out = append(out, f)
		routeIDs = append(routeIDs, f.RouteID)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(routeIDs) == 0 {
		return out, nil
	}
	nextStopsByRoute, err := r.listFavoriteRouteNextStops(ctx, routeIDs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if stops, ok := nextStopsByRoute[out[i].RouteID]; ok {
			out[i].NextTwoStops = stops
		}
	}
	return out, nil
}

func (r *Repository) listFavoriteRouteNextStops(ctx context.Context, routeIDs []string) (map[string][]models.FavoriteRouteNextStopPreview, error) {
	out := make(map[string][]models.FavoriteRouteNextStopPreview, len(routeIDs))
	if len(routeIDs) == 0 {
		return out, nil
	}
	const q = `
WITH live AS (
	SELECT
		COALESCE(
			NULLIF(TRIM(COALESCE(v.route_id, '')), ''),
			NULLIF(TRIM(COALESCE(t.route_id, '')), ''),
			''
		) AS route_id,
		COALESCE(v.trip_id, '') AS trip_id,
		COALESCE(v.updated_at, NOW()) AS updated_at
	FROM vehicle_positions_current v
	LEFT JOIN trips t ON t.trip_id = v.trip_id
	WHERE v.latitude IS NOT NULL
	  AND v.longitude IS NOT NULL
	  AND COALESCE(v.updated_at, NOW()) >= NOW() - INTERVAL '2 minutes'
),
chosen AS (
	SELECT route_id, trip_id
	FROM (
		SELECT
			route_id,
			trip_id,
			ROW_NUMBER() OVER (PARTITION BY route_id ORDER BY updated_at DESC) AS rn
		FROM live
		WHERE route_id = ANY($1::text[]) AND trip_id <> ''
	) x
	WHERE rn = 1
),
candidate_dates AS (
	SELECT CURRENT_DATE AS d
	UNION ALL
	SELECT (CURRENT_DATE - INTERVAL '1 day')::date
),
expanded AS (
	SELECT
		c.route_id,
		st.stop_id,
		COALESCE(s.stop_name, '') AS stop_name,
		st.stop_sequence,
		(cd.d::timestamp + st.arrival_time::interval) AT TIME ZONE current_setting('TimeZone') AS scheduled_time,
		rt.arrival_delay
	FROM chosen c
	JOIN stop_times st ON st.trip_id = c.trip_id
	JOIN candidate_dates cd ON true
	LEFT JOIN stops s ON s.stop_id = st.stop_id
	LEFT JOIN LATERAL (
		SELECT rt_inner.arrival_delay
		FROM trip_update_stop_times_current rt_inner
		WHERE rt_inner.trip_id = c.trip_id AND rt_inner.stop_id = st.stop_id
		LIMIT 1
	) rt ON true
),
future AS (
	SELECT
		route_id,
		stop_id,
		stop_name,
		stop_sequence,
		scheduled_time,
		arrival_delay,
		ROW_NUMBER() OVER (PARTITION BY route_id ORDER BY scheduled_time, stop_sequence) AS rn
	FROM expanded
	WHERE scheduled_time >= NOW()
)
SELECT
	route_id,
	stop_id,
	stop_name,
	scheduled_time,
	CASE
		WHEN arrival_delay IS NULL THEN NULL
		ELSE scheduled_time + (arrival_delay || ' seconds')::interval
	END AS estimated_time
FROM future
WHERE rn <= 2
ORDER BY route_id, rn
`
	rows, err := r.pool.Query(ctx, q, routeIDs)
	if err != nil {
		return nil, fmt.Errorf("query favorite next stops: %w", err)
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		var (
			routeID       string
			scheduledTime time.Time
			estimatedTime sql.NullTime
			stop          models.FavoriteRouteNextStopPreview
			baseTime      time.Time
		)
		if err = rows.Scan(&routeID, &stop.StopID, &stop.StopName, &scheduledTime, &estimatedTime); err != nil {
			return nil, fmt.Errorf("scan favorite next stop: %w", err)
		}
		if estimatedTime.Valid {
			stop.IsRealtime = true
			baseTime = estimatedTime.Time
		} else {
			baseTime = scheduledTime
		}
		stop.ETAMinutes = int(baseTime.Sub(now).Minutes())
		if stop.ETAMinutes < 0 {
			stop.ETAMinutes = 0
		}
		out[routeID] = append(out[routeID], stop)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AddFavoriteRoute inserts one (user_id, route_id) pair, ignoring duplicates.
func (r *Repository) AddFavoriteRoute(ctx context.Context, userID, routeID string) error {
	const q = `
INSERT INTO user_favorite_routes (user_id, route_id)
VALUES ($1::uuid, $2)
ON CONFLICT (user_id, route_id) DO NOTHING
`
	if _, err := r.pool.Exec(ctx, q, userID, routeID); err != nil {
		return fmt.Errorf("add favorite route: %w", err)
	}
	return nil
}

func (r *Repository) DeleteFavoriteRoute(ctx context.Context, userID, routeID string) error {
	const q = `
DELETE FROM user_favorite_routes
WHERE user_id = $1::uuid AND route_id = $2
`
	if _, err := r.pool.Exec(ctx, q, userID, routeID); err != nil {
		return fmt.Errorf("delete favorite route: %w", err)
	}
	return nil
}

// ============================================================================
// Vehicles (realtime)
// ============================================================================

func (r *Repository) GetVehicles(ctx context.Context) ([]models.Vehicle, error) {
	// Canonical route_id: realtime row first, else trips.route_id; names from routes table.
	const q = `
SELECT
	x.vehicle_id,
	x.trip_id,
	x.route_id,
	COALESCE(NULLIF(TRIM(r.route_short_name), ''), ''),
	COALESCE(NULLIF(TRIM(r.route_long_name), ''), ''),
	x.lat,
	x.lon,
	x.bearing,
	x.speed,
	x.updated_at,
	true AS is_live
FROM (
	SELECT
		v.vehicle_id,
		COALESCE(v.trip_id, '') AS trip_id,
		COALESCE(
			NULLIF(TRIM(COALESCE(v.route_id, '')), ''),
			NULLIF(TRIM(COALESCE(t.route_id, '')), ''),
			''
		) AS route_id,
		COALESCE(v.latitude, 0) AS lat,
		COALESCE(v.longitude, 0) AS lon,
		v.bearing,
		v.speed,
		COALESCE(v.updated_at, NOW()) AS updated_at
	FROM vehicle_positions_current v
	LEFT JOIN trips t ON t.trip_id = v.trip_id
	WHERE v.latitude IS NOT NULL
	  AND v.longitude IS NOT NULL
	  AND COALESCE(v.updated_at, NOW()) >= NOW() - INTERVAL '2 minutes'
) x
LEFT JOIN routes r ON r.route_id = x.route_id AND x.route_id <> ''
`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query vehicles: %w", err)
	}
	defer rows.Close()

	out := make([]models.Vehicle, 0, 64)
	for rows.Next() {
		var v models.Vehicle
		if err = rows.Scan(&v.VehicleID, &v.TripID, &v.RouteID, &v.ShortName, &v.LongName, &v.Lat, &v.Lon, &v.Bearing, &v.Speed, &v.UpdatedAt, &v.IsLive); err != nil {
			return nil, fmt.Errorf("scan vehicle: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// UpsertVehiclePosition writes a latest GPS row by vehicle_id and refreshes updated_at.
func (r *Repository) UpsertVehiclePosition(ctx context.Context, v models.Vehicle) error {
	const q = `
INSERT INTO vehicle_positions_current (
	vehicle_id, trip_id, route_id, latitude, longitude, bearing, speed, updated_at
)
VALUES ($1, NULLIF($2,''), NULLIF($3,''), $4, $5, $6, $7, COALESCE($8, NOW()))
ON CONFLICT (vehicle_id) DO UPDATE
SET
	trip_id = EXCLUDED.trip_id,
	route_id = EXCLUDED.route_id,
	latitude = EXCLUDED.latitude,
	longitude = EXCLUDED.longitude,
	bearing = EXCLUDED.bearing,
	speed = EXCLUDED.speed,
	updated_at = EXCLUDED.updated_at
`
	_, err := r.pool.Exec(ctx, q, v.VehicleID, v.TripID, v.RouteID, v.Lat, v.Lon, v.Bearing, v.Speed, v.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert vehicle position: %w", err)
	}
	return nil
}

type routeDisplayName struct {
	short, long string
}

func (r *Repository) lookupRouteDisplayNames(ctx context.Context, routeIDs []string) (map[string]routeDisplayName, error) {
	out := make(map[string]routeDisplayName)
	if len(routeIDs) == 0 {
		return out, nil
	}
	const q = `
SELECT route_id,
	COALESCE(NULLIF(TRIM(route_short_name), ''), ''),
	COALESCE(NULLIF(TRIM(route_long_name), ''), '')
FROM routes
WHERE route_id = ANY($1::text[])
`
	rows, err := r.pool.Query(ctx, q, routeIDs)
	if err != nil {
		return nil, fmt.Errorf("lookup route display names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, sn, ln string
		if err = rows.Scan(&id, &sn, &ln); err != nil {
			return nil, fmt.Errorf("scan route display name: %w", err)
		}
		out[id] = routeDisplayName{short: sn, long: ln}
	}
	return out, rows.Err()
}

// EnrichVehiclesWithInferredRoutes fills route_id and route names when the realtime row has no route,
// using the same nearest-stop + shape heuristics as unassigned_hints. Sets RouteInferred on those rows.
func (r *Repository) EnrichVehiclesWithInferredRoutes(ctx context.Context, vehicles []models.Vehicle) error {
	var ids []string
	seen := map[string]struct{}{}
	for i := range vehicles {
		if strings.TrimSpace(vehicles[i].RouteID) != "" {
			continue
		}
		id := strings.TrimSpace(vehicles[i].VehicleID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	hints, err := r.GetUnassignedVehicleHints(ctx, len(ids), ids)
	if err != nil {
		return err
	}
	routePick := make(map[string]string, len(hints))
	for _, h := range hints {
		picked := strings.TrimSpace(h.BestRouteID)
		if picked == "" && len(h.PossibleRouteIDs) == 1 {
			picked = h.PossibleRouteIDs[0]
		}
		if picked == "" && len(h.RankedRoutes) > 0 && h.RankedRoutes[0].DistanceM < 1e14 && h.RankedRoutes[0].DistanceM <= 1200 {
			picked = h.RankedRoutes[0].RouteID
		}
		if picked != "" {
			routePick[h.VehicleID] = picked
		}
	}
	var routeIDs []string
	seenR := map[string]struct{}{}
	for _, rid := range routePick {
		if _, ok := seenR[rid]; ok {
			continue
		}
		seenR[rid] = struct{}{}
		routeIDs = append(routeIDs, rid)
	}
	names, err := r.lookupRouteDisplayNames(ctx, routeIDs)
	if err != nil {
		return err
	}
	for i := range vehicles {
		if strings.TrimSpace(vehicles[i].RouteID) != "" {
			continue
		}
		rid, ok := routePick[vehicles[i].VehicleID]
		if !ok || rid == "" {
			continue
		}
		vehicles[i].RouteID = rid
		vehicles[i].RouteInferred = true
		if n, ok := names[rid]; ok {
			vehicles[i].ShortName = n.short
			vehicles[i].LongName = n.long
		}
	}
	return nil
}

// GetRoutesWithLiveVehicleCounts returns per-route vehicle counts (canonical routes.route_id only)
// and how many vehicles could not be tied to any route. Routes slice omits zero counts.
func (r *Repository) GetRoutesWithLiveVehicleCounts(ctx context.Context) (models.RoutesWithLiveVehiclesPayload, error) {
	const base = `
WITH resolved AS (
	SELECT
		v.vehicle_id,
		COALESCE(
			NULLIF(TRIM(COALESCE(v.route_id, '')), ''),
			NULLIF(TRIM(COALESCE(t.route_id, '')), ''),
			''
		) AS route_id
	FROM vehicle_positions_current v
	LEFT JOIN trips t ON t.trip_id = v.trip_id
WHERE v.latitude IS NOT NULL
  AND v.longitude IS NOT NULL
  AND COALESCE(v.updated_at, NOW()) >= NOW() - INTERVAL '2 minutes'
)
`
	var out models.RoutesWithLiveVehiclesPayload

	var total int
	if err := r.pool.QueryRow(ctx, base+`SELECT COUNT(*)::int FROM resolved`).Scan(&total); err != nil {
		return out, fmt.Errorf("count live vehicles: %w", err)
	}
	out.TotalVehicles = total

	if err := r.pool.QueryRow(ctx, base+`SELECT COUNT(*)::int FROM resolved WHERE route_id = ''`).Scan(&out.UnassignedVehicleCount); err != nil {
		return out, fmt.Errorf("count unassigned vehicles: %w", err)
	}

	qAgg := base + `
SELECT route_id, COUNT(*)::int
FROM resolved
WHERE route_id <> ''
GROUP BY route_id
ORDER BY route_id
`
	rows, err := r.pool.Query(ctx, qAgg)
	if err != nil {
		return out, fmt.Errorf("aggregate vehicles by route: %w", err)
	}
	defer rows.Close()

	out.Routes = make([]models.RouteLiveVehicleCount, 0, 64)
	for rows.Next() {
		var row models.RouteLiveVehicleCount
		if err = rows.Scan(&row.RouteID, &row.LiveVehicleCount); err != nil {
			return out, fmt.Errorf("scan route live count: %w", err)
		}
		if row.LiveVehicleCount > 0 {
			out.Routes = append(out.Routes, row)
		}
	}
	return out, rows.Err()
}

const maxPossibleRoutesPerHint = 8

// GetUnassignedVehicleHints returns unassigned positions with nearest-stop distance and possible_route_ids.
// filterVehicleIDs: when non-empty, only those vehicle_ids are considered (for map vehicle enrichment); pass nil
// for the usual “first N unassigned” behavior capped by maxVehicles.
func (r *Repository) GetUnassignedVehicleHints(ctx context.Context, maxVehicles int, filterVehicleIDs []string) ([]models.UnassignedVehicleHint, error) {
	limit := maxVehicles
	if limit <= 0 {
		limit = 80
	}
	if len(filterVehicleIDs) == 0 && limit > 150 {
		limit = 150
	}
	if len(filterVehicleIDs) > 0 {
		limit = len(filterVehicleIDs)
		if limit > 500 {
			limit = 500
			filterVehicleIDs = filterVehicleIDs[:500]
		}
	}
	const q = `
WITH unassigned AS (
	SELECT
		v.vehicle_id,
		COALESCE(v.trip_id, '') AS trip_id,
		v.latitude AS lat,
		v.longitude AS lon,
		v.bearing,
		v.speed
	FROM vehicle_positions_current v
	LEFT JOIN trips t ON t.trip_id = v.trip_id
	WHERE v.latitude IS NOT NULL
	  AND v.longitude IS NOT NULL
	  AND COALESCE(v.updated_at, NOW()) >= NOW() - INTERVAL '2 minutes'
	  AND COALESCE(
			NULLIF(TRIM(COALESCE(v.route_id, '')), ''),
			NULLIF(TRIM(COALESCE(t.route_id, '')), ''),
			''
		) = ''
	  AND (
			array_length($2::text[], 1) IS NULL
			OR array_length($2::text[], 1) = 0
			OR v.vehicle_id = ANY($2::text[])
	  )
	LIMIT $1
),
nearest AS (
	SELECT
		u.vehicle_id,
		u.trip_id,
		u.lat,
		u.lon,
		u.bearing,
		u.speed,
		s.stop_id AS nearest_stop_id,
		ST_Distance(
			ST_SetSRID(ST_MakePoint(s.stop_lon, s.stop_lat), 4326)::geography,
			ST_SetSRID(ST_MakePoint(u.lon, u.lat), 4326)::geography
		)::double precision AS nearest_stop_m
	FROM unassigned u
	CROSS JOIN LATERAL (
		SELECT stop_id, stop_lon, stop_lat
		FROM stops
		WHERE stop_lat IS NOT NULL AND stop_lon IS NOT NULL
		ORDER BY
			ST_SetSRID(ST_MakePoint(stop_lon, stop_lat), 4326)::geography
			<-> ST_SetSRID(ST_MakePoint(u.lon, u.lat), 4326)::geography
		LIMIT 1
	) s
)
SELECT
	n.vehicle_id,
	n.trip_id,
	n.lat,
	n.lon,
	n.bearing,
	n.speed,
	n.nearest_stop_id,
	n.nearest_stop_m,
	COALESCE(string_agg(DISTINCT t.route_id, ',' ORDER BY t.route_id), '')
FROM nearest n
JOIN stop_times st ON st.stop_id = n.nearest_stop_id
JOIN trips t ON t.trip_id = st.trip_id AND COALESCE(TRIM(t.route_id), '') <> ''
GROUP BY
	n.vehicle_id, n.trip_id, n.lat, n.lon, n.bearing, n.speed, n.nearest_stop_id, n.nearest_stop_m
ORDER BY n.vehicle_id
`
	filterArg := []string{}
	if len(filterVehicleIDs) > 0 {
		filterArg = filterVehicleIDs
	}
	rows, err := r.pool.Query(ctx, q, limit, filterArg)
	if err != nil {
		return nil, fmt.Errorf("unassigned vehicle hints: %w", err)
	}
	defer rows.Close()

	out := make([]models.UnassignedVehicleHint, 0, limit)
	for rows.Next() {
		var (
			h         models.UnassignedVehicleHint
			bear, spd sql.NullFloat64
			routesCSV string
		)
		if err = rows.Scan(
			&h.VehicleID, &h.TripID, &h.Lat, &h.Lon, &bear, &spd,
			&h.NearestStopID, &h.NearestStopDistanceM, &routesCSV,
		); err != nil {
			return nil, fmt.Errorf("scan unassigned hint: %w", err)
		}
		if bear.Valid {
			f := bear.Float64
			h.Bearing = &f
		}
		if spd.Valid {
			f := spd.Float64
			h.Speed = &f
		}
		h.PossibleRouteIDs = splitRouteCSV(routesCSV, maxPossibleRoutesPerHint)
		r.attachShapeProximityRanking(ctx, &h)
		out = append(out, h)
	}
	return out, rows.Err()
}

const (
	maxRoutesToRankPerHint = 6
	bestRouteMaxDistM      = 720.0
	bestRouteSecondGapM    = 14.0
)

// attachShapeProximityRanking fills ranked_routes and optionally best_route_id using min distance
// from the vehicle point to each candidate route’s dominant GTFS shape (vertex sampling). Best is
// only set when the closest line is within bestRouteMaxDistM and clearly nearer than the runner-up.
func (r *Repository) attachShapeProximityRanking(ctx context.Context, h *models.UnassignedVehicleHint) {
	ids := h.PossibleRouteIDs
	if len(ids) == 0 {
		return
	}
	toRank := ids
	if len(toRank) > maxRoutesToRankPerHint {
		toRank = ids[:maxRoutesToRankPerHint]
	}
	ranked, err := r.rankRoutesByShapeProximity(ctx, h.Lon, h.Lat, toRank)
	if err != nil {
		return
	}
	h.RankedRoutes = ranked
	if len(ids) == 1 {
		h.BestRouteID = ids[0]
		return
	}
	if len(ranked) == 0 {
		return
	}
	first := ranked[0]
	if first.DistanceM > bestRouteMaxDistM {
		return
	}
	if len(ranked) == 1 {
		h.BestRouteID = first.RouteID
		return
	}
	if ranked[1].DistanceM-first.DistanceM >= bestRouteSecondGapM {
		h.BestRouteID = first.RouteID
	}
	// Last resort: closest shape within ~900 m so clients still get a line when gaps are ambiguous.
	if h.BestRouteID == "" && len(h.RankedRoutes) > 0 {
		d := h.RankedRoutes[0].DistanceM
		if d < 1e14 && d <= 900 {
			h.BestRouteID = h.RankedRoutes[0].RouteID
		}
	}
}

func (r *Repository) rankRoutesByShapeProximity(ctx context.Context, lon, lat float64, routeIDs []string) ([]models.RouteProximityRank, error) {
	if len(routeIDs) == 0 {
		return nil, nil
	}
	const q = `
SELECT u.route_id,
	COALESCE(MIN(
		ST_Distance(
			ST_SetSRID(ST_MakePoint($1::float8, $2::float8), 4326)::geography,
			ST_SetSRID(ST_MakePoint(sh.shape_pt_lon, sh.shape_pt_lat), 4326)::geography
		)
	), 1e15)::float8 AS dist_m
FROM unnest($3::text[]) AS u(route_id)
LEFT JOIN LATERAL (
	SELECT sh.shape_pt_lon, sh.shape_pt_lat
	FROM shapes sh
	INNER JOIN (
		SELECT t.shape_id
		FROM trips t
		WHERE t.route_id = u.route_id
		  AND t.shape_id IS NOT NULL
		  AND TRIM(COALESCE(t.shape_id::text, '')) <> ''
		GROUP BY t.shape_id
		ORDER BY COUNT(*) DESC
		LIMIT 1
	) rep ON TRIM(BOTH FROM rep.shape_id::text) = TRIM(BOTH FROM sh.shape_id::text)
	WHERE sh.shape_pt_lat IS NOT NULL AND sh.shape_pt_lon IS NOT NULL
) sh ON true
GROUP BY u.route_id
ORDER BY dist_m ASC
`
	rows, err := r.pool.Query(ctx, q, lon, lat, routeIDs)
	if err != nil {
		return nil, fmt.Errorf("rank routes by shape proximity: %w", err)
	}
	defer rows.Close()

	out := make([]models.RouteProximityRank, 0, len(routeIDs))
	for rows.Next() {
		var rr models.RouteProximityRank
		if err = rows.Scan(&rr.RouteID, &rr.DistanceM); err != nil {
			return nil, fmt.Errorf("scan route proximity: %w", err)
		}
		out = append(out, rr)
	}
	return out, rows.Err()
}

func splitRouteCSV(csv string, max int) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
		if len(out) >= max {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
