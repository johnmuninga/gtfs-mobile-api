package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"backend_mobile_app_go/internal/cache"
	"backend_mobile_app_go/internal/config"
	"backend_mobile_app_go/internal/models"
	"backend_mobile_app_go/internal/repository"
	"backend_mobile_app_go/internal/snapshot"
	"backend_mobile_app_go/internal/supabaseauth"

	"github.com/jackc/pgx/v5"
)

const (
	defaultStopsLimit  = 50
	maxStopsLimit      = 200
	defaultRoutesLimit = 200
	maxRoutesLimit     = 500
	defaultTripsLimit  = 200
	maxTripsLimit      = 1000
	defaultDepartures  = 20
	maxDepartures      = 50
	defaultWindowMins  = 60
	maxWindowMins      = 360
	requestTimeout     = 5 * time.Second
)

type Server struct {
	cfg      config.Config
	repo     *repository.Repository
	cache    *cache.TTLCache
	snapshot *snapshot.Snapshot
	auth     *supabaseauth.Client
}

func New(cfg config.Config, repo *repository.Repository, snap *snapshot.Snapshot, auth *supabaseauth.Client) *Server {
	return &Server{
		cfg:      cfg,
		repo:     repo,
		cache:    cache.New(2 * time.Minute),
		snapshot: snap,
		auth:     auth,
	}
}

func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /swagger", s.handleSwaggerUI)
	mux.HandleFunc("GET /swagger/", s.handleSwaggerUI)
	mux.HandleFunc("GET /openapi.yaml", s.handleOpenAPI)
	mux.HandleFunc("POST /v1/auth/signup", s.handleAuthSignup)
	mux.HandleFunc("POST /v1/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /v1/auth/verify-otp", s.handleAuthVerifyOTP)

	// Static GTFS endpoints (cacheable)
	staticCache := cacheControl(60)
	dynamicCache := cacheControl(15)

	mux.Handle("GET /v1/gtfs/stops", staticCache(http.HandlerFunc(s.handleSearchStops)))
	mux.Handle("GET /v1/gtfs/stops/{stopId}", staticCache(http.HandlerFunc(s.handleGetStop)))
	mux.Handle("GET /v1/gtfs/stops/{stopId}/departures", dynamicCache(http.HandlerFunc(s.handleStopDepartures)))

	mux.Handle("GET /v1/gtfs/routes", staticCache(http.HandlerFunc(s.handleSearchRoutes)))
	mux.Handle("GET /v1/gtfs/routes/{routeId}", staticCache(http.HandlerFunc(s.handleGetRoute)))
	mux.Handle("GET /v1/gtfs/routes/{routeId}/directions", staticCache(http.HandlerFunc(s.handleRouteDirections)))
	mux.Handle("GET /v1/gtfs/routes/{routeId}/stops", staticCache(http.HandlerFunc(s.handleRouteStops)))
	mux.Handle("GET /v1/gtfs/routes/{routeId}/shape", staticCache(http.HandlerFunc(s.handleRouteShape)))

	mux.Handle("GET /v1/gtfs/trips", staticCache(http.HandlerFunc(s.handleListTrips)))
	mux.Handle("GET /v1/gtfs/trips/{tripId}", staticCache(http.HandlerFunc(s.handleGetTrip)))
	mux.Handle("GET /v1/gtfs/trips/{tripId}/stop-times", staticCache(http.HandlerFunc(s.handleTripStopTimes)))

	mux.Handle("GET /v1/gtfs/calendar/service", staticCache(http.HandlerFunc(s.handleServiceCalendar)))
	mux.Handle("GET /v1/gtfs/calendar/exceptions", staticCache(http.HandlerFunc(s.handleCalendarExceptions)))

	// Realtime
	mux.HandleFunc("GET /v1/map/vehicles", s.handleVehicles)

	return recoverMiddleware(gzipMiddleware(mux))
}

// ============================================================================
// Stops
// ============================================================================

