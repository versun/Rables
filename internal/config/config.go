// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// Config holds process-wide settings sourced from the environment.
type Config struct {
	Addr               string // listen address, e.g. ":8080"
	DataDir            string // root for the SQLite DB and uploaded files
	HMACSecret         string // signs math-captcha tokens; required
	ArticleRoutePrefix string // optional public route prefix for articles
	LogLevel           slog.Level
}

// Load reads configuration from the environment. HMAC_SECRET is required;
// ADDR defaults to ":8080", DATA_DIR to "./data", LOG_LEVEL to "info".
// An unknown LOG_LEVEL is an error (fail fast rather than silently degrade).
func Load() (Config, error) {
	cfg := Config{
		Addr:               getEnv("ADDR", ":8080"),
		DataDir:            getEnv("DATA_DIR", "./data"),
		HMACSecret:         os.Getenv("HMAC_SECRET"),
		ArticleRoutePrefix: os.Getenv("ARTICLE_ROUTE_PREFIX"),
		LogLevel:           slog.LevelInfo,
	}

	if cfg.HMACSecret == "" {
		return Config{}, errors.New("config: HMAC_SECRET is required")
	}

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		level, err := parseLevel(v)
		if err != nil {
			return Config{}, err
		}
		cfg.LogLevel = level
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseLevel(v string) (slog.Level, error) {
	switch v {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: invalid LOG_LEVEL %q (want debug|info|warn|error)", v)
	}
}
