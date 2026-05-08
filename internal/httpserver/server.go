package httpserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
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
	defaultStopsLimit         = 50
	maxStopsLimit             = 200
	defaultRoutesLimit        = 200
	maxRoutesLimit            = 500
	defaultTripsLimit         = 200
	maxTripsLimit             = 1000
	defaultDepartures         = 20
	maxDepartures             = 50
	defaultWindowMins         = 60
	maxWindowMins             = 360
	defaultShapePoints        = 2000 // cap returned vertices; full table may be 400k+ rows across all shapes
	minShapePoints            = 50
	maxShapePoints            = 25000
	defaultMapStops           = 150
	maxMapStops               = 500
	defaultMapRoutes          = 300
	maxMapRoutes              = 500
	defaultOverlayShapePoints = 1800
	maxOverlayRoutes          = 25
	defaultCalendarLimit      = 100
	maxCalendarLimit          = 500
	requestTimeout            = 5 * time.Second
	mapOverlayTimeout         = 25 * time.Second
	routeShapeCacheTTL        = 2 * time.Minute
	mapOverlayCacheTTL        = 45 * time.Second
	feedActiveCacheTTL        = 30 * time.Second
	vehiclesCacheTTL          = 5 * time.Second
	realtimeTripCacheTTL      = 8 * time.Second
	realtimeAlertsCacheTTL    = 20 * time.Second
	defaultRealtimeLimit      = 500
	maxRealtimeLimit          = 2000
)

type Server struct {
	cfg      config.Config
	repo     *repository.Repository
	cache    *cache.TTLCache
	snapshot *snapshot.Snapshot
	auth     *supabaseauth.Client
	live     *vehicleLiveHub
}

func New(cfg config.Config, repo *repository.Repository, snap *snapshot.Snapshot, auth *supabaseauth.Client) *Server {
	return &Server{
		cfg:      cfg,
		repo:     repo,
		cache:    cache.New(2 * time.Minute),
		snapshot: snap,
		auth:     auth,
		live:     newVehicleLiveHub(),
	}
}

func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleSwaggerRoot)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /swagger", s.handleSwaggerUI)
	mux.HandleFunc("GET /swagger/", s.handleSwaggerUI)
	mux.HandleFunc("GET /openapi.yaml", s.handleOpenAPI)
	mux.HandleFunc("POST /v1/auth/signup", s.handleAuthSignup)
	mux.HandleFunc("POST /v1/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /v1/auth/verify-otp", s.handleAuthVerifyOTP)
	mux.HandleFunc("GET /v1/users/me/favorite-routes", s.handleListFavoriteRoutes)
	mux.HandleFunc("POST /v1/users/me/favorite-routes", s.handleAddFavoriteRoute)
	mux.HandleFunc("DELETE /v1/users/me/favorite-routes/{routeId}", s.handleDeleteFavoriteRoute)

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
	mux.Handle("GET /v1/gtfs/routes/{routeId}/shape-encoded", staticCache(http.HandlerFunc(s.handleRouteShapeEncoded)))

	mux.Handle("GET /v1/gtfs/trips", staticCache(http.HandlerFunc(s.handleListTrips)))
	mux.Handle("GET /v1/gtfs/trips/{tripId}", staticCache(http.HandlerFunc(s.handleGetTrip)))
	mux.Handle("GET /v1/gtfs/trips/{tripId}/stop-times", staticCache(http.HandlerFunc(s.handleTripStopTimes)))

	mux.Handle("GET /v1/gtfs/calendar/service", staticCache(http.HandlerFunc(s.handleServiceCalendar)))
	mux.Handle("GET /v1/gtfs/calendar/exceptions", staticCache(http.HandlerFunc(s.handleCalendarExceptions)))
	mux.Handle("GET /v1/gtfs/calendar/day", staticCache(http.HandlerFunc(s.handleCalendarDay)))
	mux.Handle("GET /v1/gtfs/calendar/timetable-lite", staticCache(http.HandlerFunc(s.handleCalendarTimetableLite)))
	mux.Handle("GET /v1/gtfs/calendar/{serviceId}", staticCache(http.HandlerFunc(s.handleGetCalendarService)))
	mux.Handle("GET /v1/gtfs/calendar", staticCache(http.HandlerFunc(s.handleListCalendar)))

	mux.HandleFunc("GET /v1/feed/active", s.handleFeedActive)

	// Map: one lightweight call for pins + route colors (snapshot; no double fetch)
	mux.Handle("GET /v1/map/static", staticCache(http.HandlerFunc(s.handleMapStatic)))
	// Map: colored polylines + stops per route/direction (for “map explorer” overlays)
	mux.Handle("GET /v1/map/routes-overlay", staticCache(http.HandlerFunc(s.handleMapRoutesOverlay)))
	// Map: normalized dictionaries for high-volume RN rendering
	mux.Handle("GET /v1/map/routes-normalized", staticCache(http.HandlerFunc(s.handleMapRoutesNormalized)))
	// Map: normalized stops dictionary with ordered ids + cursor pagination
	mux.Handle("GET /v1/map/stops-normalized", staticCache(http.HandlerFunc(s.handleMapStopsNormalized)))
	// Map: normalized stop_id -> arrivals[] for flat list rendering
	mux.Handle("GET /v1/map/arrivals-normalized", dynamicCache(http.HandlerFunc(s.handleArrivalsNormalized)))
	// Map: one next arrival row per stop_id (lightweight badges/chips)
	mux.Handle("GET /v1/map/arrivals-next", dynamicCache(http.HandlerFunc(s.handleArrivalsNext)))

	// Realtime
	mux.HandleFunc("GET /v1/map/vehicles", s.handleVehicles)
	mux.HandleFunc("GET /v1/map/trip-live", s.handleTripLive)
	mux.HandleFunc("GET /v1/map/eta-between-stops", s.handleETABetweenStops)
	mux.HandleFunc("GET /v1/map/vehicles/live", s.handleVehiclesLive)
	mux.HandleFunc("POST /v1/vehicle-position", s.handleVehiclePositionIngest)
	mux.HandleFunc("GET /v1/map/routes-with-live-vehicles", s.handleRoutesWithLiveVehicles)
	mux.Handle("GET /v1/realtime/trip-updates", dynamicCache(http.HandlerFunc(s.handleRealtimeTripUpdates)))
	mux.Handle("GET /v1/realtime/alerts", dynamicCache(http.HandlerFunc(s.handleRealtimeAlerts)))

	return recoverMiddleware(gzipMiddleware(s.authMiddleware(mux)))
}

func routeMapLite(r models.Route) models.RouteMapLite {
	return models.RouteMapLite{
		RouteID:        r.RouteID,
		ShortName:      r.ShortName,
		LongName:       r.LongName,
		RouteColor:     r.RouteColor,
		RouteTextColor: r.RouteTextColor,
	}
}

