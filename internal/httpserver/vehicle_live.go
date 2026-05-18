package httpserver

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"backend_mobile_app_go/internal/models"

	"github.com/gorilla/websocket"
)

type vehicleIngestPayload struct {
	VehicleID string   `json:"vehicle_id"`
	TripID    string   `json:"trip_id,omitempty"`
	RouteID   string   `json:"route_id,omitempty"`
	Lat       float64  `json:"lat"`
	Lon       float64  `json:"lon"`
	Bearing   *float64 `json:"bearing,omitempty"`
	Speed     *float64 `json:"speed,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"` // RFC3339 optional
}

type vehicleLiveEvent struct {
	Type    string         `json:"type"`
	TS      time.Time      `json:"ts"`
	Vehicle models.Vehicle `json:"vehicle"`
}

type vehicleLiveHub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]struct{}
}

func newVehicleLiveHub() *vehicleLiveHub {
	return &vehicleLiveHub{clients: make(map[*websocket.Conn]struct{})}
}

func (h *vehicleLiveHub) add(conn *websocket.Conn) {
	h.mu.Lock()
	h.clients[conn] = struct{}{}
	h.mu.Unlock()
}

func (h *vehicleLiveHub) remove(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
	_ = conn.Close()
}

func (h *vehicleLiveHub) broadcast(msg any) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err = c.WriteMessage(websocket.TextMessage, payload); err != nil {
			h.remove(c)
		}
	}
}

var vehiclesWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// JWT auth is enforced by middleware before upgrade.
		return true
	},
}

func (s *Server) handleVehiclesLive(w http.ResponseWriter, r *http.Request) {
	conn, err := vehiclesWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.live.add(conn)
	defer s.live.remove(conn)

	// Push a current snapshot immediately so clients can hydrate in one socket.
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	vehicles, err := s.repo.GetVehicles(ctx)
	cancel()
	if err == nil {
		for i := range vehicles {
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if writeErr := conn.WriteJSON(vehicleLiveEvent{
				Type:    "vehicle.upsert",
				TS:      time.Now().UTC(),
				Vehicle: vehicles[i],
			}); writeErr != nil {
				return
			}
		}
	}

	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, readErr := conn.ReadMessage(); readErr != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if err = conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleVehiclePositionIngest(w http.ResponseWriter, r *http.Request) {
	var body vehicleIngestPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	body.VehicleID = strings.TrimSpace(body.VehicleID)
	body.TripID = strings.TrimSpace(body.TripID)
	body.RouteID = strings.TrimSpace(body.RouteID)
	if body.VehicleID == "" {
		httpError(w, http.StatusBadRequest, "vehicle_id is required")
		return
	}
	if body.Lat < -90 || body.Lat > 90 || body.Lon < -180 || body.Lon > 180 {
		httpError(w, http.StatusBadRequest, "invalid lat/lon")
		return
	}

	updatedAt := time.Now().UTC()
	if strings.TrimSpace(body.UpdatedAt) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(body.UpdatedAt))
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid updated_at (use RFC3339)")
			return
		}
		updatedAt = t.UTC()
	}

	v := models.Vehicle{
		VehicleID: body.VehicleID,
		TripID:    body.TripID,
		RouteID:   body.RouteID,
		Lat:       body.Lat,
		Lon:       body.Lon,
		Bearing:   body.Bearing,
		Speed:     body.Speed,
		UpdatedAt: updatedAt,
		IsLive:    true,
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if err := s.repo.UpsertVehiclePosition(ctx, v); err != nil {
		log.Printf("vehicle ingest upsert: %v", err)
		httpError(w, http.StatusInternalServerError, "failed to store vehicle position")
		return
	}

	// Drop short-lived vehicle caches so next REST reads see latest state.
	s.cache.Invalidate("vehicles_payload:infer=true")
	s.cache.Invalidate("vehicles_payload:infer=false")
	// Only push to map clients when the vehicle is logged on to a GTFS trip.
	if body.TripID != "" {
		s.live.broadcast(vehicleLiveEvent{Type: "vehicle.upsert", TS: time.Now().UTC(), Vehicle: v})
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "ok"})
}
