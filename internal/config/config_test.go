package config

import (
	"log/slog"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string // substring of expected error; empty means no error
	}{
		{
			name: "defaults with required secret",
			env:  map[string]string{"HMAC_SECRET": "s3cret"},
			want: Config{
				Addr:       ":8080",
				DataDir:    "./data",
				HMACSecret: "s3cret",
				LogLevel:   slog.LevelInfo,
			},
		},
		{
			name: "all values overridden",
			env: map[string]string{
				"ADDR":                 "127.0.0.1:9000",
				"DATA_DIR":             "/var/lib/rables",
				"HMAC_SECRET":          "abc",
				"ARTICLE_ROUTE_PREFIX": "posts",
				"LOG_LEVEL":            "debug",
			},
			want: Config{
				Addr:               "127.0.0.1:9000",
				DataDir:            "/var/lib/rables",
				HMACSecret:         "abc",
				ArticleRoutePrefix: "posts",
				LogLevel:           slog.LevelDebug,
			},
		},
		{
			name:    "missing HMAC_SECRET",
			env:     map[string]string{},
			wantErr: "HMAC_SECRET is required",
		},
		{
			name:    "empty HMAC_SECRET",
			env:     map[string]string{"HMAC_SECRET": ""},
			wantErr: "HMAC_SECRET is required",
		},
		{
			name:    "invalid LOG_LEVEL",
			env:     map[string]string{"HMAC_SECRET": "x", "LOG_LEVEL": "verbose"},
			wantErr: "invalid LOG_LEVEL",
		},
		{
			name: "warn and error levels",
			env:  map[string]string{"HMAC_SECRET": "x", "LOG_LEVEL": "error"},
			want: Config{
				Addr:       ":8080",
				DataDir:    "./data",
				HMACSecret: "x",
				LogLevel:   slog.LevelError,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"ADDR", "DATA_DIR", "HMAC_SECRET", "ARTICLE_ROUTE_PREFIX", "LOG_LEVEL"} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			got, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