// parseRouteIDsFromQuery reads routeIds (comma-separated or repeated) and optional routeIdsKind.
// Same resolution rules as /v1/map/routes-overlay; capped at maxOverlayRoutes.
func (s *Server) parseRouteIDsFromQuery(ctx context.Context, q url.Values) []string {
	raw := strings.TrimSpace(q.Get("routeIds"))
	multi := q["routeIds"]
	var parts []string
	if len(multi) > 1 {
		parts = multi
	} else if raw != "" {
		parts = strings.Split(raw, ",")
	}
	if len(parts) == 0 {
		return nil
	}
	kind := strings.ToLower(strings.TrimSpace(q.Get("routeIdsKind")))
	byShort := kind == "short_name" || kind == "short"
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		token := strings.TrimSpace(p)
		if token == "" {
			continue
		}
		id := token
		if byShort {
			rid, err := s.repo.LookupRouteIDByShortName(ctx, token)
			if err != nil {
				continue
			}
			id = rid
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) >= maxOverlayRoutes {
			break
		}
	}
	return out
}

// mapStaticRoutesLiteOrdered returns colors/names only for the given route ids (map filter mode).
func (s *Server) mapStaticRoutesLiteOrdered(ctx context.Context, filterIDs []string, routeSearch string) []models.RouteMapLite {
	q := strings.ToLower(strings.TrimSpace(routeSearch))
	out := make([]models.RouteMapLite, 0, len(filterIDs))
	if s.snapshot != nil && s.snapshot.Loaded() {
		for _, id := range filterIDs {
			rt, ok := s.snapshot.RouteByID(id)
			if !ok {
				continue
			}
			if q != "" {
				if !strings.Contains(strings.ToLower(rt.ShortName), q) &&
					!strings.Contains(strings.ToLower(rt.LongName), q) {
					continue
				}
			}
			out = append(out, routeMapLite(rt))
		}
		return out
	}
	for _, id := range filterIDs {
		rt, err := s.repo.GetRoute(ctx, id)
		if err != nil {
			continue
		}
		if q != "" {
			if !strings.Contains(strings.ToLower(rt.ShortName), q) &&
				!strings.Contains(strings.ToLower(rt.LongName), q) {
				continue
			}
		}
		out = append(out, routeMapLite(*rt))
	}
	return out
}

// handleMapStatic returns stops near a point plus a slim route list in one JSON payload.
// The app should call this on region change (debounced) instead of separate /stops + /routes.
func (s *Server) handleMapStatic(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	q := r.URL.Query()
	hasLat, latErr := optionalFloat(q, "lat")
	hasLon, lonErr := optionalFloat(q, "lon")
	if latErr != nil || lonErr != nil {
		httpError(w, http.StatusBadRequest, "invalid lat or lon")
		return
	}
	if hasLat == nil || hasLon == nil {
		httpError(w, http.StatusBadRequest, "lat and lon are required")
		return
	}
	lat, lon := *hasLat, *hasLon

	radius := s.cfg.NearbyDefaultRadiusMeters
	if raw := q.Get("radius"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > 50_000 {
			httpError(w, http.StatusBadRequest, "invalid radius (1–50000 meters)")
			return
		}
		radius = v
	}

	stopLimit := clamp(parseIntOr(q.Get("stopLimit"), defaultMapStops), 1, maxMapStops)
	routeLimit := clamp(parseIntOr(q.Get("routeLimit"), defaultMapRoutes), 1, maxMapRoutes)
	routeSearch := strings.TrimSpace(q.Get("routeSearch"))
	filterRouteIDs := s.parseRouteIDsFromQuery(ctx, q)

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	var (
		stops      []models.StopSummary
		stopsTotal int
		routesLite []models.RouteMapLite
	)
	if len(filterRouteIDs) > 0 {
		// Only stops served by selected routes within radius (requires DB; matches map filter UX).
		params := repository.StopSearchParams{
			Search:       "",
			Lat:          &lat,
			Lon:          &lon,
			RadiusMeters: radius,
			Limit:        stopLimit,
			Page:         1,
			RouteIDs:     filterRouteIDs,
		}
		stops, stopsTotal, err = s.repo.SearchStops(ctx, params)
		if err != nil {
			log.Printf("map static stops (route filter): %v", err)
			httpError(w, http.StatusInternalServerError, "failed to load map stops")
			return
		}
		models.EnrichStopSummariesForMap(stops)
		routesLite = s.mapStaticRoutesLiteOrdered(ctx, filterRouteIDs, routeSearch)
	} else if s.snapshot != nil && s.snapshot.Loaded() {
		stops, stopsTotal = s.snapshot.FilterStops("", &lat, &lon, radius, 1, stopLimit)
		models.EnrichStopSummariesForMap(stops)
		for _, rt := range s.snapshot.FilterRoutes(routeSearch, "", routeLimit) {
			routesLite = append(routesLite, routeMapLite(rt))
		}
	} else {
		params := repository.StopSearchParams{
			Search:       "",
			Lat:          &lat,
			Lon:          &lon,
			RadiusMeters: radius,
			Limit:        stopLimit,
			Page:         1,
		}
		stops, stopsTotal, err = s.repo.SearchStops(ctx, params)
		if err != nil {
			log.Printf("map static stops: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to load map stops")
			return
		}
		models.EnrichStopSummariesForMap(stops)
		routes, _, err := s.repo.SearchRoutes(ctx, repository.RouteSearchParams{
			Search:    routeSearch,
			RouteType: "",
			Limit:     routeLimit,
			Page:      1,
		})
		if err != nil {
			log.Printf("map static routes: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to load map routes")
			return
		}
		for i := range routes {
			routesLite = append(routesLite, routeMapLite(routes[i]))
		}
	}

	bundle := models.MapStaticBundle{Stops: stops, Routes: routesLite}
	hasNext := stopsTotal > len(stops)

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   bundle,
		Meta: &models.Meta{
			Total:   stopsTotal,
			Limit:   stopLimit,
			Page:    1,
			HasNext: hasNext,
		},
	})
}

