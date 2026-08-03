package config

import (
	"log/slog"
	"os"
)

// NewLogger returns a JSON slog logger writing to stdout at cfg.LogLevel.
func NewLogger(cfg Config) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	return slog.New(h)
}
