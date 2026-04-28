package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"backend_mobile_app_go/internal/models"

	"github.com/jackc/pgx/v5"
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
}

func (r *Repository) SearchStops(ctx context.Context, p StopSearchParams) ([]models.StopSummary, int, error) {
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

	where := ""
	if len(filters) > 0 {
		where = "WHERE " + strings.Join(filters, " AND ")
	}

	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM stops %s`, where)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
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
candidates AS (
	SELECT
		st.trip_id,
		t.route_id,
		t.trip_headsign,
		a.service_date,
		(a.service_date::timestamp + st.arrival_time::interval) AT TIME ZONE current_setting('TimeZone') AS scheduled_time
	FROM stop_times st
	JOIN trips t ON t.trip_id = st.trip_id
	JOIN active_services a ON a.service_id = t.service_id
	WHERE st.stop_id = $2
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
LEFT JOIN trip_update_stop_times_current rt
	ON rt.trip_id = c.trip_id AND rt.stop_id = $2
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

func (r *Repository) GetRouteStops(ctx context.Context, routeID, directionID string) ([]models.StopSummary, error) {
	base := `
SELECT DISTINCT s.stop_id, COALESCE(s.stop_name,''), COALESCE(s.stop_code,''), COALESCE(s.stop_lat,0), COALESCE(s.stop_lon,0)
FROM stops s
JOIN stop_times st ON st.stop_id = s.stop_id
JOIN trips t ON t.trip_id = st.trip_id
WHERE t.route_id = $1
`
	args := []any{routeID}
	if directionID != "" {
		base += " AND COALESCE(t.direction_id, '') = $2\n"
		args = append(args, directionID)
	}
	base += `
ORDER BY 2 NULLS LAST
`
	rows, err := r.pool.Query(ctx, base, args...)
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

func (r *Repository) GetRouteShape(ctx context.Context, routeID, directionID string) (*models.RouteShape, error) {
	q := `
WITH selected_shape AS (
	SELECT shape_id
	FROM trips
	WHERE route_id = $1
	  AND shape_id IS NOT NULL
	  AND shape_id <> ''
`
	args := []any{routeID}
	if directionID != "" {
		q += " AND COALESCE(direction_id, '') = $2\n"
		args = append(args, directionID)
	}
	q += `
	GROUP BY shape_id
	ORDER BY COUNT(*) DESC
	LIMIT 1
)
SELECT s.shape_id, s.shape_pt_sequence, s.shape_pt_lat, s.shape_pt_lon
FROM shapes s
JOIN selected_shape ss ON ss.shape_id = s.shape_id
WHERE s.shape_pt_lat IS NOT NULL AND s.shape_pt_lon IS NOT NULL
ORDER BY s.shape_pt_sequence
`
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query route shape: %w", err)
	}
	defer rows.Close()

	shape := &models.RouteShape{
		RouteID: routeID,
		Points:  make([]models.RouteShapePoint, 0, 128),
	}
	for rows.Next() {
		var (
			shapeID string
			pt      models.RouteShapePoint
		)
		if err = rows.Scan(&shapeID, &pt.Sequence, &pt.Lat, &pt.Lon); err != nil {
			return nil, fmt.Errorf("scan route shape point: %w", err)
		}
		shape.ShapeID = shapeID
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

// ============================================================================
// Calendar
// ============================================================================

func (r *Repository) GetServiceCalendarOnDate(ctx context.Context, date string) ([]models.ServiceCalendarDay, error) {
	const q = `
SELECT c.service_id, 'calendar' AS source
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
SELECT ex.service_id, 'exception_added' AS source
FROM calendar_dates ex
WHERE ex.date = to_char($1::date, 'YYYYMMDD')
  AND ex.exception_type = '1'
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
SELECT service_id, date, COALESCE(exception_type,'')
FROM calendar_dates
WHERE date >= to_char($1::date, 'YYYYMMDD')
  AND date <= to_char($2::date, 'YYYYMMDD')
ORDER BY date, service_id
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

// ============================================================================
// Vehicles (realtime)
// ============================================================================

func (r *Repository) GetVehicles(ctx context.Context) ([]models.Vehicle, error) {
	const q = `
SELECT vehicle_id, COALESCE(trip_id,''), COALESCE(route_id,''),
	COALESCE(latitude,0), COALESCE(longitude,0),
	bearing, speed
FROM vehicle_positions_current
WHERE latitude IS NOT NULL AND longitude IS NOT NULL
`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query vehicles: %w", err)
	}
	defer rows.Close()

	out := make([]models.Vehicle, 0, 64)
	for rows.Next() {
		var v models.Vehicle
		if err = rows.Scan(&v.VehicleID, &v.TripID, &v.RouteID, &v.Lat, &v.Lon, &v.Bearing, &v.Speed); err != nil {
			return nil, fmt.Errorf("scan vehicle: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