// handleMapRoutesOverlay returns routes with legs: each leg has shape points + stops for one GTFS direction.
// Query: routeIds=R1,R2&directionId=0 (optional)&maxPoints=1800&routeIdsKind=short_name
func (s *Server) handleMapRoutesOverlay(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), mapOverlayTimeout)
	defer cancel()

	q := r.URL.Query()
	raw := strings.TrimSpace(q.Get("routeIds"))
	multi := q["routeIds"]
	var parts []string
	if len(multi) > 1 {
		parts = multi
	} else if raw != "" {
		parts = strings.Split(raw, ",")
	}
	if len(parts) == 0 {
		httpError(w, http.StatusBadRequest, "routeIds is required (comma-separated or repeated query values)")
		return
	}

	routeIdsKind := strings.ToLower(strings.TrimSpace(q.Get("routeIdsKind")))
	byShortName := routeIdsKind == "short_name" || routeIdsKind == "short"

	seen := make(map[string]struct{}, len(parts))
	ids := make([]string, 0, len(parts))
	var missing []string
	for _, p := range parts {
		token := strings.TrimSpace(p)
		if token == "" {
			continue
		}
		id := token
		if byShortName {
			rid, err := s.repo.LookupRouteIDByShortName(ctx, token)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					missing = append(missing, token)
				} else {
					log.Printf("map overlay resolve short_name %q: %v", token, err)
					missing = append(missing, token)
				}
				continue
			}
			id = rid
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		if len(ids) >= maxOverlayRoutes {
			break
		}
	}
	if len(ids) == 0 {
		httpError(w, http.StatusBadRequest, "no valid route ids after resolving (check route_id or use routeIdsKind=short_name)")
		return
	}

	onlyDir := strings.TrimSpace(q.Get("directionId"))
	maxPts := defaultOverlayShapePoints
	if rawMP := q.Get("maxPoints"); rawMP != "" {
		v, err := strconv.Atoi(rawMP)
		if err != nil || v < 0 || v > maxShapePoints {
			httpError(w, http.StatusBadRequest, "invalid maxPoints (0 for full resolution, or 50–25000)")
			return
		}
		if v > 0 {
			maxPts = clamp(v, minShapePoints, maxShapePoints)
		} else {
			maxPts = 0
		}
	}

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	overlayCacheKey := "map_overlay:" + strings.Join(ids, ",") + "|" + onlyDir + "|" + strconv.Itoa(maxPts) + "|" + string(status.Mode)

	routes := make([]models.MapRouteWithGeometry, 0, len(ids))
	if cached, ok := s.cache.Get(overlayCacheKey); ok {
		if hit, ok := cached.(struct {
			Routes  []models.MapRouteWithGeometry
			Missing []string
		}); ok {
			routes = hit.Routes
			missing = append(missing, hit.Missing...)
		}
	}
	if routes == nil {
		for _, routeID := range ids {
			geo, err := s.repo.GetMapRouteWithGeometry(ctx, routeID, maxPts, onlyDir)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					missing = append(missing, routeID)
					continue
				}
				log.Printf("map routes overlay %s: %v", routeID, err)
				missing = append(missing, routeID)
				continue
			}
			routes = append(routes, *geo)
		}
		s.cache.SetWithTTL(overlayCacheKey, struct {
			Routes  []models.MapRouteWithGeometry
			Missing []string
		}{Routes: routes, Missing: missing}, mapOverlayCacheTTL)
	}

	payload := models.MapRoutesOverlayPayload{Routes: routes}
	meta := &models.Meta{
		Total:           len(routes),
		RequestedRoutes: len(ids),
	}
	if len(missing) > 0 {
		meta.MissingRouteIds = missing
	}
	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   payload,
		Meta:   meta,
	})
}

// handleMapRoutesNormalized returns route/stops dictionaries plus junctions and encoded polylines.
// This avoids deep nested arrays on clients and enables O(1) lookups in React Native.
func (s *Server) handleMapRoutesNormalized(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), mapOverlayTimeout)
	defer cancel()

	q := r.URL.Query()
	raw := strings.TrimSpace(q.Get("routeIds"))
	multi := q["routeIds"]
	var parts []string
	if len(multi) > 1 {
		parts = multi
	} else if raw != "" {
		parts = strings.Split(raw, ",")
	}
	if len(parts) == 0 {
		httpError(w, http.StatusBadRequest, "routeIds is required (comma-separated or repeated query values)")
		return
	}

	routeIdsKind := strings.ToLower(strings.TrimSpace(q.Get("routeIdsKind")))
	byShortName := routeIdsKind == "short_name" || routeIdsKind == "short"

	seen := make(map[string]struct{}, len(parts))
	ids := make([]string, 0, len(parts))
	var missing []string
	for _, p := range parts {
		token := strings.TrimSpace(p)
		if token == "" {
			continue
		}
		id := token
		if byShortName {
			rid, err := s.repo.LookupRouteIDByShortName(ctx, token)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					missing = append(missing, token)
				} else {
					log.Printf("map normalized resolve short_name %q: %v", token, err)
					missing = append(missing, token)
				}
				continue
			}
			id = rid
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		if len(ids) >= maxOverlayRoutes {
			break
		}
	}
	if len(ids) == 0 {
		httpError(w, http.StatusBadRequest, "no valid route ids after resolving (check route_id or use routeIdsKind=short_name)")
		return
	}

	onlyDir := strings.TrimSpace(q.Get("directionId"))
	maxPts := defaultOverlayShapePoints
	if rawMP := q.Get("maxPoints"); rawMP != "" {
		v, err := strconv.Atoi(rawMP)
		if err != nil || v < 0 || v > maxShapePoints {
			httpError(w, http.StatusBadRequest, "invalid maxPoints (0 for full resolution, or 50–25000)")
			return
		}
		if v > 0 {
			maxPts = clamp(v, minShapePoints, maxShapePoints)
		} else {
			maxPts = 0
		}
	}

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	cacheKey := "map_normalized:" + strings.Join(ids, ",") + "|" + onlyDir + "|" + strconv.Itoa(maxPts) + "|" + string(status.Mode)
	payload := models.MapNormalizedPayload{
		Routes:          map[string]models.RouteMapLite{},
		Stops:           map[string]models.StopSummary{},
		Junctions:       map[string][]string{},
		RouteGeometries: map[string]string{},
	}
	if cached, ok := s.cache.Get(cacheKey); ok {
		if hit, ok := cached.(struct {
			Payload models.MapNormalizedPayload
			Missing []string
		}); ok {
			payload = hit.Payload
			missing = append(missing, hit.Missing...)
		}
	}
	if len(payload.Routes) == 0 && len(payload.Stops) == 0 && len(payload.RouteGeometries) == 0 {
		for _, routeID := range ids {
			geo, err := s.repo.GetMapRouteWithGeometry(ctx, routeID, maxPts, onlyDir)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					missing = append(missing, routeID)
					continue
				}
				log.Printf("map routes normalized %s: %v", routeID, err)
				missing = append(missing, routeID)
				continue
			}
			payload.Routes[geo.RouteID] = models.RouteMapLite{
				RouteID:        geo.RouteID,
				ShortName:      geo.ShortName,
				LongName:       geo.LongName,
				RouteColor:     geo.RouteColor,
				RouteTextColor: geo.RouteTextColor,
			}
			stopIDs := make([]string, 0, 64)
			seenStop := map[string]struct{}{}
			for _, leg := range geo.Legs {
				for _, st := range leg.Stops {
					id := strings.TrimSpace(st.StopID)
					if id == "" {
						continue
					}
					if _, ok := seenStop[id]; ok {
						continue
					}
					seenStop[id] = struct{}{}
					stopIDs = append(stopIDs, id)
					payload.Stops[id] = st
				}
			}
			payload.Junctions[geo.RouteID] = stopIDs
			if len(geo.Legs) > 0 {
				// Prefer first leg geometry for route-level fast render; request directionId for strict direction.
				payload.RouteGeometries[geo.RouteID] = encodePolyline(geo.Legs[0].Points)
			}
		}
		s.cache.SetWithTTL(cacheKey, struct {
			Payload models.MapNormalizedPayload
			Missing []string
		}{Payload: payload, Missing: missing}, mapOverlayCacheTTL)
	}

	meta := &models.Meta{
		Total:           len(payload.Routes),
		RequestedRoutes: len(ids),
	}
	if len(missing) > 0 {
		meta.MissingRouteIds = missing
	}
	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   payload,
		Meta:   meta,
	})
}

