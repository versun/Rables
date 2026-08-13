package httpd

import (
	"io"
	"log/slog"
	"testing"

	"rables/internal/config"
	"rables/internal/db"
)

// TestSetupIncompleteErrorNotCached: a database error fails open (reports
// setup complete) but is not cached, so the next request retries against the
// database — matching the redirectRules cache behavior.
func TestSetupIncompleteErrorNotCached(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := NewServer(database, config.Config{HMACSecret: "x"}, logger, nil)
	database.Close()

	if s.setupIncomplete(t.Context()) {
		t.Error("setupIncomplete on closed db = true, want fail-open false")
	}
	if _, ok := s.Ext.Load(setupCacheKey); ok {
		t.Error("error verdict was cached; the next request would not retry the database")
	}
}
