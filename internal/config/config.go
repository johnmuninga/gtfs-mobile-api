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
	TLSCertFile               string
	TLSKeyFile                string
	ForceHTTPSRedirect        bool
	HTTPRedirectPort          string
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
		TLSCertFile:               os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:                os.Getenv("TLS_KEY_FILE"),
		ForceHTTPSRedirect:        getenvBool("FORCE_HTTPS_REDIRECT", false),
		HTTPRedirectPort:          getenv("HTTP_REDIRECT_PORT", "8080"),
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

func getenvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}