func (s *Server) handleMapStopsNormalized(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	q := r.URL.Query()
	hasLat, latErr := optionalFloat(q, "lat")
	hasLon, lonErr := optionalFloat(q, "lon")
	if latErr != nil || lonErr != nil || hasLat == nil || hasLon == nil {
		httpError(w, http.StatusBadRequest, "lat and lon are required")
		return
	}
	lat, lon := *hasLat, *hasLon

	radius := s.cfg.NearbyDefaultRadiusMeters
	if raw := q.Get("radius"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > 50_000 {
			httpError(w, http.StatusBadRequest, "invalid radius (1–50000 meters)")
			return
		}
		radius = v
	}

	offset, bad := parseCursorOffset(q.Get("cursor"))
	if bad {
		httpError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	limit := clamp(parseIntOr(q.Get("limit"), defaultMapStops), 1, maxMapStops)
	page := (offset / limit) + 1

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	filterRouteIDs := s.parseRouteIDsFromQuery(ctx, q)
	params := repository.StopSearchParams{
		Search:       strings.TrimSpace(q.Get("search")),
		Lat:          &lat,
		Lon:          &lon,
		RadiusMeters: radius,
		Limit:        limit,
		Page:         page,
		RouteIDs:     filterRouteIDs,
	}

	var (
		stops []models.StopSummary
		total int
	)
	if len(filterRouteIDs) > 0 {
		// Route-filtered stops must hit DB (same behavior as /v1/map/static).
		stops, total, err = s.repo.SearchStops(ctx, params)
	} else if s.snapshot != nil && s.snapshot.Loaded() {
		stops, total = s.snapshot.FilterStops(params.Search, params.Lat, params.Lon, params.RadiusMeters, params.Page, params.Limit)
	} else {
		stops, total, err = s.repo.SearchStops(ctx, params)
	}
	if err != nil {
		log.Printf("map stops normalized: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to load map stops")
		return
	}
	models.EnrichStopSummariesForMap(stops)

	payload := models.StopsNormalizedPayload{
		Stops:   make(map[string]models.StopSummary, len(stops)),
		StopIDs: make([]string, 0, len(stops)),
	}
	for i := range stops {
		id := strings.TrimSpace(stops[i].StopID)
		if id == "" {
			continue
		}
		payload.Stops[id] = stops[i]
		payload.StopIDs = append(payload.StopIDs, id)
	}

	hasNext := page*limit < total
	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   payload,
		Meta: &models.Meta{
			Total:      total,
			Limit:      limit,
			Page:       page,
			HasNext:    hasNext,
			NextCursor: nextCursorForCount(len(payload.StopIDs), offset, limit),
		},
	})
}

func (s *Server) handleArrivalsNormalized(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	q := r.URL.Query()
	raw := strings.TrimSpace(q.Get("stopIds"))
	multi := q["stopIds"]
	var parts []string
	if len(multi) > 1 {
		parts = multi
	} else if raw != "" {
		parts = strings.Split(raw, ",")
	}
	if len(parts) == 0 {
		httpError(w, http.StatusBadRequest, "stopIds is required (comma-separated or repeated query values)")
		return
	}
	seen := make(map[string]struct{}, len(parts))
	stopIDs := make([]string, 0, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		stopIDs = append(stopIDs, id)
		if len(stopIDs) >= 250 {
			break
		}
	}
	if len(stopIDs) == 0 {
		httpError(w, http.StatusBadRequest, "no valid stop ids")
		return
	}

	at := time.Now()
	if rawAt := strings.TrimSpace(q.Get("at")); rawAt != "" {
		v, err := time.Parse(time.RFC3339, rawAt)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid at (use RFC3339)")
			return
		}
		at = v
	}
	window := clamp(parseIntOr(q.Get("windowMinutes"), 180), 1, 360)
	limit := clamp(parseIntOr(q.Get("limit"), 500), 1, 2000)
	offset, bad := parseCursorOffset(q.Get("cursor"))
	if bad {
		httpError(w, http.StatusBadRequest, "invalid cursor")
		return
	}

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	cacheKey := "arrivals_normalized:" + strings.Join(stopIDs, ",") + "|" + at.UTC().Format(time.RFC3339) + "|" + strconv.Itoa(window) + "|" + strconv.Itoa(limit) + "|" + strconv.Itoa(offset)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if env, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, env)
			return
		}
	}

	rowStopIDs, rows, err := s.repo.GetNormalizedArrivals(ctx, stopIDs, at, window, limit, offset)
	if err != nil {
		log.Printf("arrivals normalized: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to load arrivals")
		return
	}
	payload := models.ArrivalsNormalizedPayload{Arrivals: make(map[string][]models.StopArrivalLite, len(stopIDs))}
	for i := range rows {
		sid := rowStopIDs[i]
		payload.Arrivals[sid] = append(payload.Arrivals[sid], rows[i])
	}

	resp := models.ResponseEnvelope{
		Status: status,
		Data:   payload,
		Meta: &models.Meta{
			Total:      len(rows),
			Limit:      limit,
			NextCursor: nextCursorForCount(len(rows), offset, limit),
		},
	}
	s.cache.SetWithTTL(cacheKey, resp, realtimeTripCacheTTL)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleArrivalsNext(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	q := r.URL.Query()
	raw := strings.TrimSpace(q.Get("stopIds"))
	multi := q["stopIds"]
	var parts []string
	if len(multi) > 1 {
		parts = multi
	} else if raw != "" {
		parts = strings.Split(raw, ",")
	}
	if len(parts) == 0 {
		httpError(w, http.StatusBadRequest, "stopIds is required (comma-separated or repeated query values)")
		return
	}
	seen := make(map[string]struct{}, len(parts))
	stopIDs := make([]string, 0, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		stopIDs = append(stopIDs, id)
		if len(stopIDs) >= 250 {
			break
		}
	}
	if len(stopIDs) == 0 {
		httpError(w, http.StatusBadRequest, "no valid stop ids")
		return
	}

	at := time.Now()
	if rawAt := strings.TrimSpace(q.Get("at")); rawAt != "" {
		v, err := time.Parse(time.RFC3339, rawAt)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid at (use RFC3339)")
			return
		}
		at = v
	}
	window := clamp(parseIntOr(q.Get("windowMinutes"), 180), 1, 360)

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}
	cacheKey := "arrivals_next:" + strings.Join(stopIDs, ",") + "|" + at.UTC().Format(time.RFC3339) + "|" + strconv.Itoa(window)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if env, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, env)
			return
		}
	}

	arrivals, err := s.repo.GetNextArrivalsPerStop(ctx, stopIDs, at, window)
	if err != nil {
		log.Printf("arrivals next: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to load next arrivals")
		return
	}
	resp := models.ResponseEnvelope{
		Status: status,
		Data:   models.ArrivalsNextPayload{Arrivals: arrivals},
		Meta:   &models.Meta{Total: len(arrivals)},
	}
	s.cache.SetWithTTL(cacheKey, resp, realtimeTripCacheTTL)
	writeJSON(w, http.StatusOK, resp)
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
	// Fallback: if nothing upcoming in the requested window, fetch from
	// next morning so the UI can still show the next available service.
	if len(departures) == 0 {
		nextAt := time.Date(
			at.Year(), at.Month(), at.Day()+1,
			4, 0, 0, 0,
			at.Location(),
		)
		departures, err = s.repo.GetStopDepartures(ctx, stopID, nextAt, 24*60, limit)
		if err != nil {
			log.Printf("stop departures fallback: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to fetch departures")
			return
		}
		if len(departures) > 0 {
			at = nextAt
		}
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
	q := r.URL.Query()
	directionID := strings.TrimSpace(q.Get("directionId"))
	lite := strings.EqualFold(strings.TrimSpace(q.Get("lite")), "true") || strings.TrimSpace(q.Get("lite")) == "1"

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	cacheKey := "route_stops:" + routeID + "|" + directionID + "|lite=" + strconv.FormatBool(lite)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if hit, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, hit)
			return
		}
	}

	stops, err := s.repo.GetRouteStops(ctx, routeID, directionID)
	if err != nil {
		log.Printf("route stops: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch route stops")
		return
	}
	if lite {
		payload := models.RouteStopsLitePayload{
			Stops:   make(map[string]models.StopPointLite, len(stops)),
			StopIDs: make([]string, 0, len(stops)),
		}
		for i := range stops {
			id := strings.TrimSpace(stops[i].StopID)
			if id == "" {
				continue
			}
			payload.StopIDs = append(payload.StopIDs, id)
			payload.Stops[id] = models.StopPointLite{
				StopID:   id,
				StopName: strings.TrimSpace(stops[i].StopName),
				StopCode: strings.TrimSpace(stops[i].StopCode),
				Lat:      stops[i].Lat,
				Lon:      stops[i].Lon,
			}
		}
		resp := models.ResponseEnvelope{
			Status: status,
			Data:   payload,
			Meta:   &models.Meta{Total: len(payload.StopIDs)},
		}
		s.cache.SetWithTTL(cacheKey, resp, 60*time.Second)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp := models.ResponseEnvelope{
		Status: status,
		Data:   stops,
		Meta:   &models.Meta{Total: len(stops)},
	}
	s.cache.SetWithTTL(cacheKey, resp, 60*time.Second)
	writeJSON(w, http.StatusOK, resp)
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
	q := r.URL.Query()
	maxPoints := defaultShapePoints
	if raw := strings.TrimSpace(q.Get("maxPoints")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			httpError(w, http.StatusBadRequest, "invalid maxPoints (use 0 for full resolution, or a positive integer)")
			return
		}
		if v == 0 {
			maxPoints = 0
		} else {
			maxPoints = clamp(v, minShapePoints, maxShapePoints)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	shapeCacheKey := "route_shape:" + routeID + "|" + directionID + "|" + strconv.Itoa(maxPoints) + "|" + string(status.Mode)
	var shape *models.RouteShape
	if cached, ok := s.cache.Get(shapeCacheKey); ok {
		if hit, ok := cached.(*models.RouteShape); ok {
			shape = hit
		}
	}
	if shape == nil {
		shape, err = s.repo.GetRouteShape(ctx, routeID, directionID, maxPoints)
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "route shape not found")
			return
		}
		if err != nil {
			log.Printf("route shape: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to fetch route shape")
			return
		}
		s.cache.SetWithTTL(shapeCacheKey, shape, routeShapeCacheTTL)
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   shape,
		Meta:   &models.Meta{Total: len(shape.Points)},
	})
}

