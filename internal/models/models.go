package models

import "time"

type Mode string

const (
	ModeNormal     Mode = "normal"
	ModeStaticOnly Mode = "static_only"
)

type APIStatus struct {
	Mode               Mode       `json:"mode"`
	LastSuccessfulSync *time.Time `json:"last_successful_sync,omitempty"`
}

type ResponseEnvelope struct {
	Status APIStatus `json:"status"`
	Data   any       `json:"data"`
	Meta   *Meta     `json:"meta,omitempty"`
}

type Meta struct {
	Total           int      `json:"total,omitempty"`
	Limit           int      `json:"limit,omitempty"`
	Page            int      `json:"page,omitempty"`
	HasNext         bool     `json:"has_next,omitempty"`
	NextCursor      string   `json:"next_cursor,omitempty"`
	ServiceDate     string   `json:"service_date,omitempty"`
	RequestedRoutes int      `json:"requested_routes,omitempty"`  // overlay: ids in the query
	MissingRouteIds []string `json:"missing_route_ids,omitempty"` // overlay: no drawable geometry / unknown route
}

type Vehicle struct {
	VehicleID string `json:"vehicle_id"`
	TripID    string `json:"trip_id,omitempty"`
	// RouteID is always canonical GTFS routes.route_id when resolvable (vehicle row, else trips.route_id).
	RouteID   string `json:"route_id,omitempty"`
	ShortName string `json:"route_short_name,omitempty"` // from routes; display / fallback join
	LongName  string `json:"route_long_name,omitempty"`
	// RouteInferred is true when RouteID was filled from nearest-stop / shape heuristics (feed omitted route).
	RouteInferred bool     `json:"route_inferred,omitempty"`
	Lat           float64  `json:"lat"`
	Lon           float64  `json:"lon"`
	Bearing       *float64 `json:"bearing,omitempty"`
	Speed         *float64 `json:"speed,omitempty"`
}

// RouteLiveVehicleCount is one route that currently has at least one vehicle in the feed.
type RouteLiveVehicleCount struct {
	RouteID          string `json:"route_id"`
	LiveVehicleCount int    `json:"live_vehicle_count"`
}

// RoutesWithLiveVehiclesPayload is GET /v1/map/routes-with-live-vehicles.
// Routes lists only route_id values with count ≥ 1; merge with static route catalog on the client for zeros.
type RoutesWithLiveVehiclesPayload struct {
	Routes                 []RouteLiveVehicleCount `json:"routes"`
	UnassignedVehicleCount int                     `json:"unassigned_vehicle_count"`
	TotalVehicles          int                     `json:"total_vehicles"`
	// UnassignedHints is set only when includeUnassignedHints=1: location + heuristic candidate routes (nearest stop).
	UnassignedHints []UnassignedVehicleHint `json:"unassigned_hints,omitempty"`
}

// RouteProximityRank orders candidate routes by min distance (m) from the vehicle point to the route’s
// representative GTFS shape polyline (vertex sampling). Lower is closer to that line on the map.
type RouteProximityRank struct {
	RouteID   string  `json:"route_id"`
	DistanceM float64 `json:"distance_m"`
}

// UnassignedVehicleHint is a vehicle with no canonical route_id, plus optional geographic hints.
// PossibleRouteIDs are routes that serve the nearest stop — not proof the vehicle is on that line.
// RankedRoutes / BestRouteID disambiguate when several routes share that stop, using distance-to-shape (heuristic).
type UnassignedVehicleHint struct {
	VehicleID            string               `json:"vehicle_id"`
	TripID               string               `json:"trip_id,omitempty"`
	Lat                  float64              `json:"lat"`
	Lon                  float64              `json:"lon"`
	Bearing              *float64             `json:"bearing,omitempty"`
	Speed                *float64             `json:"speed,omitempty"`
	NearestStopID        string               `json:"nearest_stop_id,omitempty"`
	NearestStopDistanceM float64              `json:"nearest_stop_distance_m,omitempty"`
	PossibleRouteIDs     []string             `json:"possible_route_ids,omitempty"`
	RankedRoutes         []RouteProximityRank `json:"ranked_routes,omitempty"`
	// BestRouteID is set when a single candidate exists, or when the closest shape is clearly nearer than the rest (heuristic).
	BestRouteID string `json:"best_route_id,omitempty"`
}

