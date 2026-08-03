package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
)

// newRedirectTestServer builds a Server backed by a real SQLite DB and a chi
// router with the redirect middleware wrapping a fallback handler, like the
// integrator wiring it ahead of the public routes.
func newRedirectTestServer(t *testing.T) (*Server, *chi.Mux) {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := NewServer(database, config.Config{Addr: ":8080", DataDir: t.TempDir(), HMACSecret: "x"}, logger, nil)
	r := chi.NewRouter()
	r.Use(s.redirectMiddleware)
	fallback := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("passthrough"))
	}
	r.Get("/*", fallback)
	r.Post("/*", fallback)
	return s, r
}

// insertRedirect stores one redirect row directly.
func insertRedirect(t *testing.T, s *Server, regex, replacement string, permanent, enabled int64) query.Redirect {
	t.Helper()
	now := time.Now().Unix()
	redirect, err := s.Q.CreateRedirect(t.Context(), query.CreateRedirectParams{
		Regex:       regex,
		Replacement: replacement,
		Permanent:   permanent,
		Enabled:     enabled,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		t.Fatalf("insert redirect: %v", err)
	}
	return redirect
}

// TestRedirectMiddleware covers the status/matching matrix of
// app/middleware/redirect_middleware.rb.
func TestRedirectMiddleware(t *testing.T) {
	tests := []struct {
		name         string
		rules        []query.Redirect // inserted in order
		method       string
		path         string
		wantStatus   int
		wantLocation string
	}{
		{name: "no rules passthrough", method: http.MethodGet, path: "/anything", wantStatus: http.StatusOK},
		{
			name: "temporary is 302", method: http.MethodGet, path: "/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/new", Enabled: 1}},
			wantStatus: http.StatusFound, wantLocation: "/new",
		},
		{
			name: "permanent is 301", method: http.MethodGet, path: "/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/new", Permanent: 1, Enabled: 1}},
			wantStatus: http.StatusMovedPermanently, wantLocation: "/new",
		},
		{
			name: "first match wins", method: http.MethodGet, path: "/old",
			rules: []query.Redirect{
				{Regex: "^/old$", Replacement: "/first", Enabled: 1},
				{Regex: "^/o", Replacement: "/second", Permanent: 1, Enabled: 1},
			},
			wantStatus: http.StatusFound, wantLocation: "/first",
		},
		{
			name: "disabled rule skipped", method: http.MethodGet, path: "/old",
			rules: []query.Redirect{
				{Regex: "^/old$", Replacement: "/off", Enabled: 0},
				{Regex: "^/old$", Replacement: "/on", Enabled: 1},
			},
			wantStatus: http.StatusFound, wantLocation: "/on",
		},
		{
			name: "non-matching path passthrough", method: http.MethodGet, path: "/other",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/new", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "POST not redirected", method: http.MethodPost, path: "/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/new", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "HEAD redirected", method: http.MethodHead, path: "/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/new", Enabled: 1}},
			wantStatus: http.StatusFound, wantLocation: "/new",
		},
		{
			name: "capture groups substituted", method: http.MethodGet, path: "/posts/42",
			rules:      []query.Redirect{{Regex: "^/posts/(.+)$", Replacement: "/articles/\\1", Enabled: 1}},
			wantStatus: http.StatusFound, wantLocation: "/articles/42",
		},
		{
			name: "partial match substitutes span", method: http.MethodGet, path: "/a/old/b",
			rules:      []query.Redirect{{Regex: "old", Replacement: "new", Enabled: 1}},
			wantStatus: http.StatusFound, wantLocation: "/a/new/b",
		},
	}
	for _, prefix := range []string{"/admin", "/assets", "/files", "/static", "/up"} {
		tests = append(tests, struct {
			name         string
			rules        []query.Redirect
			method       string
			path         string
			wantStatus   int
			wantLocation string
		}{
			name: "skip prefix " + prefix, method: http.MethodGet, path: prefix + "/old",
			rules:      []query.Redirect{{Regex: "old", Replacement: "new", Enabled: 1}},
			wantStatus: http.StatusOK,
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, h := newRedirectTestServer(t)
			for _, rule := range tt.rules {
				insertRedirect(t, s, rule.Regex, rule.Replacement, rule.Permanent, rule.Enabled)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tt.wantLocation {
				t.Errorf("location = %q, want %q", got, tt.wantLocation)
			}
		})
	}
}

// TestRedirectMiddlewareCacheInvalidation: rules are cached, and
// InvalidateRedirectCache makes writes visible immediately.
func TestRedirectMiddlewareCacheInvalidation(t *testing.T) {
	s, h := newRedirectTestServer(t)
	insertRedirect(t, s, "^/old$", "/new", 0, 1)

	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/old", nil))
		return rec
	}

	if rec := get(); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/new" {
		t.Fatalf("before update: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}

	// Without invalidation the cached rule list still applies.
	if err := s.Q.UpdateRedirect(t.Context(), query.UpdateRedirectParams{
		Regex: "^/old$", Replacement: "/newer", Enabled: 1, UpdatedAt: time.Now().Unix(), ID: 1,
	}); err != nil {
		t.Fatalf("update redirect: %v", err)
	}
	if rec := get(); rec.Header().Get("Location") != "/new" {
		t.Fatalf("cached rules: location = %q, want /new", rec.Header().Get("Location"))
	}

	// After invalidation the new rule applies.
	s.InvalidateRedirectCache()
	if rec := get(); rec.Header().Get("Location") != "/newer" {
		t.Fatalf("after invalidation: location = %q, want /newer", rec.Header().Get("Location"))
	}

	// Deleting the rule plus invalidation restores passthrough.
	if err := s.Q.DeleteRedirect(t.Context(), 1); err != nil {
		t.Fatalf("delete redirect: %v", err)
	}
	s.InvalidateRedirectCache()
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("after delete: status = %d, want 200", rec.Code)
	}
}
