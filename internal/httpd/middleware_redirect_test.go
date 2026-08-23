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

// insertRedirect stores one redirect row directly; Position follows creation
// order via the CreateRedirect MAX(position)+1 default.
func insertRedirect(t *testing.T, s *Server, rule query.Redirect) query.Redirect {
	t.Helper()
	now := time.Now().Unix()
	redirect, err := s.Q.CreateRedirect(t.Context(), query.CreateRedirectParams{
		Regex:       rule.Regex,
		Replacement: rule.Replacement,
		MatchFrom:   rule.MatchFrom,
		MatchPrefix: rule.MatchPrefix,
		MatchOn:     rule.MatchOn,
		Permanent:   rule.Permanent,
		Enabled:     rule.Enabled,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		t.Fatalf("insert redirect: %v", err)
	}
	return redirect
}

// redirectTestCase is one row of the redirect-middleware tables.
type redirectTestCase struct {
	name         string
	siteURL      string // configured settings.url; empty = unset
	rules        []query.Redirect
	method       string // empty = GET
	url          string // full URL (host rules) or path (path rules)
	wantStatus   int
	wantLocation string
}

// runRedirectTable executes one redirect-middleware table, one server per row.
func runRedirectTable(t *testing.T, tests []redirectTestCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, h := newRedirectTestServer(t)
			if tt.siteURL != "" {
				setSiteURL(t, s, tt.siteURL)
			}
			for _, rule := range tt.rules {
				insertRedirect(t, s, rule)
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

// TestRedirectMiddleware covers the status/matching matrix of
// app/middleware/redirect_middleware.rb (regex path rules).
func TestRedirectMiddleware(t *testing.T) {
	tests := []redirectTestCase{
		{name: "no rules passthrough", url: "/anything", wantStatus: http.StatusOK},
		{
			name: "temporary is 302", url: "/old",
			rules:        []query.Redirect{{Regex: "^/old$", Replacement: "/new", Enabled: 1}},
			wantStatus:   http.StatusFound,
			wantLocation: "/new",
		},
		{
			name: "permanent is 301", url: "/old",
			rules:        []query.Redirect{{Regex: "^/old$", Replacement: "/new", Permanent: 1, Enabled: 1}},
			wantStatus:   http.StatusMovedPermanently,
			wantLocation: "/new",
		},
		{
			name: "first match wins", url: "/old",
			rules: []query.Redirect{
				{Regex: "^/old$", Replacement: "/first", Enabled: 1},
				{Regex: "^/o", Replacement: "/second", Permanent: 1, Enabled: 1},
			},
			wantStatus:   http.StatusFound,
			wantLocation: "/first",
		},
		{
			name: "disabled rule skipped", url: "/old",
			rules: []query.Redirect{
				{Regex: "^/old$", Replacement: "/off", Enabled: 0},
				{Regex: "^/old$", Replacement: "/on", Enabled: 1},
			},
			wantStatus:   http.StatusFound,
			wantLocation: "/on",
		},
		{
			name: "non-matching path passthrough", url: "/other",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/new", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "POST not redirected", method: http.MethodPost, url: "/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/new", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "HEAD redirected", method: http.MethodHead, url: "/old",
			rules:        []query.Redirect{{Regex: "^/old$", Replacement: "/new", Enabled: 1}},
			wantStatus:   http.StatusFound,
			wantLocation: "/new",
		},
		{
			name: "capture groups substituted", url: "/posts/42",
			rules:        []query.Redirect{{Regex: "^/posts/(.+)$", Replacement: "/articles/\\1", Enabled: 1}},
			wantStatus:   http.StatusFound,
			wantLocation: "/articles/42",
		},
		{
			name: "named capture group substituted", url: "/posts/42",
			rules:        []query.Redirect{{Regex: "^/posts/(?<id>.+)$", Replacement: "/articles/\\k<id>", Enabled: 1}},
			wantStatus:   http.StatusFound,
			wantLocation: "/articles/42",
		},
		{
			name: "named capture quote form substituted", url: "/posts/42",
			rules:        []query.Redirect{{Regex: "^/posts/(?<id>.+)$", Replacement: "/articles/\\k'id'", Enabled: 1}},
			wantStatus:   http.StatusFound,
			wantLocation: "/articles/42",
		},
		{
			name: "whole match substituted", url: "/a/old/b",
			rules:        []query.Redirect{{Regex: "old", Replacement: "[\\&]", Enabled: 1}},
			wantStatus:   http.StatusFound,
			wantLocation: "/a/[old]/b",
		},
		{
			name: "partial match substitutes span", url: "/a/old/b",
			rules:        []query.Redirect{{Regex: "old", Replacement: "new", Enabled: 1}},
			wantStatus:   http.StatusFound,
			wantLocation: "/a/new/b",
		},
		{
			name: "self-target rule is skipped", url: "/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/old", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "self-target with only a query added is skipped", url: "/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/old?x=1", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "self-target with only a fragment added is skipped", url: "/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "/old#x", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "absolute self-target with a query is skipped", url: "http://example.com/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "https://example.com/old?x=1", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "self-target rule falls through to a later rule", url: "/old",
			rules: []query.Redirect{
				{Regex: "^/old$", Replacement: "/old", Enabled: 1},
				{Regex: "^/old$", Replacement: "/new", Enabled: 1},
			},
			wantStatus:   http.StatusFound,
			wantLocation: "/new",
		},
		{
			name: "absolute self-target on the same host is skipped", url: "http://example.com/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "https://example.com/old", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "absolute self-target ignores case and port in the host", url: "http://example.com/old",
			rules:      []query.Redirect{{Regex: "^/old$", Replacement: "https://EXAMPLE.com:443/old", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
		{
			name: "absolute target on another host still redirects", url: "http://www.example.com/old",
			rules:        []query.Redirect{{Regex: "^/old$", Replacement: "https://example.com/old", Enabled: 1}},
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/old",
		},
		{
			name: "slug sharing skip prefix redirected", url: "/updates-2024",
			rules:        []query.Redirect{{Regex: "^/updates-2024$", Replacement: "/articles/updates-2024", Enabled: 1}},
			wantStatus:   http.StatusFound,
			wantLocation: "/articles/updates-2024",
		},
		{
			name: "exact skip prefix still skipped", url: "/admin",
			rules:      []query.Redirect{{Regex: "admin", Replacement: "new", Enabled: 1}},
			wantStatus: http.StatusOK,
		},
	}
	for _, prefix := range []string{"/admin", "/assets", "/files", "/static", "/up"} {
		tests = append(tests, redirectTestCase{
			name: "skip prefix " + prefix, url: prefix + "/old",
			rules:      []query.Redirect{{Regex: "old", Replacement: "new", Enabled: 1}},
			wantStatus: http.StatusOK,
		})
	}
	runRedirectTable(t, tests)
}

// TestRedirectMiddlewareProtocolRelativeTarget: a path rule keeps the request
// path's unmatched prefix, so a crafted path starting with "//host" would
// leak into Location as a protocol-relative URL pointing off-site. Such
// targets are treated as non-matching.
func TestRedirectMiddlewareProtocolRelativeTarget(t *testing.T) {
	s, h := newRedirectTestServer(t)
	insertRedirect(t, s, query.Redirect{Regex: "old", Replacement: "new", Enabled: 1})

	req := httptest.NewRequest(http.MethodGet, "/a/old", nil)
	req.URL.Path = "//evil.example/a/old"
	req.RequestURI = "//evil.example/a/old"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Location"); got != "" {
		t.Errorf("location = %q, want empty", got)
	}
}

// TestRedirectMiddlewareCacheInvalidation: rules are cached, and
// InvalidateRedirectCache makes writes visible immediately.
func TestRedirectMiddlewareCacheInvalidation(t *testing.T) {
	s, h := newRedirectTestServer(t)
	rule := insertRedirect(t, s, query.Redirect{Regex: "^/old$", Replacement: "/new", Enabled: 1})

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
		Regex: "^/old$", Replacement: "/newer", MatchOn: "path", Enabled: 1, UpdatedAt: time.Now().Unix(), ID: rule.ID,
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
	if err := s.Q.DeleteRedirect(t.Context(), rule.ID); err != nil {
		t.Fatalf("delete redirect: %v", err)
	}
	s.InvalidateRedirectCache()
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("after delete: status = %d, want 200", rec.Code)
	}
}

// TestRedirectMiddlewareHostPathRules covers the regex host+path extension:
// the pattern runs against host+path (the migrated form of the old match_on=
// 'host' rules), in the same position-ordered list as every other rule.
func TestRedirectMiddlewareHostPathRules(t *testing.T) {
	hostRule := func(regex, replacement string) query.Redirect {
		return query.Redirect{Regex: regex, Replacement: replacement, Enabled: 1, MatchOn: "host_path"}
	}
	tests := []redirectTestCase{
		{
			name: "exact host to external URL",
			rules: []query.Redirect{
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://abc.example.com/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "capture group into absolute target",
			rules: []query.Redirect{
				hostRule(`^(\d+)\.example\.com(?:/.*)?$`, `https://example.com/tags/\1`),
			},
			url:          "http://54321.example.com/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/tags/54321",
		},
		{
			name: "path capture group carried",
			rules: []query.Redirect{
				hostRule(`^blog\.example\.com/blog/(.+)$`, `https://example.com/blog/\1`),
			},
			url:          "http://blog.example.com/blog/hello-world",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/blog/hello-world",
		},
		{
			name:    "relative target completed with site URL",
			siteURL: "https://example.com",
			rules: []query.Redirect{
				hostRule(`^abcd\.example\.com(?:/.*)?$`, `/blog/abcd`),
			},
			url:          "http://abcd.example.com/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/blog/abcd",
		},
		{
			name: "relative target without site URL fails open",
			rules: []query.Redirect{
				hostRule(`^abcd\.example\.com(?:/.*)?$`, `/blog/abcd`),
			},
			url:        "http://abcd.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "request path and query never appended",
			rules: []query.Redirect{
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://abc.example.com/foo/bar?x=1",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "host rule fires on skip-prefix path",
			rules: []query.Redirect{
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://abc.example.com/admin",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "first-created rule wins across match types",
			rules: []query.Redirect{
				{Regex: "^/old$", Replacement: "/path-rule", Enabled: 1},
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://abc.example.com/old",
			wantStatus:   http.StatusFound,
			wantLocation: "/path-rule",
		},
		{
			name: "host with port matches",
			rules: []query.Redirect{
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://abc.example.com:8080/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "host case-insensitive",
			rules: []query.Redirect{
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://ABC.Example.COM/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "apex host does not match subdomain rule",
			rules: []query.Redirect{
				hostRule(`^([^.]+)\.example\.com(?:/.*)?$`, `https://example.com/\1`),
			},
			url:        "http://example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "disabled host rule skipped",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com(?:/.*)?$`, Replacement: "https://google.com", Enabled: 0, MatchOn: "host_path"},
			},
			url:        "http://abc.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name:   "POST not redirected by host rule",
			method: http.MethodPost,
			rules: []query.Redirect{
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:        "http://abc.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "non-matching host falls through to path rules",
			rules: []query.Redirect{
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
				{Regex: "^/old$", Replacement: "/new", Enabled: 1},
			},
			url:          "http://www.example.com/old",
			wantStatus:   http.StatusFound,
			wantLocation: "/new",
		},
		{
			// The self-target skip resolves the request host for its
			// comparison; the later host+path rule must reuse that cached
			// resolution, not an empty subject.
			name: "host rule after a self-target-skipped path rule",
			rules: []query.Redirect{
				{Regex: "^/old$", Replacement: "/old", Enabled: 1},
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://abc.example.com/old",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "permanent host rule is 301",
			rules: []query.Redirect{
				{Regex: `^abc\.example\.com(?:/.*)?$`, Replacement: "https://google.com", Permanent: 1, Enabled: 1, MatchOn: "host_path"},
			},
			url:          "http://abc.example.com/",
			wantStatus:   http.StatusMovedPermanently,
			wantLocation: "https://google.com",
		},
		{
			name: "unanchored regex still matches whole subject only",
			rules: []query.Redirect{
				hostRule(`abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://abc.example.com/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "host suffix beyond a missing-$ regex does not leak into target",
			rules: []query.Redirect{
				hostRule(`^abc\.example\.com(?:/.*)?`, "https://google.com"),
			},
			url:        "http://abc.example.com.evil.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "host prefix beyond a missing-^ regex does not match",
			rules: []query.Redirect{
				hostRule(`abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:        "http://evil-abc.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "trailing-dot FQDN matches",
			rules: []query.Redirect{
				hostRule(`^abc\.example\.com(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://abc.example.com./",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "IPv6 literal with port matches bracketless pattern",
			rules: []query.Redirect{
				hostRule(`^::1(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://[::1]:8080/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
		{
			name: "IPv6 literal without port matches bracketless pattern",
			rules: []query.Redirect{
				hostRule(`^::1(?:/.*)?$`, "https://google.com"),
			},
			url:          "http://[::1]/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com",
		},
	}
	runRedirectTable(t, tests)
}

// TestRedirectMiddlewareSimpleRules covers the no-regex mode: match_from is a
// plain string ("/path" on any host, or "host/path"), matched exactly or as a
// prefix that carries the remainder onto the target.
func TestRedirectMiddlewareSimpleRules(t *testing.T) {
	simple := func(from, to string, prefix bool) query.Redirect {
		rule := query.Redirect{MatchFrom: from, Replacement: to, Enabled: 1, MatchOn: "simple"}
		if prefix {
			rule.MatchPrefix = 1
		}
		return rule
	}
	tests := []redirectTestCase{
		{
			name: "exact path", url: "/old",
			rules:        []query.Redirect{simple("/old", "/new", false)},
			wantStatus:   http.StatusFound,
			wantLocation: "/new",
		},
		{
			name: "exact path tolerates one trailing slash", url: "/old/",
			rules:        []query.Redirect{simple("/old", "/new", false)},
			wantStatus:   http.StatusFound,
			wantLocation: "/new",
		},
		{
			name: "exact path is not a substring match", url: "/older",
			rules:      []query.Redirect{simple("/old", "/new", false)},
			wantStatus: http.StatusOK,
		},
		{
			name: "prefix path carries the remainder", url: "/blog/hello",
			rules:        []query.Redirect{simple("/blog/", "/articles/", true)},
			wantStatus:   http.StatusFound,
			wantLocation: "/articles/hello",
		},
		{
			name: "prefix path on the bare base", url: "/blog/",
			rules:        []query.Redirect{simple("/blog/", "/articles/", true)},
			wantStatus:   http.StatusFound,
			wantLocation: "/articles/",
		},
		{
			name: "prefix without trailing slash respects the segment boundary", url: "/blogroll",
			rules:      []query.Redirect{simple("/blog", "/articles", true)},
			wantStatus: http.StatusOK,
		},
		{
			name: "prefix without trailing slash joins exactly one slash", url: "/blog/x",
			rules:        []query.Redirect{simple("/blog", "/articles/", true)},
			wantStatus:   http.StatusFound,
			wantLocation: "/articles/x",
		},
		{
			name: "prefix without trailing slash matches the base itself", url: "/blog",
			rules:        []query.Redirect{simple("/blog", "/articles", true)},
			wantStatus:   http.StatusFound,
			wantLocation: "/articles",
		},
		{
			name:         "simple host root exact",
			siteURL:      "https://example.com",
			rules:        []query.Redirect{simple("blog.example.com", "/tags/blog", false)},
			url:          "http://blog.example.com/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/tags/blog",
		},
		{
			name:       "simple host exact does not match sub-paths",
			siteURL:    "https://example.com",
			rules:      []query.Redirect{simple("blog.example.com", "/tags/blog", false)},
			url:        "http://blog.example.com/foo",
			wantStatus: http.StatusOK,
		},
		{
			name:         "simple host prefix maps every path",
			siteURL:      "https://example.com",
			rules:        []query.Redirect{simple("blog.example.com/", "/pages/", true)},
			url:          "http://blog.example.com/about",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/pages/about",
		},
		{
			name:         "simple host prefix with absolute target",
			rules:        []query.Redirect{simple("blog.example.com", "https://google.com", true)},
			url:          "http://blog.example.com/foo/bar",
			wantStatus:   http.StatusFound,
			wantLocation: "https://google.com/foo/bar",
		},
		{
			name:         "query string is never carried",
			siteURL:      "https://example.com",
			rules:        []query.Redirect{simple("blog.example.com/", "/pages/", true)},
			url:          "http://blog.example.com/about?x=1",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/pages/about",
		},
		{
			name:         "simple host rule is case-insensitive on the host",
			siteURL:      "https://example.com",
			rules:        []query.Redirect{simple("blog.example.com", "/tags/blog", false)},
			url:          "http://BLOG.Example.COM/",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/tags/blog",
		},
		{
			name:         "simple host rule fires on skip-prefix path",
			siteURL:      "https://example.com",
			rules:        []query.Redirect{simple("blog.example.com/", "/pages/", true)},
			url:          "http://blog.example.com/admin/panel",
			wantStatus:   http.StatusFound,
			wantLocation: "https://example.com/pages/admin/panel",
		},
		{
			name: "simple path rule honors skip prefixes", url: "/admin/panel",
			rules:      []query.Redirect{simple("/admin/", "/x/", true)},
			wantStatus: http.StatusOK,
		},
		{
			name:       "relative target without site URL fails open",
			rules:      []query.Redirect{simple("blog.example.com", "/tags/blog", false)},
			url:        "http://blog.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name: "permanent simple rule is 301", url: "/old",
			rules: []query.Redirect{
				{MatchFrom: "/old", Replacement: "/new", Permanent: 1, Enabled: 1, MatchOn: "simple"},
			},
			wantStatus:   http.StatusMovedPermanently,
			wantLocation: "/new",
		},
		{
			name: "disabled simple rule skipped", url: "/old",
			rules: []query.Redirect{
				{MatchFrom: "/old", Replacement: "/off", Enabled: 0, MatchOn: "simple"},
				{MatchFrom: "/old", Replacement: "/on", Enabled: 1, MatchOn: "simple"},
			},
			wantStatus:   http.StatusFound,
			wantLocation: "/on",
		},
		{
			name:   "POST not redirected by simple rule",
			method: http.MethodPost, url: "/old",
			rules:      []query.Redirect{simple("/old", "/new", false)},
			wantStatus: http.StatusOK,
		},
		{
			name: "exact self-target rule is skipped", url: "/old",
			rules:      []query.Redirect{simple("/old", "/old", false)},
			wantStatus: http.StatusOK,
		},
		{
			name: "absolute self-target on the same host is skipped", url: "http://example.com/old",
			rules:      []query.Redirect{simple("/old", "https://example.com/old", false)},
			wantStatus: http.StatusOK,
		},
		{
			name:       "host rule targeting its own host and path is skipped",
			rules:      []query.Redirect{simple("blog.example.com/", "https://blog.example.com/", true)},
			url:        "http://blog.example.com/about",
			wantStatus: http.StatusOK,
		},
		{
			name:       "host root rule targeting its bare host URL is skipped",
			rules:      []query.Redirect{simple("blog.example.com", "https://blog.example.com", false)},
			url:        "http://blog.example.com/",
			wantStatus: http.StatusOK,
		},
		{
			name:         "host rule targeting another path on its own host still redirects",
			rules:        []query.Redirect{simple("blog.example.com/", "https://blog.example.com/pages/", true)},
			url:          "http://blog.example.com/about",
			wantStatus:   http.StatusFound,
			wantLocation: "https://blog.example.com/pages/about",
		},
		{
			name: "first-created simple rule wins", url: "/old",
			rules: []query.Redirect{
				simple("/old", "/first", false),
				simple("/old", "/second", false),
			},
			wantStatus:   http.StatusFound,
			wantLocation: "/first",
		},
	}
	runRedirectTable(t, tests)
}

// TestRedirectMiddlewareReorder: SetRedirectPosition decides the evaluation
// order across rule kinds — the admin drag-and-drop writes through it.
func TestRedirectMiddlewareReorder(t *testing.T) {
	s, h := newRedirectTestServer(t)
	setSiteURL(t, s, "https://example.com")
	first := insertRedirect(t, s, query.Redirect{Regex: "^/blog/(.+)$", Replacement: "/path-rule/\\1", Enabled: 1})
	second := insertRedirect(t, s, query.Redirect{MatchFrom: "blog.example.com/blog/", Replacement: "/blog/", MatchPrefix: 1, Enabled: 1, MatchOn: "simple"})

	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://blog.example.com/blog/x", nil))
		return rec
	}

	// Creation order: the regex path rule was created first and wins.
	if rec := get(); rec.Header().Get("Location") != "/path-rule/x" {
		t.Fatalf("before reorder: location = %q, want /path-rule/x", rec.Header().Get("Location"))
	}

	// Dragging the simple rule above the regex rule flips the winner.
	for i, id := range []int64{second.ID, first.ID} {
		if err := s.Q.SetRedirectPosition(t.Context(), query.SetRedirectPositionParams{Position: int64(i + 1), ID: id}); err != nil {
			t.Fatalf("set position: %v", err)
		}
	}
	s.InvalidateRedirectCache()
	if rec := get(); rec.Header().Get("Location") != "https://example.com/blog/x" {
		t.Fatalf("after reorder: location = %q, want https://example.com/blog/x", rec.Header().Get("Location"))
	}
}

// TestRedirectMiddlewareBlogSubdomainSet is the motivating example for the
// simple mode: three ordered rules map a whole blog subdomain onto the
// canonical site — root to /tags/blog, /blog/* kept as /blog/*, everything
// else under /pages/*.
func TestRedirectMiddlewareBlogSubdomainSet(t *testing.T) {
	s, h := newRedirectTestServer(t)
	setSiteURL(t, s, "https://versun.me")
	insertRedirect(t, s, query.Redirect{MatchFrom: "blog.versun.me", Replacement: "/tags/blog", Enabled: 1, MatchOn: "simple"})
	insertRedirect(t, s, query.Redirect{MatchFrom: "blog.versun.me/blog/", Replacement: "/blog/", MatchPrefix: 1, Enabled: 1, MatchOn: "simple"})
	insertRedirect(t, s, query.Redirect{MatchFrom: "blog.versun.me/", Replacement: "/pages/", MatchPrefix: 1, Enabled: 1, MatchOn: "simple"})

	cases := []struct {
		url  string
		want string
	}{
		{"http://blog.versun.me/", "https://versun.me/tags/blog"},
		{"http://blog.versun.me/blog/hello-world", "https://versun.me/blog/hello-world"},
		{"http://blog.versun.me/blog/", "https://versun.me/blog/"},
		{"http://blog.versun.me/about", "https://versun.me/pages/about"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.url, nil))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != c.want {
			t.Errorf("%s: status = %d location = %q, want 302 %q", c.url, rec.Code, rec.Header().Get("Location"), c.want)
		}
	}
}

// TestRedirectMiddlewareLegacyHostRow: a match_on='host' row in the pre-0008
// form (restored from an old backup via the transfer import, which bypasses
// migrations) is rewritten at load time like migration 0008 does.
func TestRedirectMiddlewareLegacyHostRow(t *testing.T) {
	s, h := newRedirectTestServer(t)
	setSiteURL(t, s, "https://example.com")
	insertRedirect(t, s, query.Redirect{Regex: `^abc\.example\.com$`, Replacement: "/tags/abc", Enabled: 1, MatchOn: "host"})

	for _, url := range []string{"http://abc.example.com/", "http://abc.example.com/foo/bar"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if got := rec.Header().Get("Location"); got != "https://example.com/tags/abc" {
			t.Errorf("%s: location = %q, want https://example.com/tags/abc", url, got)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://abc.example.com.evil.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("evil host: status = %d, want 200", rec.Code)
	}
}

// TestRedirectMiddlewareHostOnlyPattern: a host_path regex without any "/" can
// only intend the host itself, so it gets the whole-host rewrite at load time
// and fires on every path. A pattern containing "/" is path-aware and used
// as-is, so "^host/?$" pins the root.
func TestRedirectMiddlewareHostOnlyPattern(t *testing.T) {
	s, h := newRedirectTestServer(t)
	setSiteURL(t, s, "https://example.com")
	insertRedirect(t, s, query.Redirect{Regex: `^abc\.example\.com$`, Replacement: "/landing", Enabled: 1, MatchOn: "host_path"})
	insertRedirect(t, s, query.Redirect{Regex: `^root\.example\.com/?$`, Replacement: "/root-only", Enabled: 1, MatchOn: "host_path"})

	for _, url := range []string{"http://abc.example.com/", "http://abc.example.com/foo/bar"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if got := rec.Header().Get("Location"); got != "https://example.com/landing" {
			t.Errorf("%s: location = %q, want https://example.com/landing", url, got)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://root.example.com/", nil))
	if got := rec.Header().Get("Location"); got != "https://example.com/root-only" {
		t.Errorf("root: location = %q, want https://example.com/root-only", got)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://root.example.com/other", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("root-aware rule fired on a sub-path: status = %d, want 200", rec.Code)
	}
}

// TestRedirectMiddlewareBlankPatternRows: a hand-edited row restored via the
// transfer import (which bypasses the admin form validations) can hold a
// match_on='simple' rule without a match_from, or a regex rule without a
// pattern. Both must be skipped — an empty regex compiles and would otherwise
// redirect every request on the site.
func TestRedirectMiddlewareBlankPatternRows(t *testing.T) {
	s, h := newRedirectTestServer(t)
	insertRedirect(t, s, query.Redirect{Replacement: "/catch-all", Enabled: 1, MatchOn: "simple"})
	insertRedirect(t, s, query.Redirect{Replacement: "/catch-all", Enabled: 1, MatchOn: "path"})
	insertRedirect(t, s, query.Redirect{Regex: "^/old$", Replacement: "/new", Enabled: 1})

	get := func(url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		return rec
	}
	if rec := get("/anything"); rec.Code != http.StatusOK {
		t.Errorf("blank rows: status = %d, want 200 (no catch-all redirect)", rec.Code)
	}
	if rec := get("/old"); rec.Header().Get("Location") != "/new" {
		t.Errorf("later rules still apply: location = %q, want /new", rec.Header().Get("Location"))
	}
}