func (s *Server) handleSearchStops(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	q := r.URL.Query()
	limit := clamp(parseIntOr(q.Get("limit"), defaultStopsLimit), 1, maxStopsLimit)
	page := clamp(parseIntOr(q.Get("page"), 1), 1, 1_000_000)

	params := repository.StopSearchParams{
		Search: q.Get("search"),
		Limit:  limit,
		Page:   page,
	}

	hasLat, latErr := optionalFloat(q, "lat")
	hasLon, lonErr := optionalFloat(q, "lon")
	if latErr != nil {
		httpError(w, http.StatusBadRequest, "invalid lat")
		return
	}
	if lonErr != nil {
		httpError(w, http.StatusBadRequest, "invalid lon")
		return
	}
	if (hasLat == nil) != (hasLon == nil) {
		httpError(w, http.StatusBadRequest, "lat and lon must be provided together")
		return
	}
	params.Lat = hasLat
	params.Lon = hasLon

	if rawRadius := q.Get("radius"); rawRadius != "" {
		radius, err := strconv.Atoi(rawRadius)
		if err != nil || radius <= 0 {
			httpError(w, http.StatusBadRequest, "invalid radius")
			return
		}
		params.RadiusMeters = radius
	} else if hasLat != nil {
		params.RadiusMeters = s.cfg.NearbyDefaultRadiusMeters
	}

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	var (
		stops []models.StopSummary
		total int
	)
	if s.snapshot != nil && s.snapshot.Loaded() {
		stops, total = s.snapshot.FilterStops(params.Search, params.Lat, params.Lon, params.RadiusMeters, params.Page, params.Limit)
	} else {
		stops, total, err = s.repo.SearchStops(ctx, params)
		if err != nil {
			log.Printf("search stops: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to search stops")
			return
		}
	}
	hasNext := params.Page*params.Limit < total

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   stops,
		Meta:   &models.Meta{Total: total, Limit: limit, Page: page, HasNext: hasNext},
	})
}

func (s *Server) handleGetStop(w http.ResponseWriter, r *http.Request) {
	stopID := r.PathValue("stopId")
	if stopID == "" {
		httpError(w, http.StatusBadRequest, "missing stop id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	stop, err := s.repo.GetStop(ctx, stopID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "stop not found")
		return
	}
	if err != nil {
		log.Printf("get stop: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch stop")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{Status: status, Data: stop})
}

func (s *Server) handleStopDepartures(w http.ResponseWriter, r *http.Request) {
	stopID := r.PathValue("stopId")
	if stopID == "" {
		httpError(w, http.StatusBadRequest, "missing stop id")
		return
	}

	q := r.URL.Query()
	at := time.Now()
	if rawAt := q.Get("at"); rawAt != "" {
		parsed, err := time.Parse(time.RFC3339, rawAt)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid at (use RFC3339)")
			return
		}
		at = parsed
	}

	window := clamp(parseIntOr(q.Get("windowMinutes"), defaultWindowMins), 1, maxWindowMins)
	limit := clamp(parseIntOr(q.Get("limit"), defaultDepartures), 1, maxDepartures)

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	departures, err := s.repo.GetStopDepartures(ctx, stopID, at, window, limit)
	if err != nil {
		log.Printf("stop departures: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch departures")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   departures,
		Meta:   &models.Meta{Total: len(departures), Limit: limit, ServiceDate: at.Format("2006-01-02")},
	})
}

// ============================================================================
// Routes
// ============================================================================

func (s *Server) handleSearchRoutes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	q := r.URL.Query()
	params := repository.RouteSearchParams{
		Search:    q.Get("search"),
		RouteType: q.Get("type"),
		Limit:     clamp(parseIntOr(q.Get("limit"), defaultRoutesLimit), 1, maxRoutesLimit),
		Page:      clamp(parseIntOr(q.Get("page"), 1), 1, 1_000_000),
	}

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	cacheKey := "routes:" + params.Search + "|" + params.RouteType + "|" + strconv.Itoa(params.Limit) + "|" + strconv.Itoa(params.Page)
	var (
		routes []models.Route
		total  int
	)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if hit, ok := cached.(struct {
			Routes []models.Route
			Total  int
		}); ok {
			routes = hit.Routes
			total = hit.Total
		}
	}
	if routes == nil {
		routes, total, err = s.repo.SearchRoutes(ctx, params)
		if err != nil {
			log.Printf("search routes: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to search routes")
			return
		}
		s.cache.Set(cacheKey, struct {
			Routes []models.Route
			Total  int
		}{Routes: routes, Total: total})
	}

	hasNext := params.Page*params.Limit < total
	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   routes,
		Meta:   &models.Meta{Total: total, Limit: params.Limit, Page: params.Page, HasNext: hasNext},
	})
}

