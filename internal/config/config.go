package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Port                      string
	SupabaseDBURL             string
	SupabaseURL               string
	SupabaseAnonKey           string
	MaxDBConns                int32
	APIDegradedMinutes        int
	NearbyDefaultRadiusMeters int
	SnapshotRefreshMinutes    int
	// HealthcheckSecret, if set, allows GET /healthz without a user JWT when header
	// X-Healthcheck-Secret matches (for uptime probes). All other routes still require Bearer auth.
	HealthcheckSecret string
	// RealtimeTripUpdatesTable / RealtimeAlertsTable: Postgres table (or view) names for live GTFS-RT style feeds.
	// Override if your ingest uses different names (must be identifier-safe: letters, digits, underscore).
	RealtimeTripUpdatesTable string
	RealtimeAlertsTable      string
}

func Load() (Config, error) {
	cfg := Config{
		Port:                      getenv("PORT", "8080"),
		SupabaseDBURL:             os.Getenv("SUPABASE_DB_URL"),
		SupabaseURL:               os.Getenv("SUPABASE_URL"),
		SupabaseAnonKey:           os.Getenv("SUPABASE_ANON_KEY"),
		MaxDBConns:                int32(getenvInt("MAX_DB_CONNS", 20)),
		APIDegradedMinutes:        getenvInt("API_DEGRADED_MINUTES", 15),
		NearbyDefaultRadiusMeters: getenvInt("NEARBY_DEFAULT_RADIUS_METERS", 1000),
		SnapshotRefreshMinutes:    getenvInt("SNAPSHOT_REFRESH_MINUTES", 10),
		HealthcheckSecret:         os.Getenv("HEALTHCHECK_SECRET"),
		RealtimeTripUpdatesTable:  getenv("REALTIME_TRIP_UPDATES_TABLE", "trip_updates_current"),
		RealtimeAlertsTable:       getenv("REALTIME_ALERTS_TABLE", "service_alerts_current"),
	}

	if cfg.SupabaseDBURL == "" {
		return Config{}, fmt.Errorf("missing SUPABASE_DB_URL")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

