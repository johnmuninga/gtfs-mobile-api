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
	Total       int    `json:"total,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Page        int    `json:"page,omitempty"`
	HasNext     bool   `json:"has_next,omitempty"`
	ServiceDate string `json:"service_date,omitempty"`
}

type Vehicle struct {
	VehicleID string   `json:"vehicle_id"`
	TripID    string   `json:"trip_id,omitempty"`
	RouteID   string   `json:"route_id,omitempty"`
	Lat       float64  `json:"lat"`
	Lon       float64  `json:"lon"`
	Bearing   *float64 `json:"bearing,omitempty"`
	Speed     *float64 `json:"speed,omitempty"`
}

type StopSummary struct {
	StopID   string  `json:"stop_id"`
	StopName string  `json:"stop_name,omitempty"`
	StopCode string  `json:"stop_code,omitempty"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
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

type Trip struct {
	TripID        string `json:"trip_id"`
	RouteID       string `json:"route_id,omitempty"`
	ServiceID     string `json:"service_id,omitempty"`
	Headsign      string `json:"headsign,omitempty"`
	ShortName     string `json:"short_name,omitempty"`
	DirectionID   string `json:"direction_id,omitempty"`
	BlockID       string `json:"block_id,omitempty"`
	ShapeID       string `json:"shape_id,omitempty"`
	Wheelchair    string `json:"wheelchair_accessible,omitempty"`
	BikesAllowed  string `json:"bikes_allowed,omitempty"`
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

type CalendarException struct {
	ServiceID     string `json:"service_id"`
	Date          string `json:"date"`
	ExceptionType string `json:"exception_type"`
}

type RouteShapePoint struct {
	Sequence int     `json:"sequence"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
}

type RouteShape struct {
	RouteID string            `json:"route_id"`
	ShapeID string            `json:"shape_id"`
	Points  []RouteShapePoint `json:"points"`
}

type RouteDirection struct {
	DirectionID string `json:"direction_id"`
	Headsign    string `json:"headsign,omitempty"`
	TripCount   int    `json:"trip_count"`
}