func (s *Server) handleGetRoute(w http.ResponseWriter, r *http.Request) {
	routeID := r.PathValue("routeId")
	if routeID == "" {
		httpError(w, http.StatusBadRequest, "missing route id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	route, err := s.repo.GetRoute(ctx, routeID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "route not found")
		return
	}
	if err != nil {
		log.Printf("get route: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch route")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{Status: status, Data: route})
}

func (s *Server) handleRouteStops(w http.ResponseWriter, r *http.Request) {
	routeID := r.PathValue("routeId")
	if routeID == "" {
		httpError(w, http.StatusBadRequest, "missing route id")
		return
	}
	directionID := strings.TrimSpace(r.URL.Query().Get("directionId"))

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	stops, err := s.repo.GetRouteStops(ctx, routeID, directionID)
	if err != nil {
		log.Printf("route stops: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch route stops")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   stops,
		Meta:   &models.Meta{Total: len(stops)},
	})
}

func (s *Server) handleRouteDirections(w http.ResponseWriter, r *http.Request) {
	routeID := r.PathValue("routeId")
	if routeID == "" {
		httpError(w, http.StatusBadRequest, "missing route id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	directions, err := s.repo.GetRouteDirections(ctx, routeID)
	if err != nil {
		log.Printf("route directions: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch route directions")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   directions,
		Meta:   &models.Meta{Total: len(directions)},
	})
}

func (s *Server) handleRouteShape(w http.ResponseWriter, r *http.Request) {
	routeID := r.PathValue("routeId")
	if routeID == "" {
		httpError(w, http.StatusBadRequest, "missing route id")
		return
	}
	directionID := strings.TrimSpace(r.URL.Query().Get("directionId"))

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	shape, err := s.repo.GetRouteShape(ctx, routeID, directionID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "route shape not found")
		return
	}
	if err != nil {
		log.Printf("route shape: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch route shape")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   shape,
		Meta:   &models.Meta{Total: len(shape.Points)},
	})
}

// ============================================================================
// Trips
// ============================================================================

func (s *Server) handleListTrips(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serviceDate := q.Get("serviceDate")
	if serviceDate == "" {
		serviceDate = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", serviceDate); err != nil {
		httpError(w, http.StatusBadRequest, "invalid serviceDate (use YYYY-MM-DD)")
		return
	}

	routeID := q.Get("routeId")
	limit := clamp(parseIntOr(q.Get("limit"), defaultTripsLimit), 1, maxTripsLimit)

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	trips, err := s.repo.ListTrips(ctx, routeID, serviceDate, limit)
	if err != nil {
		log.Printf("list trips: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to list trips")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   trips,
		Meta:   &models.Meta{Total: len(trips), Limit: limit, ServiceDate: serviceDate},
	})
}

func (s *Server) handleGetTrip(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("tripId")
	if tripID == "" {
		httpError(w, http.StatusBadRequest, "missing trip id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	trip, err := s.repo.GetTrip(ctx, tripID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "trip not found")
		return
	}
	if err != nil {
		log.Printf("get trip: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch trip")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{Status: status, Data: trip})
}

func (s *Server) handleTripStopTimes(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("tripId")
	if tripID == "" {
		httpError(w, http.StatusBadRequest, "missing trip id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	stopTimes, err := s.repo.GetTripStopTimes(ctx, tripID)
	if err != nil {
		log.Printf("trip stop times: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch trip stop times")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   stopTimes,
		Meta:   &models.Meta{Total: len(stopTimes)},
	})
}

// ============================================================================
// Calendar
// ============================================================================

func (s *Server) handleServiceCalendar(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		httpError(w, http.StatusBadRequest, "invalid date (use YYYY-MM-DD)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	services, err := s.repo.GetServiceCalendarOnDate(ctx, date)
	if err != nil {
		log.Printf("service calendar: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch service calendar")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   services,
		Meta:   &models.Meta{Total: len(services), ServiceDate: date},
	})
}

func (s *Server) handleCalendarExceptions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from := q.Get("from")
	to := q.Get("to")
	if from == "" || to == "" {
		httpError(w, http.StatusBadRequest, "from and to are required (YYYY-MM-DD)")
		return
	}
	if _, err := time.Parse("2006-01-02", from); err != nil {
		httpError(w, http.StatusBadRequest, "invalid from")
		return
	}
	if _, err := time.Parse("2006-01-02", to); err != nil {
		httpError(w, http.StatusBadRequest, "invalid to")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	exceptions, err := s.repo.GetCalendarExceptions(ctx, from, to)
	if err != nil {
		log.Printf("calendar exceptions: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch calendar exceptions")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   exceptions,
		Meta:   &models.Meta{Total: len(exceptions)},
	})
}

// ============================================================================
// Realtime
// ============================================================================

func (s *Server) handleVehicles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	vehicles, err := s.repo.GetVehicles(ctx)
	if err != nil {
		log.Printf("vehicles: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch vehicles")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   vehicles,
		Meta:   &models.Meta{Total: len(vehicles)},
	})
}