func (s *Server) handleRouteShapeEncoded(w http.ResponseWriter, r *http.Request) {
	routeID := r.PathValue("routeId")
	if routeID == "" {
		httpError(w, http.StatusBadRequest, "missing route id")
		return
	}
	directionID := strings.TrimSpace(r.URL.Query().Get("directionId"))
	q := r.URL.Query()
	maxPoints := defaultShapePoints
	if raw := strings.TrimSpace(q.Get("maxPoints")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			httpError(w, http.StatusBadRequest, "invalid maxPoints (use 0 for full resolution, or a positive integer)")
			return
		}
		if v == 0 {
			maxPoints = 0
		} else {
			maxPoints = clamp(v, minShapePoints, maxShapePoints)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	shapeCacheKey := "route_shape_encoded:" + routeID + "|" + directionID + "|" + strconv.Itoa(maxPoints) + "|" + string(status.Mode)
	var payload *models.RouteShapeEncoded
	if cached, ok := s.cache.Get(shapeCacheKey); ok {
		if hit, ok := cached.(*models.RouteShapeEncoded); ok {
			payload = hit
		}
	}
	if payload == nil {
		shape, err := s.repo.GetRouteShape(ctx, routeID, directionID, maxPoints)
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "route shape not found")
			return
		}
		if err != nil {
			log.Printf("route shape encoded: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to fetch route shape")
			return
		}
		payload = &models.RouteShapeEncoded{
			RouteID:         shape.RouteID,
			ShapeID:         shape.ShapeID,
			EncodedPolyline: encodePolyline(shape.Points),
			TotalPoints:     shape.TotalPoints,
		}
		s.cache.SetWithTTL(shapeCacheKey, payload, routeShapeCacheTTL)
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   payload,
		Meta:   &models.Meta{Total: payload.TotalPoints},
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
	fromT, err := time.Parse("2006-01-02", from)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid from")
		return
	}
	toT, err := time.Parse("2006-01-02", to)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid to")
		return
	}
	if toT.Before(fromT) {
		httpError(w, http.StatusBadRequest, "from must be on or before to")
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

// handleCalendarDay resolves active service_ids for date D (GTFS calendar + calendar_dates add/remove).
// Query: date=YYYY-MM-DD, detail=true (per-service calendar row, display, trips), include=window (feed min/max dates).
func (s *Server) handleCalendarDay(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_date", "invalid date (use YYYY-MM-DD)")
		return
	}
	q := r.URL.Query()
	detail := strings.EqualFold(q.Get("detail"), "true") || q.Get("detail") == "1"
	include := q.Get("include")
	includeWindow := strings.Contains(include, "window")

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		writeAPIError(w, r, http.StatusInternalServerError, "feed_state_error", "failed to read feed state")
		return
	}

	active, err := s.repo.GetServiceCalendarOnDate(ctx, date)
	if err != nil {
		log.Printf("calendar day: %v", err)
		writeAPIError(w, r, http.StatusInternalServerError, "calendar_query_error", "failed to resolve services for date")
		return
	}

	events, err := s.repo.GetCalendarExceptions(ctx, date, date)
	if err != nil {
		log.Printf("calendar day events: %v", err)
		events = nil
	}

	services := make([]models.ActiveServiceOnDay, 0, len(active))
	for _, a := range active {
		row := models.ActiveServiceOnDay{ServiceID: a.ServiceID, Source: a.Source}
		if detail {
			for _, e := range events {
				if e.ServiceID == a.ServiceID {
					row.ExceptionsToday = append(row.ExceptionsToday, e)
				}
			}
			if cal, err := s.repo.GetCalendarService(ctx, a.ServiceID); err == nil {
				row.Calendar = cal
				lbl, desc := models.ServiceWeekdayLabel(cal)
				tripN, _ := s.repo.CountTripsForService(ctx, a.ServiceID)
				routes, _ := s.repo.RouteShortNamesForService(ctx, a.ServiceID)
				row.Display = &models.ServiceDisplay{
					Label: lbl, Description: desc, RouteShortNames: routes,
					TripCount: tripN, HasTrips: tripN > 0,
				}
			} else {
				tripN, _ := s.repo.CountTripsForService(ctx, a.ServiceID)
				routes, _ := s.repo.RouteShortNamesForService(ctx, a.ServiceID)
				row.Display = &models.ServiceDisplay{
					Label: a.ServiceID, RouteShortNames: routes,
					TripCount: tripN, HasTrips: tripN > 0,
					Description: "Added by calendar_dates for this date (no base calendar row)",
				}
			}
		}
		services = append(services, row)
	}

	payload := models.CalendarDayPayload{
		Date:               date,
		Services:           services,
		CalendarDateEvents: events,
	}
	if includeWindow {
		win, err := s.repo.GetFeedCalendarWindow(ctx)
		if err != nil {
			log.Printf("calendar day window: %v", err)
		} else if win != nil && (win.MinDate != "" || win.MaxDate != "") {
			payload.FeedCalendarWindow = win
		}
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   payload,
		Meta:   &models.Meta{Total: len(services), ServiceDate: date},
	})
}

