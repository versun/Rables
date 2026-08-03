package httpd

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

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
			if err == nil {
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
			if strings.HasPrefix(r.URL.Path, prefix) {
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
// row exists with setup_completed = 0.
func (s *Server) setupIncomplete(ctx context.Context) (incomplete bool) {
	if v, ok := s.Ext.Load(setupCacheKey); ok {
		if entry := v.(setupCacheEntry); time.Since(entry.checkedAt) < setupCacheTTL {
			return entry.incomplete
		}
	}
	defer func() {
		s.Ext.Store(setupCacheKey, setupCacheEntry{incomplete: incomplete, checkedAt: time.Now()})
	}()

	users, err := s.Q.CountUsers(ctx)
	if err != nil {
		s.Log.Error("setup check: count users", "error", err)
		return false // fail open; the error surfaces on real queries
	}
	if users == 0 {
		return true
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
	return settings.SetupCompleted == 0
}

// InvalidateSetupCache drops the cached setup verdict so the next request
// re-checks the database.
func (s *Server) InvalidateSetupCache() {
	s.Ext.Delete(setupCacheKey)
}
