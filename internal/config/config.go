// Package config centralises all runtime configuration for FeedRank.
//
// Config is loaded once at startup from environment variables. No config
// files, no YAML — env vars are the single source of truth in keeping with
// 12-factor principles and Docker-Compose/k8s deployments.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all runtime configuration for FeedRank.
type Config struct {
	// DatabaseURL is the full postgres connection string.
	// Example: postgres://feedrank:feedrank@localhost:5432/feedrank?sslmode=disable
	DatabaseURL string

	// RedisURL is the Redis server address (host:port, no scheme).
	// Example: localhost:6379
	RedisURL string

	// Port is the HTTP port the API server listens on.
	Port string

	// CelebrityThreshold is the follower count at which an account is
	// classified as a celebrity. Posts by celebrity accounts are NOT fanned
	// out to followers' timelines; they are fetched on-demand at read time.
	//
	// Default: 10000
	CelebrityThreshold int

	// TimelineCap is the maximum number of post IDs stored per user's
	// Redis timeline sorted set. Older entries are trimmed on every ZADD.
	// This keeps memory usage bounded — Postgres is the fallback for deep
	// scrollback.
	//
	// Default: 500
	TimelineCap int

	// FanoutMode controls which fan-out strategy the worker uses.
	//
	//   "hybrid" (default) — fan-out-on-write for normal accounts,
	//                        fan-out-on-read for celebrities.
	//   "pure"             — fan-out-on-write for ALL accounts, including
	//                        celebrities. Used only for load-test comparison.
	FanoutMode string
}

// Load reads all environment variables and returns a populated Config.
// It returns an error if any required variable is missing or malformed.
func Load() (*Config, error) {
	cfg := &Config{}

	cfg.DatabaseURL = requireEnv("DATABASE_URL")
	cfg.RedisURL = requireEnv("REDIS_URL")
	cfg.Port = getEnv("PORT", "8080")
	cfg.FanoutMode = getEnv("FANOUT_MODE", "hybrid")

	var err error
	cfg.CelebrityThreshold, err = getEnvInt("CELEBRITY_THRESHOLD", 10_000)
	if err != nil {
		return nil, fmt.Errorf("config: CELEBRITY_THRESHOLD: %w", err)
	}

	cfg.TimelineCap, err = getEnvInt("TIMELINE_CAP", 500)
	if err != nil {
		return nil, fmt.Errorf("config: TIMELINE_CAP: %w", err)
	}

	if cfg.FanoutMode != "hybrid" && cfg.FanoutMode != "pure" {
		return nil, fmt.Errorf("config: FANOUT_MODE must be \"hybrid\" or \"pure\", got %q", cfg.FanoutMode)
	}

	return cfg, nil
}

// requireEnv returns the value of an environment variable, panicking if it is
// not set. Call this only for variables that have no meaningful default.
func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("required environment variable %q is not set", key))
	}
	return v
}

// getEnv returns the value of an environment variable, or a default if unset.
func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// getEnvInt parses an integer environment variable, returning defaultVal if
// the variable is unset, and an error if it is set but not a valid integer.
func getEnvInt(key string, defaultVal int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q: %w", v, err)
	}
	return n, nil
}