// handleCalendarTimetableLite provides a compact one-call route timetable list for a date.
// Query: date=YYYY-MM-DD&routeId=R1[&directionId=0][&limit=80][&cursor=...]
func (s *Server) handleCalendarTimetableLite(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	date := strings.TrimSpace(q.Get("date"))
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		httpError(w, http.StatusBadRequest, "invalid date (use YYYY-MM-DD)")
		return
	}
	routeID := strings.TrimSpace(q.Get("routeId"))
	if routeID == "" {
		httpError(w, http.StatusBadRequest, "routeId is required")
		return
	}
	directionID := strings.TrimSpace(q.Get("directionId"))
	limit := clamp(parseIntOr(q.Get("limit"), 80), 1, 500)
	offset, bad := parseCursorOffset(q.Get("cursor"))
	if bad {
		httpError(w, http.StatusBadRequest, "invalid cursor")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	cacheKey := "calendar_timetable_lite:" + date + "|" + routeID + "|" + directionID + "|" + strconv.Itoa(limit) + "|" + strconv.Itoa(offset)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if env, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, env)
			return
		}
	}

	trips, err := s.repo.ListTimetableTripsLite(ctx, routeID, date, directionID, limit, offset)
	if err != nil {
		log.Printf("calendar timetable lite: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch timetable")
		return
	}
	// Static-data fallback: if calendar/day filter yields no rows, return route timetable from trips+stop_times.
	if len(trips) == 0 {
		trips, err = s.repo.ListTimetableTripsLiteStatic(ctx, routeID, directionID, limit, offset)
		if err != nil {
			log.Printf("calendar timetable lite static fallback: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to fetch timetable")
			return
		}
	}
	payload := models.CalendarTimetableLitePayload{
		Date:      date,
		RouteID:   routeID,
		Direction: directionID,
		Trips:     trips,
	}
	resp := models.ResponseEnvelope{
		Status: status,
		Data:   payload,
		Meta: &models.Meta{
			Total:       len(trips),
			Limit:       limit,
			ServiceDate: date,
			NextCursor:  nextCursorForCount(len(trips), offset, limit),
		},
	}
	s.cache.SetWithTTL(cacheKey, resp, 45*time.Second)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleFeedActive(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	const feedCacheKey = "feed_active_payload"
	if cached, ok := s.cache.Get(feedCacheKey); ok {
		if hit, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, hit)
			return
		}
	}

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		writeAPIError(w, r, http.StatusInternalServerError, "feed_state_error", "failed to read feed state")
		return
	}

	win, err := s.repo.GetFeedCalendarWindow(ctx)
	if err != nil {
		log.Printf("feed active window: %v", err)
		writeAPIError(w, r, http.StatusInternalServerError, "calendar_window_error", "failed to read calendar coverage")
		return
	}

	payload := models.FeedActivePayload{
		CalendarWindow:     win,
		LastSuccessfulSync: status.LastSuccessfulSync,
	}

	resp := models.ResponseEnvelope{
		Status: status,
		Data:   payload,
	}
	s.cache.SetWithTTL(feedCacheKey, resp, feedActiveCacheTTL)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListCalendar(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	q := r.URL.Query()
	limit := clamp(parseIntOr(q.Get("limit"), defaultCalendarLimit), 1, maxCalendarLimit)
	page := clamp(parseIntOr(q.Get("page"), 1), 1, 1_000_000)

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	services, total, err := s.repo.ListCalendarServices(ctx, page, limit)
	if err != nil {
		log.Printf("list calendar: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch calendar services")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   services,
		Meta: &models.Meta{
			Total:   total,
			Limit:   limit,
			Page:    page,
			HasNext: page*limit < total,
		},
	})
}