// ============================================================================
// Health
// ============================================================================

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// ============================================================================
// Auth
// ============================================================================

type authPayload struct {
	Email    string         `json:"email"`
	Password string         `json:"password"`
	FullName string         `json:"full_name,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type verifyOTPPayload struct {
	Email string `json:"email,omitempty"`
	Token string `json:"token"`
	Type  string `json:"type"` // signup, email, recovery, magiclink, email_change
}

func (s *Server) handleAuthSignup(w http.ResponseWriter, r *http.Request) {
	s.handleAuth(w, r, true)
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	s.handleAuth(w, r, false)
}

func (s *Server) handleAuthVerifyOTP(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.Enabled() {
		httpError(w, http.StatusServiceUnavailable, "supabase auth is not configured")
		return
	}

	var body verifyOTPPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if strings.TrimSpace(body.Token) == "" || strings.TrimSpace(body.Type) == "" {
		httpError(w, http.StatusBadRequest, "token and type are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	resp, statusCode, err := s.auth.VerifyOTP(ctx, supabaseauth.VerifyOTPRequest{
		Email: strings.TrimSpace(body.Email),
		Token: strings.TrimSpace(body.Token),
		Type:  strings.TrimSpace(body.Type),
	})
	if err != nil {
		httpError(w, statusCodeOr(statusCode, http.StatusBadGateway), err.Error())
		return
	}

	writeJSON(w, statusCodeOr(statusCode, http.StatusOK), resp)
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request, signup bool) {
	if s.auth == nil || !s.auth.Enabled() {
		httpError(w, http.StatusServiceUnavailable, "supabase auth is not configured")
		return
	}

	var body authPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if strings.TrimSpace(body.Email) == "" || strings.TrimSpace(body.Password) == "" {
		httpError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	metadata := map[string]any{}
	for k, v := range body.Metadata {
		metadata[k] = v
	}
	if signup {
		if strings.TrimSpace(body.FullName) == "" {
			httpError(w, http.StatusBadRequest, "full_name is required for signup")
			return
		}
		metadata["full_name"] = strings.TrimSpace(body.FullName)
	}

	req := supabaseauth.AuthRequest{
		Email:    strings.TrimSpace(body.Email),
		Password: body.Password,
		Metadata: metadata,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var (
		resp       *supabaseauth.AuthResponse
		statusCode int
		err        error
	)
	if signup {
		resp, statusCode, err = s.auth.SignUp(ctx, req)
	} else {
		resp, statusCode, err = s.auth.Login(ctx, req)
	}
	if err != nil {
		httpError(w, statusCodeOr(statusCode, http.StatusBadGateway), err.Error())
		return
	}

	writeJSON(w, statusCodeOr(statusCode, http.StatusOK), resp)
}

// ============================================================================
// Helpers
// ============================================================================

func parseIntOr(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	if v, err := strconv.Atoi(raw); err == nil {
		return v
	}
	return fallback
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func optionalFloat(values map[string][]string, key string) (*float64, error) {
	vals, ok := values[key]
	if !ok || len(vals) == 0 || vals[0] == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(vals[0], 64)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("encode json failed: %v", err)
	}
}

func statusCodeOr(code, fallback int) int {
	if code <= 0 {
		return fallback
	}
	return code
}
