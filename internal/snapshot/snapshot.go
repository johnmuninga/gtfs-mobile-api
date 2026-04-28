package snapshot

import (
	"context"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"backend_mobile_app_go/internal/models"
	"backend_mobile_app_go/internal/repository"
)

const earthRadiusMeters = 6371000.0

// Snapshot keeps an in-memory copy of static GTFS data so the API can serve
// most reads without hitting the database. It is refreshed periodically.
type Snapshot struct {
	mu              sync.RWMutex
	routes          []models.Route
	stops           []models.StopSummary
	refreshedAt     time.Time
	loaded          bool
	refreshInterval time.Duration
	repo            *repository.Repository
}

func New(repo *repository.Repository, refreshInterval time.Duration) *Snapshot {
	return &Snapshot{
		repo:            repo,
		refreshInterval: refreshInterval,
	}
}

func (s *Snapshot) Refresh(ctx context.Context) error {
	routes, err := s.repo.ListAllRoutes(ctx)
	if err != nil {
		return err
	}
	stops, err := s.repo.ListAllStops(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.routes = routes
	s.stops = stops
	s.refreshedAt = time.Now()
	s.loaded = true
	s.mu.Unlock()
	return nil
}

func (s *Snapshot) Loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded
}

func (s *Snapshot) RefreshedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.refreshedAt
}

// StartBackgroundRefresh launches a goroutine that re-fetches the snapshot
// at the configured interval. It returns when ctx is cancelled.
func (s *Snapshot) StartBackgroundRefresh(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(s.refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := s.Refresh(rctx); err != nil {
					log.Printf("snapshot refresh failed: %v", err)
				} else {
					log.Printf("snapshot refreshed: routes=%d stops=%d", s.RoutesCount(), s.StopsCount())
				}
				cancel()
			}
		}
	}()
}

func (s *Snapshot) RoutesCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.routes)
}

func (s *Snapshot) StopsCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.stops)
}

// FilterRoutes performs in-memory search over the cached routes.
func (s *Snapshot) FilterRoutes(search, routeType string, limit int) []models.Route {
	s.mu.RLock()
	all := s.routes
	s.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(search))
	out := make([]models.Route, 0, min(limit, len(all)))
	for i := range all {
		r := all[i]
		if routeType != "" && r.RouteType != routeType {
			continue
		}
		if q != "" {
			if !strings.Contains(strings.ToLower(r.ShortName), q) &&
				!strings.Contains(strings.ToLower(r.LongName), q) {
				continue
			}
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// FilterStops performs in-memory search over cached stops with optional text
// match and geo radius (haversine distance in meters).
func (s *Snapshot) FilterStops(search string, lat, lon *float64, radiusMeters, page, limit int) ([]models.StopSummary, int) {
	s.mu.RLock()
	all := s.stops
	s.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(search))
	useGeo := lat != nil && lon != nil

	type scored struct {
		stop models.StopSummary
		dist float64
	}
	tmp := make([]scored, 0, 256)

	for i := range all {
		st := all[i]
		if q != "" {
			if !strings.Contains(strings.ToLower(st.StopName), q) &&
				!strings.Contains(strings.ToLower(st.StopCode), q) {
				continue
			}
		}

		dist := math.Inf(1)
		if useGeo {
			dist = haversineMeters(*lat, *lon, st.Lat, st.Lon)
			if radiusMeters > 0 && dist > float64(radiusMeters) {
				continue
			}
		}
		tmp = append(tmp, scored{st, dist})
	}

	if useGeo {
		sort.Slice(tmp, func(i, j int) bool { return tmp[i].dist < tmp[j].dist })
	} else {
		sort.Slice(tmp, func(i, j int) bool {
			return strings.ToLower(tmp[i].stop.StopName) < strings.ToLower(tmp[j].stop.StopName)
		})
	}

	total := len(tmp)
	offset := (page - 1) * limit
	if offset >= len(tmp) {
		return []models.StopSummary{}, total
	}
	end := offset + limit
	if end > len(tmp) {
		end = len(tmp)
	}
	tmp = tmp[offset:end]

	out := make([]models.StopSummary, len(tmp))
	for i := range tmp {
		out[i] = tmp[i].stop
	}
	return out, total
}

func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	toRad := math.Pi / 180.0
	dLat := (lat2 - lat1) * toRad
	dLon := (lon2 - lon1) * toRad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*toRad)*math.Cos(lat2*toRad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMeters * c
}