func (s *Server) handleGetCalendarService(w http.ResponseWriter, r *http.Request) {
	serviceID := r.PathValue("serviceId")
	if serviceID == "" {
		httpError(w, http.StatusBadRequest, "missing service id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	svc, err := s.repo.GetCalendarService(ctx, serviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "calendar service not found")
		return
	}
	if err != nil {
		log.Printf("get calendar service: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch calendar service")
		return
	}

	writeJSON(w, http.StatusOK, models.ResponseEnvelope{
		Status: status,
		Data:   svc,
	})
}

// ============================================================================
// Realtime
// ============================================================================

func (s *Server) handleVehicles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	inferRoute := !(strings.EqualFold(strings.TrimSpace(q.Get("inferRoute")), "false") || strings.TrimSpace(q.Get("inferRoute")) == "0")
	cacheKey := "vehicles_payload:infer=" + strconv.FormatBool(inferRoute)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if hit, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, hit)
			return
		}
	}

	ctxDur := requestTimeout
	if inferRoute {
		ctxDur = mapOverlayTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), ctxDur)
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
	if inferRoute {
		if err = s.repo.EnrichVehiclesWithInferredRoutes(ctx, vehicles); err != nil {
			log.Printf("vehicles infer route: %v", err)
			// Keep /v1/map/vehicles available even if heuristic inference fails in prod;
			// clients still receive canonical feed/trip-resolved routes.
		}
	}

	resp := models.ResponseEnvelope{
		Status: status,
		Data:   vehicles,
		Meta:   &models.Meta{Total: len(vehicles)},
	}
	s.cache.SetWithTTL(cacheKey, resp, vehiclesCacheTTL)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRoutesWithLiveVehicles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	wantHints := strings.EqualFold(q.Get("includeUnassignedHints"), "true") || q.Get("includeUnassignedHints") == "1"
	maxHints := clamp(parseIntOr(q.Get("maxUnassignedHints"), 80), 1, 150)
	cacheKey := "routes_with_live_vehicles:hints=" + strconv.FormatBool(wantHints) + ":n=" + strconv.Itoa(maxHints)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if hit, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, hit)
			return
		}
	}

	ctxDur := requestTimeout
	if wantHints {
		ctxDur = mapOverlayTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), ctxDur)
	defer cancel()

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	payload, err := s.repo.GetRoutesWithLiveVehicleCounts(ctx)
	if err != nil {
		log.Printf("routes with live vehicles: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to aggregate live vehicles by route")
		return
	}

	if wantHints {
		hints, errH := s.repo.GetUnassignedVehicleHints(ctx, maxHints, nil)
		if errH != nil {
			log.Printf("unassigned vehicle hints: %v", errH)
			httpError(w, http.StatusInternalServerError, "failed to build unassigned vehicle hints")
			return
		}
		payload.UnassignedHints = hints
	}

	resp := models.ResponseEnvelope{
		Status: status,
		Data:   payload,
		Meta:   &models.Meta{Total: len(payload.Routes)},
	}
	s.cache.SetWithTTL(cacheKey, resp, vehiclesCacheTTL)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTripLive(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	q := r.URL.Query()
	tripID := strings.TrimSpace(q.Get("tripId"))
	vehicleID := strings.TrimSpace(q.Get("vehicleId"))
	if tripID == "" && vehicleID == "" {
		httpError(w, http.StatusBadRequest, "tripId or vehicleId is required")
		return
	}
	if tripID == "" {
		var err error
		tripID, err = s.repo.ResolveTripIDForVehicle(ctx, vehicleID)
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "vehicle has no active trip")
			return
		}
		if err != nil {
			log.Printf("resolve trip from vehicle: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to resolve trip")
			return
		}
	}
	limit := clamp(parseIntOr(q.Get("limit"), 12), 1, 50)

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	cacheKey := "trip_live:" + tripID + ":" + strconv.Itoa(limit)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if env, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, env)
			return
		}
	}

	payload, err := s.repo.GetTripLiveSnapshot(ctx, tripID, time.Now(), limit)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "trip not found")
		return
	}
	if err != nil {
		log.Printf("trip live: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to load trip live data")
		return
	}
	if vehicleID != "" && payload.VehicleID == "" {
		payload.VehicleID = vehicleID
	}

	resp := models.ResponseEnvelope{
		Status: status,
		Data:   payload,
		Meta:   &models.Meta{Total: len(payload.UpcomingStops), Limit: limit},
	}
	s.cache.SetWithTTL(cacheKey, resp, vehiclesCacheTTL)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleETABetweenStops(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	q := r.URL.Query()
	tripID := strings.TrimSpace(q.Get("tripId"))
	vehicleID := strings.TrimSpace(q.Get("vehicleId"))
	fromStopID := strings.TrimSpace(q.Get("fromStopId"))
	toStopID := strings.TrimSpace(q.Get("toStopId"))
	if (tripID == "" && vehicleID == "") || fromStopID == "" || toStopID == "" {
		httpError(w, http.StatusBadRequest, "tripId or vehicleId and both fromStopId/toStopId are required")
		return
	}
	if tripID == "" {
		var err error
		tripID, err = s.repo.ResolveTripIDForVehicle(ctx, vehicleID)
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "vehicle has no active trip")
			return
		}
		if err != nil {
			log.Printf("resolve trip from vehicle: %v", err)
			httpError(w, http.StatusInternalServerError, "failed to resolve trip")
			return
		}
	}

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	cacheKey := "eta_between:" + tripID + "|" + fromStopID + "|" + toStopID
	if cached, ok := s.cache.Get(cacheKey); ok {
		if env, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, env)
			return
		}
	}

	payload, err := s.repo.GetTripLiveSnapshot(ctx, tripID, time.Now(), 50)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "trip not found")
		return
	}
	if err != nil {
		log.Printf("eta between stops: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to compute eta")
		return
	}

	fromIdx, toIdx := -1, -1
	for i := range payload.UpcomingStops {
		if payload.UpcomingStops[i].StopID == fromStopID && fromIdx < 0 {
			fromIdx = i
		}
		if payload.UpcomingStops[i].StopID == toStopID && toIdx < 0 {
			toIdx = i
		}
	}
	if fromIdx < 0 || toIdx < 0 {
		httpError(w, http.StatusNotFound, "requested stops not found in upcoming trip segment")
		return
	}
	if toIdx < fromIdx {
		httpError(w, http.StatusBadRequest, "toStopId must be after fromStopId on upcoming trip segment")
		return
	}

	fromETA := payload.UpcomingStops[fromIdx].ETAMinutes
	toETA := payload.UpcomingStops[toIdx].ETAMinutes
	out := models.ETABetweenStopsPayload{
		VehicleID:              payload.VehicleID,
		TripID:                 payload.TripID,
		FromStopID:             fromStopID,
		FromStopName:           payload.UpcomingStops[fromIdx].StopName,
		ToStopID:               toStopID,
		ToStopName:             payload.UpcomingStops[toIdx].StopName,
		FromStopETA:            fromETA,
		ToStopETA:              toETA,
		BetweenStopsETAMinutes: toETA - fromETA,
		IsRealtime:             payload.UpcomingStops[fromIdx].IsRealtime || payload.UpcomingStops[toIdx].IsRealtime,
	}
	if out.BetweenStopsETAMinutes < 0 {
		out.BetweenStopsETAMinutes = 0
	}
	resp := models.ResponseEnvelope{
		Status: status,
		Data:   out,
	}
	s.cache.SetWithTTL(cacheKey, resp, vehiclesCacheTTL)
	writeJSON(w, http.StatusOK, resp)
}

// handleRealtimeTripUpdates returns rows from REALTIME_TRIP_UPDATES_TABLE (default trip_updates_current)
// as a JSON array so any column layout from your ingest pipeline is forwarded to the app.
func (s *Server) handleRealtimeTripUpdates(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	offset, bad := parseCursorOffset(r.URL.Query().Get("cursor"))
	if bad {
		httpError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	limit := clamp(parseIntOr(r.URL.Query().Get("limit"), defaultRealtimeLimit), 1, maxRealtimeLimit)
	tbl := strings.TrimSpace(s.cfg.RealtimeTripUpdatesTable)
	cacheKey := "rt_trip_updates:" + tbl + ":" + strconv.Itoa(limit) + ":" + strconv.Itoa(offset)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if env, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, env)
			return
		}
	}

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	raw, err := s.repo.SelectTableAsJSONArrayCursor(ctx, tbl, limit, offset)
	if err != nil {
		log.Printf("realtime trip-updates: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch trip updates")
		return
	}

	resp := models.ResponseEnvelope{
		Status: status,
		Data:   raw,
		Meta:   &models.Meta{Total: jsonArrayLen(raw), Limit: limit, NextCursor: nextCursor(raw, offset, limit)},
	}
	s.cache.SetWithTTL(cacheKey, resp, realtimeTripCacheTTL)
	writeJSON(w, http.StatusOK, resp)
}

