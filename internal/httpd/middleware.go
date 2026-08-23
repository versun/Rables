package httpd

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
)

// sessionCookieName carries the session token (sessions.token), mirroring the
// Rails signed session_id cookie.
const sessionCookieName = "session_token"

type contextKey string

const userContextKey contextKey = "httpd.user"

// CurrentUser returns the user attached to the request by RequireAuth.
func CurrentUser(r *http.Request) (query.User, bool) {
	u, ok := r.Context().Value(userContextKey).(query.User)
	return u, ok
}

// RequireAuth rejects requests without a valid session cookie, redirecting to
// the login page like Rails' require_authentication.
func (s *Server) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err == nil {
			sess, err := s.Q.GetSessionByToken(r.Context(), cookie.Value)
			if err == nil && !s.sessionExpired(r.Context(), sess) {
				user, err := s.Q.GetUserByID(r.Context(), sess.UserID)
				if err == nil {
					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
					return
				}
			}
		}
		http.Redirect(w, r, "/session/new", http.StatusFound)
	})
}

// sessionExpired reports whether sess is older than sessionTTL. An expired
// session no longer authenticates, and its row is deleted on the way out so
// the table does not keep dead entries around.
func (s *Server) sessionExpired(ctx context.Context, sess query.Session) bool {
	if time.Since(time.Unix(sess.CreatedAt, 0)) <= sessionTTL {
		return false
	}
	if err := s.Q.DeleteSessionByToken(ctx, sess.Token); err != nil {
		s.Log.Error("delete expired session", "error", err)
	}
	return true
}

// originCheck replaces Rails' CSRF protection (spec §1). Non-GET requests are
// allowed when the Origin header matches the Host (scheme ignored) and/or
// Sec-Fetch-Site is same-origin/same-site/none. Requests carrying neither
// header (curl and other non-browser clients) pass; everything else is 403.
func originCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		fetchSite := r.Header.Get("Sec-Fetch-Site")
		if origin == "" && fetchSite == "" {
			next.ServeHTTP(w, r)
			return
		}
		if origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		switch fetchSite {
		case "", "same-origin", "same-site", "none":
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// stripTrailingSlash normalizes a single trailing "/" out of the routing
// path before matching, so every route also answers its trailing-slash form
// without a redirect (/blog/a/ serves the same handler as /blog/a) — the
// Rails router this app mirrors ignores it. /archives/ URLs are excluded:
// the archive tree route is directory-shaped and redirects the bare record
// URL to its trailing-slash form on purpose, so relative asset links inside
// the static site resolve against the record directory (see archives.go).
//
// Like chi's middleware.StripSlashes, only the chi routing path is rewritten;
// r.URL.Path keeps the original value for access logs and handlers.
func stripTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if len(path) > 1 && path[len(path)-1] == '/' && !strings.HasPrefix(path, "/archives/") {
			// chi routes on URL.RawPath when the URL carries escapes and
			// slugParam relies on that to unescape params itself, so strip
			// from the same form chi would route on. A literal trailing "/"
			// is never escaped, so both forms end in "/" together; a Path
			// trailing slash decoded from %2F leaves RawPath without one,
			// and nothing is stripped.
			routePath := r.URL.RawPath
			if routePath == "" {
				routePath = path
			}
			if strings.HasSuffix(routePath, "/") {
				if rctx := chi.RouteContext(r.Context()); rctx != nil {
					rctx.RoutePath = routePath[:len(routePath)-1]
				} else {
					r.URL.Path = path[:len(path)-1]
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// setupCacheTTL mirrors the 5-minute setup_incomplete cache of the Rails app.
const setupCacheTTL = 5 * time.Minute

const setupCacheKey = "httpd.setup_incomplete"

type setupCacheEntry struct {
	incomplete bool
	checkedAt  time.Time
}

// setupAllowedPrefixes stay reachable while setup is incomplete.
var setupAllowedPrefixes = []string{"/setup", "/session", "/up", "/files", "/static", "/assets"}

// setupRedirect sends every request to /setup while no user exists or
// settings.setup_completed is 0, mirroring ApplicationController's
// redirect_to_setup_if_needed. The verdict is cached process-wide for 5
// minutes; call InvalidateSetupCache after completing setup.
func (s *Server) setupRedirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, prefix := range setupAllowedPrefixes {
			if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
				next.ServeHTTP(w, r)
				return
			}
		}
		if s.setupIncomplete(r.Context()) {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// setupIncomplete reports whether setup still needs to run, cached for
// setupCacheTTL. Mirrors Setting.setup_incomplete?: no users, or the settings
// row exists with setup_completed = 0. Like redirectRules, a database error
// fails open and is not cached, so the next request retries.
func (s *Server) setupIncomplete(ctx context.Context) bool {
	if v, ok := s.Ext.Load(setupCacheKey); ok {
		if entry := v.(setupCacheEntry); time.Since(entry.checkedAt) < setupCacheTTL {
			return entry.incomplete
		}
	}
	cache := func(incomplete bool) bool {
		s.Ext.Store(setupCacheKey, setupCacheEntry{incomplete: incomplete, checkedAt: time.Now()})
		return incomplete
	}

	users, err := s.Q.CountUsers(ctx)
	if err != nil {
		s.Log.Error("setup check: count users", "error", err)
		return false // fail open; the error surfaces on real queries
	}
	if users == 0 {
		return cache(true)
	}
	if err := s.Q.EnsureSettings(ctx, query.EnsureSettingsParams{CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}); err != nil {
		s.Log.Error("setup check: ensure settings", "error", err)
		return false
	}
	settings, err := s.Q.GetSettings(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.Log.Error("setup check: get settings", "error", err)
		return false
	}
	return cache(settings.SetupCompleted == 0)
}

// InvalidateSetupCache drops the cached setup verdict so the next request
// re-checks the database.
func (s *Server) InvalidateSetupCache() {
	s.Ext.Delete(setupCacheKey)
}