type StopSummary struct {
	StopID     string  `json:"stop_id"`
	ID         string  `json:"id,omitempty"`           // same as stop_id (map clients)
	GtfsStopID string  `json:"gtfs_stop_id,omitempty"` // same as stop_id (alt parsers)
	StopName   string  `json:"stop_name,omitempty"`
	StopCode   string  `json:"stop_code,omitempty"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
}

// EnrichStopSummariesForMap sets id and gtfs_stop_id from stop_id for map overlay consumers.
func EnrichStopSummariesForMap(stops []StopSummary) {
	for i := range stops {
		if stops[i].StopID == "" {
			continue
		}
		stops[i].ID = stops[i].StopID
		stops[i].GtfsStopID = stops[i].StopID
	}
}

type Stop struct {
	StopID             string  `json:"stop_id"`
	StopCode           string  `json:"stop_code,omitempty"`
	StopName           string  `json:"stop_name,omitempty"`
	StopDesc           string  `json:"stop_desc,omitempty"`
	Lat                float64 `json:"lat"`
	Lon                float64 `json:"lon"`
	ZoneID             string  `json:"zone_id,omitempty"`
	LocationType       string  `json:"location_type,omitempty"`
	ParentStation      string  `json:"parent_station,omitempty"`
	WheelchairBoarding string  `json:"wheelchair_boarding,omitempty"`
	PlatformCode       string  `json:"platform_code,omitempty"`
}

type Route struct {
	RouteID        string `json:"route_id"`
	AgencyID       string `json:"agency_id,omitempty"`
	ShortName      string `json:"short_name,omitempty"`
	LongName       string `json:"long_name,omitempty"`
	RouteType      string `json:"route_type,omitempty"`
	RouteColor     string `json:"route_color,omitempty"`
	RouteTextColor string `json:"route_text_color,omitempty"`
}

// RouteMapLite is a minimal route row for map UI (badges / colors) without extra GTFS fields.
type RouteMapLite struct {
	RouteID        string `json:"route_id"`
	ShortName      string `json:"short_name,omitempty"`
	LongName       string `json:"long_name,omitempty"`
	RouteColor     string `json:"route_color,omitempty"`
	RouteTextColor string `json:"route_text_color,omitempty"`
}

// MapStaticBundle is one response for the live map: nearest stops + all route colors/ids.
type MapStaticBundle struct {
	Stops  []StopSummary  `json:"stops"`
	Routes []RouteMapLite `json:"routes"`
}

// MapRouteLeg is one direction (inbound/outbound): polyline plus stops along that pattern.
// Stops are in stop_sequence order; shape points follow GTFS shape (not road-snapped).
type MapRouteLeg struct {
	DirectionID string            `json:"direction_id"`
	Headsign    string            `json:"headsign,omitempty"`
	ShapeID     string            `json:"shape_id,omitempty"`
	Points      []RouteShapePoint `json:"points"`
	TotalPoints int               `json:"total_points,omitempty"`
	Stops       []StopSummary     `json:"stops"`
}

// MapRouteWithGeometry is everything the map needs to draw one line + markers for a route.
type MapRouteWithGeometry struct {
	RouteID        string        `json:"route_id"`
	ShortName      string        `json:"short_name,omitempty"`
	LongName       string        `json:"long_name,omitempty"`
	RouteColor     string        `json:"route_color,omitempty"`
	RouteTextColor string        `json:"route_text_color,omitempty"`
	Legs           []MapRouteLeg `json:"legs"`
	AllStops       []StopSummary `json:"all_stops,omitempty"` // deduped union of leg.stops (route-level pins)
	Stops          []StopSummary `json:"stops,omitempty"`     // same as AllStops (some parsers expect route.stops)
}

// MapRoutesOverlayPayload is the data envelope for GET /v1/map/routes-overlay.
type MapRoutesOverlayPayload struct {
	Routes []MapRouteWithGeometry `json:"routes"`
}

// MapNormalizedPayload is a mobile-optimized relational shape:
// - routes/stops are ID-indexed dictionaries for O(1) lookups
// - junctions maps route_id -> ordered stop_ids
// - route_geometries keeps encoded polylines separate from entities
type MapNormalizedPayload struct {
	Routes          map[string]RouteMapLite `json:"routes"`
	Stops           map[string]StopSummary  `json:"stops"`
	Junctions       map[string][]string     `json:"junctions"`
	RouteGeometries map[string]string       `json:"route_geometries"`
}

// StopArrivalLite is a flat, pre-sorted row for mobile timetable rendering.
type StopArrivalLite struct {
	TripID         string    `json:"trip_id"`
	RouteID        string    `json:"route_id,omitempty"`
	RouteShortName string    `json:"route_short_name,omitempty"`
	Headsign       string    `json:"headsign,omitempty"`
	ScheduledTime  time.Time `json:"scheduled_time"`
	EstimatedTime  time.Time `json:"estimated_time"`
	IsRealtime     bool      `json:"is_realtime"`
}

// ArrivalsNormalizedPayload groups pre-sorted arrivals by stop_id.
type ArrivalsNormalizedPayload struct {
	Arrivals map[string][]StopArrivalLite `json:"arrivals"`
}

type Trip struct {
	TripID       string `json:"trip_id"`
	RouteID      string `json:"route_id,omitempty"`
	ServiceID    string `json:"service_id,omitempty"`
	Headsign     string `json:"headsign,omitempty"`
	ShortName    string `json:"short_name,omitempty"`
	DirectionID  string `json:"direction_id,omitempty"`
	BlockID      string `json:"block_id,omitempty"`
	ShapeID      string `json:"shape_id,omitempty"`
	Wheelchair   string `json:"wheelchair_accessible,omitempty"`
	BikesAllowed string `json:"bikes_allowed,omitempty"`
}

type TripStopTime struct {
	StopID        string `json:"stop_id"`
	StopName      string `json:"stop_name,omitempty"`
	StopSequence  int    `json:"stop_sequence"`
	ArrivalTime   string `json:"arrival_time,omitempty"`
	DepartureTime string `json:"departure_time,omitempty"`
	StopHeadsign  string `json:"stop_headsign,omitempty"`
	PickupType    string `json:"pickup_type,omitempty"`
	DropOffType   string `json:"drop_off_type,omitempty"`
}

type Departure struct {
	TripID         string    `json:"trip_id"`
	RouteID        string    `json:"route_id,omitempty"`
	RouteShortName string    `json:"route_short_name,omitempty"`
	Headsign       string    `json:"headsign,omitempty"`
	ScheduledTime  time.Time `json:"scheduled_time"`
	EstimatedTime  time.Time `json:"estimated_time"`
	IsRealtime     bool      `json:"is_realtime"`
}

type ServiceCalendarDay struct {
	ServiceID string `json:"service_id"`
	Source    string `json:"source"`
}

// CalendarService is one row from GTFS calendar.txt (weekly service window).
type CalendarService struct {
	ServiceID string `json:"service_id"`
	Monday    string `json:"monday"`
	Tuesday   string `json:"tuesday"`
	Wednesday string `json:"wednesday"`
	Thursday  string `json:"thursday"`
	Friday    string `json:"friday"`
	Saturday  string `json:"saturday"`
	Sunday    string `json:"sunday"`
	StartDate string `json:"start_date"` // YYYY-MM-DD
	EndDate   string `json:"end_date"`   // YYYY-MM-DD
}

type CalendarException struct {
	ServiceID     string `json:"service_id"`
	Date          string `json:"date"`           // YYYY-MM-DD
	ExceptionType string `json:"exception_type"` // "1" added, "2" removed (GTFS)
}

// ActiveServiceOnDay is one service_id that runs on date D after calendar + calendar_dates rules.
type ActiveServiceOnDay struct {
	ServiceID string `json:"service_id"`
	Source    string `json:"source"` // calendar | exception_added
	// Filled when detail=true (GET /v1/gtfs/calendar/day?detail=true)
	Calendar        *CalendarService    `json:"calendar,omitempty"`
	ExceptionsToday []CalendarException `json:"exceptions_today,omitempty"` // calendar_dates rows for this service_id on D
	Display         *ServiceDisplay     `json:"display,omitempty"`
}

// ServiceDisplay is derived metadata for UI chips (replaces static SERVICE_META where possible).
type ServiceDisplay struct {
	Label           string   `json:"label"`
	Description     string   `json:"description,omitempty"`
	RouteShortNames []string `json:"route_short_names,omitempty"`
	TripCount       int      `json:"trip_count"`
	HasTrips        bool     `json:"has_trips"`
}

// CalendarDayPayload is GET /v1/gtfs/calendar/day.
type CalendarDayPayload struct {
	Date               string               `json:"date"` // YYYY-MM-DD
	Services           []ActiveServiceOnDay `json:"services"`
	CalendarDateEvents []CalendarException  `json:"calendar_date_events,omitempty"` // all calendar_dates rows on D
	FeedCalendarWindow *FeedCalendarWindow  `json:"feed_calendar_window,omitempty"` // when include=window
}

// FeedCalendarWindow is min/max dates across calendar + calendar_dates (static GTFS coverage).
type FeedCalendarWindow struct {
	MinDate string `json:"min_date"` // YYYY-MM-DD
	MaxDate string `json:"max_date"`
}

// FeedActivePayload is GET /v1/feed/active.
type FeedActivePayload struct {
	CalendarWindow     *FeedCalendarWindow `json:"calendar_window,omitempty"`
	LastSuccessfulSync *time.Time          `json:"last_successful_sync,omitempty"`
}

// APIError is a structured error body (used for calendar/feed and extensible elsewhere).
type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// ErrorEnvelope wraps APIError for JSON responses.
type ErrorEnvelope struct {
	Error APIError `json:"error"`
}

type RouteShapePoint struct {
	Sequence int     `json:"sequence"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
}

type RouteShape struct {
	RouteID     string            `json:"route_id"`
	ShapeID     string            `json:"shape_id"`
	Points      []RouteShapePoint `json:"points"`
	TotalPoints int               `json:"total_points,omitempty"`
}

type RouteShapeEncoded struct {
	RouteID         string `json:"route_id"`
	ShapeID         string `json:"shape_id"`
	EncodedPolyline string `json:"encoded_polyline"`
	TotalPoints     int    `json:"total_points,omitempty"`
}

type RouteDirection struct {
	DirectionID string `json:"direction_id"`
	Headsign    string `json:"headsign,omitempty"`
	TripCount   int    `json:"trip_count"`
}

type FavoriteRoute struct {
	RouteID string `json:"route_id"`
}