// handleRealtimeAlerts returns rows from REALTIME_ALERTS_TABLE (default service_alerts_current) as JSON array.
func (s *Server) handleRealtimeAlerts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	offset, bad := parseCursorOffset(r.URL.Query().Get("cursor"))
	if bad {
		httpError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	limit := clamp(parseIntOr(r.URL.Query().Get("limit"), defaultRealtimeLimit), 1, maxRealtimeLimit)
	tbl := strings.TrimSpace(s.cfg.RealtimeAlertsTable)
	cacheKey := "rt_alerts:" + tbl + ":" + strconv.Itoa(limit) + ":" + strconv.Itoa(offset)
	if cached, ok := s.cache.Get(cacheKey); ok {
		if env, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, env)
			return
		}
	}

	status, err := s.repo.GetAPIStatus(ctx, s.cfg.APIDegradedMinutes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to read feed state")
		return
	}

	raw, err := s.repo.SelectTableAsJSONArrayCursor(ctx, tbl, limit, offset)
	if err != nil {
		log.Printf("realtime alerts: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to fetch alerts")
		return
	}

	resp := models.ResponseEnvelope{
		Status: status,
		Data:   raw,
		Meta:   &models.Meta{Total: jsonArrayLen(raw), Limit: limit, NextCursor: nextCursor(raw, offset, limit)},
	}
	s.cache.SetWithTTL(cacheKey, resp, realtimeAlertsCacheTTL)
	writeJSON(w, http.StatusOK, resp)
}

func jsonArrayLen(raw json.RawMessage) int {
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	return len(arr)
}

func parseCursorOffset(cursor string) (int, bool) {
	c := strings.TrimSpace(cursor)
	if c == "" {
		return 0, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, true
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(decoded)))
	if err != nil || v < 0 {
		return 0, true
	}
	return v, false
}

func nextCursor(raw json.RawMessage, offset, limit int) string {
	return nextCursorForCount(jsonArrayLen(raw), offset, limit)
}

func nextCursorForCount(count, offset, limit int) string {
	if count < limit {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset + limit)))
}

func encodePolyline(points []models.RouteShapePoint) string {
	if len(points) == 0 {
		return ""
	}
	var b strings.Builder
	prevLat, prevLon := 0, 0
	for _, p := range points {
		lat := int(math.Round(p.Lat * 1e5))
		lon := int(math.Round(p.Lon * 1e5))
		encodeSigned(&b, lat-prevLat)
		encodeSigned(&b, lon-prevLon)
		prevLat, prevLon = lat, lon
	}
	return b.String()
}

func encodeSigned(b *strings.Builder, value int) {
	s := value << 1
	if value < 0 {
		s = ^s
	}
	for s >= 0x20 {
		b.WriteByte(byte((0x20 | (s & 0x1f)) + 63))
		s >>= 5
	}
	b.WriteByte(byte(s + 63))
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

type favoriteRoutePayload struct {
	RouteID string `json:"route_id"`
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

func (s *Server) handleListFavoriteRoutes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	userID, err := s.requireUserID(ctx, r)
	if err != nil {
		httpError(w, http.StatusUnauthorized, err.Error())
		return
	}

	cacheKey := "favorite_routes:" + userID
	if cached, ok := s.cache.Get(cacheKey); ok {
		if env, ok := cached.(models.ResponseEnvelope); ok {
			writeJSON(w, http.StatusOK, env)
			return
		}
	}
	favorites, err := s.repo.ListFavoriteRoutes(ctx, userID)
	if err != nil {
		log.Printf("list favorite routes: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to list favorite routes")
		return
	}
	resp := models.ResponseEnvelope{
		Status: models.APIStatus{Mode: models.ModeNormal},
		Data:   favorites,
		Meta:   &models.Meta{Total: len(favorites)},
	}
	s.cache.SetWithTTL(cacheKey, resp, 20*time.Second)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAddFavoriteRoute(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	userID, err := s.requireUserID(ctx, r)
	if err != nil {
		httpError(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body favoriteRoutePayload
	if err = json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	routeID := strings.TrimSpace(body.RouteID)
	if routeID == "" {
		httpError(w, http.StatusBadRequest, "route_id is required")
		return
	}
	if err = s.repo.AddFavoriteRoute(ctx, userID, routeID); err != nil {
		log.Printf("add favorite route: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to add favorite route")
		return
	}
	s.cache.Invalidate("favorite_routes:" + userID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteFavoriteRoute(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	userID, err := s.requireUserID(ctx, r)
	if err != nil {
		httpError(w, http.StatusUnauthorized, err.Error())
		return
	}
	routeID := strings.TrimSpace(r.PathValue("routeId"))
	if routeID == "" {
		httpError(w, http.StatusBadRequest, "missing route id")
		return
	}
	if err = s.repo.DeleteFavoriteRoute(ctx, userID, routeID); err != nil {
		log.Printf("delete favorite route: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to delete favorite route")
		return
	}
	s.cache.Invalidate("favorite_routes:" + userID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ============================================================================
// Helpers
// ============================================================================

func bearerToken(r *http.Request) string {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		return ""
	}
	parts := strings.SplitN(authz, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func (s *Server) requireUserID(ctx context.Context, r *http.Request) (string, error) {
	if s.auth == nil || !s.auth.Enabled() {
		return "", errors.New("supabase auth is not configured")
	}
	token := bearerToken(r)
	if token == "" {
		return "", errors.New("missing bearer token")
	}
	user, statusCode, err := s.auth.GetUser(ctx, token)
	if err != nil {
		if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
			return "", errors.New("invalid bearer token")
		}
		return "", fmt.Errorf("auth user lookup failed")
	}
	return user.ID, nil
}

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

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// writeAPIError returns { "error": { "code", "message", "request_id" } } and sets X-Request-ID.
func writeAPIError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	rid := r.Header.Get("X-Request-ID")
	if rid == "" {
		rid = r.Header.Get("X-Request-Id")
	}
	if rid == "" {
		rid = newRequestID()
	}
	w.Header().Set("X-Request-ID", rid)
	writeJSON(w, status, models.ErrorEnvelope{
		Error: models.APIError{Code: code, Message: message, RequestID: rid},
	})
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

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		path := strings.TrimSuffix(r.URL.Path, "/")
		if path == "" {
			path = "/"
		}

		if s.isPublicAuthRoute(r.Method, path) {
			next.ServeHTTP(w, r)
			return
		}
		if s.isPublicDocsRoute(r.Method, path) {
			next.ServeHTTP(w, r)
			return
		}
		if path == "/healthz" && r.Method == http.MethodGet && s.healthcheckAuthorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		if path == "/v1/vehicle-position" && r.Method == http.MethodPost && s.vehicleIngestAuthorized(r) {
			next.ServeHTTP(w, r)
			return
		}

		if _, err := s.requireUserID(r.Context(), r); err != nil {
			httpError(w, http.StatusUnauthorized, err.Error())
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) isPublicAuthRoute(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/v1/auth/signup", "/v1/auth/login", "/v1/auth/verify-otp":
		return true
	default:
		return false
	}
}

// isPublicDocsRoute serves Swagger/OpenAPI HTML and YAML without a token so you can open /swagger in a browser,
// call the public auth endpoints, then use “Authorize” with the Supabase access_token for all other operations.
func (s *Server) isPublicDocsRoute(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	switch path {
	case "/", "/openapi.yaml", "/swagger":
		return true
	default:
		return strings.HasPrefix(path, "/swagger/")
	}
}

func (s *Server) healthcheckAuthorized(r *http.Request) bool {
	secret := strings.TrimSpace(s.cfg.HealthcheckSecret)
	if secret == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Healthcheck-Secret"))
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

func (s *Server) vehicleIngestAuthorized(r *http.Request) bool {
	secret := strings.TrimSpace(s.cfg.VehicleIngestKey)
	if secret == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Vehicle-Ingest-Key"))
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}
