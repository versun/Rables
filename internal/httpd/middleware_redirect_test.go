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

// insertRedirect stores one redirect row directly; an empty matchOn means a
// path rule.
func insertRedirect(t *testing.T, s *Server, regex, replacement string, permanent, enabled int64, matchOn string) query.Redirect {
	t.Helper()
	if matchOn == "" {
		matchOn = redirectMatchPath
	}
	now := time.Now().Unix()
	redirect, err := s.Q.CreateRedirect(t.Context(), query.CreateRedirectParams{
		Regex:       regex,
		Replacement: replacement,
		Permanent:   permanent,
		Enabled:     enabled,
		MatchOn:     matchOn,
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
			name: "named capture group substituted", method: http.MethodGet, path: "/posts/42",
			rules:      []query.Redirect{{Regex: "^/posts/(?<id>.+)$", Replacement: "/articles/\\k<id>", Enabled: 1}},
			wantStatus: http.StatusFound, wantLocation: "/articles/42",
		},
		{
			name: "named capture quote form substituted", method: http.MethodGet, path: "/posts/42",
			rules:      []query.Redirect{{Regex: "^/posts/(?<id>.+)$", Replacement: "/articles/\\k'id'", Enabled: 1}},
			wantStatus: http.StatusFound, wantLocation: "/articles/42",
		},
		{
			name: "whole match substituted", method: http.MethodGet, path: "/a/old/b",
			rules:      []query.Redirect{{Regex: "old", Replacement: "[\\&]", Enabled: 1}},
			wantStatus: http.StatusFound, wantLocation: "/a/[old]/b",
		},
		{
			name: "partial match substitutes span", method: http.MethodGet, path: "/a/old/b",
			rules:      []query.Redirect{{Regex: "old", Replacement: "new", Enabled: 1}},
			wantStatus: http.StatusFound, wantLocation: "/a/new/b",
		},
		{
			name: "slug sharing skip prefix redirected", method: http.MethodGet, path: "/updates-2024",
			rules:      []query.Redirect{{Regex: "^/updates-2024$", Replacement: "/articles/updates-2024", Enabled: 1}},
			wantStatus: http.StatusFound, wantLocation: "/articles/updates-2024",
		},
		{
			name: "exact skip prefix still skipped", method: http.MethodGet, path: "/admin",
			rules:      []query.Redirect{{Regex: "admin", Replacement: "new", Enabled: 1}},
			wantStatus: http.StatusOK,
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
				insertRedirect(t, s, rule.Regex, rule.Replacement, rule.Permanent, rule.Enabled, rule.MatchOn)
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
	insertRedirect(t, s, "^/old$", "/new", 0, 1, "")

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

// TestRedirectMiddlewareHostRules covers the match_on='host' extension:
// subdomain redirects against the request Host, ahead of every path rule.
func TestRedirectMiddlewareHostRules(t *testing.T) {
	tests := []struct {
		name         string
		siteURL      string // configured settings.url; empty = unset
		rules        []query.Redirect
		method       string // empty = GET
		url          string
		wantStatus   int
		wantLocation string
	}{
		{
			name: "exact host to external URL",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://abc.example.com/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "capture group into absolute target",
			rules: []query.Redirect{
				{Regex: `^(\d+)\.example\.com$`, Replacement: `https://example.com/tags/\1`, Enabled: 1, MatchOn: "host"},
			},
			url:          "http://54321.example.com/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/tags/54321",
		},
		{
			name:    "relative target completed with site URL",
			siteURL: "https://example.com",
			rules: []query.Redirect{
				{Regex: `^abcd\.example\.com$`, Replacement: `/blog/abcd`, Enabled: 1, MatchOn: "host"},
			},
			url:          "http://abcd.example.com/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/blog/abcd",
		},
		{
			name: "relative target without site URL fails open",
			rules: []query.Redirect{
				{Regex: `^abcd\.example\.com$`, Replacement: `/blog/abcd`, Enabled: 1, MatchOn: "host"},
			},
			url:        "http://abcd.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "request path and query never appended",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://abc.example.com/foo/bar?x=1",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "host rule fires on skip-prefix path",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://abc.example.com/admin",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "host rule beats earlier path rule",
			rules: []query.Redirect{
				{Regex: "^/old$", Replacement: "/path-rule", Enabled: 1},
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://abc.example.com/old",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "host with port matches",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://abc.example.com:8080/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "host case-insensitive",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://ABC.Example.COM/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "apex host does not match subdomain rule",
			rules: []query.Redirect{
				{Regex: `^([^.]+)\.example\.com$`, Replacement: `https://example.com/\1`, Enabled: 1, MatchOn: "host"},
			},
			url:        "http://example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "disabled host rule skipped",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 0, MatchOn: "host"},
			},
			url:        "http://abc.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name:   "POST not redirected by host rule",
			method: http.MethodPost,
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:        "http://abc.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "non-matching host falls through to path rules",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
				{Regex: "^/old$", Replacement: "/new", Enabled: 1},
			},
			url:          "http://www.example.com/old",
			wantStatus:   http.StatusFound,
			wantLocation: "/new",
		},
		{
			name: "permanent host rule is 301",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Permanent: 1, Enabled: 1, MatchOn: "host"},
			},
			url:          "http://abc.example.com/",
			wantStatus:   http.StatusMovedPermanently,
			wantLocation: "https://google.com",
		},
		{
			name: "unanchored regex still matches whole host only",
			rules: []query.Redirect{
				{Regex: `abc\.example\.com`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://abc.example.com/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "host suffix beyond a missing-$ regex does not leak into target",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:        "http://abc.example.com.evil.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "host prefix beyond a missing-^ regex does not match",
			rules: []query.Redirect{
				{Regex: `abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:        "http://evil-abc.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "trailing-dot FQDN matches",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://abc.example.com./",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "IPv6 literal with port matches bracketless pattern",
			rules: []query.Redirect{
				{Regex: `^::1$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://[::1]:8080/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "IPv6 literal without port matches bracketless pattern",
			rules: []query.Redirect{
				{Regex: `^::1$`, Replacement: "https://google.com", Enabled: 1, MatchOn: "host"},
			},
			url:          "http://[::1]/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, h := newRedirectTestServer(t)
			if tt.siteURL != "" {
				setSiteURL(t, s, tt.siteURL)
			}
			for _, rule := range tt.rules {
				insertRedirect(t, s, rule.Regex, rule.Replacement, rule.Permanent, rule.Enabled, rule.MatchOn)
			}
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, tt.url, nil))
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tt.wantLocation {
				t.Errorf("location = %q, want %q", got, tt.wantLocation)
			}
		})
	}
}
