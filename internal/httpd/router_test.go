package httpd

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/templates"
)

// newTestServer builds a Server backed by a real SQLite DB in a temp dir.
func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	return newTestServerWithConfig(t, config.Config{Addr: ":8080", HMACSecret: "x"})
}

// newTestServerWithConfig is newTestServer with a caller-controlled Config,
// so tests exercise the real wiring (e.g. NewRouter publishing SecureCookies
// to the flash cookie helpers) instead of poking package state by hand.
func newTestServerWithConfig(t *testing.T, cfg config.Config) (*Server, http.Handler) {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	renderer, err := templates.New()
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := NewServer(database, cfg, logger, renderer)
	return s, NewRouter(s)
}

// doRequest runs one request through the handler.
func doRequest(t *testing.T, h http.Handler, method, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// findCookie extracts a Set-Cookie from the response.
func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// locationPath parses a redirect's Location header and returns its path,
// dropping the cache-busting "_" query parameter redirectWithFlash appends
// to public targets.
func locationPath(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location %q: %v", rec.Header().Get("Location"), err)
	}
	return loc.Path
}

var setupForm = url.Values{
	"user_name":             {"Admin"},
	"password":              {"secret-pw"},
	"password_confirmation": {"secret-pw"},
	"title":                 {"My Blog"},
	"description":           {"desc"},
	"author":                {"author"},
	"url":                   {"https://blog.example.com"},
	"time_zone":             {"UTC"},
}

// completeSetup runs the setup flow and returns the recorder for assertions.
func completeSetup(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := doRequest(t, h, http.MethodPost, "/setup", setupForm)
	if rec.Code != http.StatusFound {
		t.Fatalf("setup status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/session/new" {
		t.Fatalf("setup redirect = %q, want /session/new", loc)
	}
	return rec
}

// login posts credentials and returns the session cookie.
func login(t *testing.T, h http.Handler, userName, password string) *http.Cookie {
	t.Helper()
	form := url.Values{"user_name": {userName}, "password": {password}}
	rec := doRequest(t, h, http.MethodPost, "/session", form)
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Fatalf("login redirect = %q, want /admin/", loc)
	}
	cookie := findCookie(rec, sessionCookieName)
	if cookie == nil || cookie.Value == "" {
		t.Fatal("login did not set a session cookie")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	return cookie
}

// TestAccessLogPanics: with accessLog outside middleware.Recoverer, a
// panicking handler still produces an access log line recording the 500
// Recoverer wrote.
func TestAccessLogPanics(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	panicHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	h := accessLog(logger)(middleware.Recoverer(panicHandler))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	out := buf.String()
	if !strings.Contains(out, `"path":"/panic"`) || !strings.Contains(out, `"status":500`) {
		t.Errorf("access log missing 500 line for the panicking request: %s", out)
	}
}

// TestStatusRecorderUnwrap: the access-log wrapper must implement Unwrap so
// http.NewResponseController (e.g. the download handler's write-deadline
// relaxation) can reach the underlying ResponseWriter through the chain.
func TestStatusRecorderUnwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}
	if got := sr.Unwrap(); got != inner {
		t.Errorf("Unwrap = %v, want the wrapped recorder", got)
	}
	rc := http.NewResponseController(sr)
	// Flush walks the Unwrap chain to the recorder's Flusher; without Unwrap
	// it would report "feature not supported".
	if err := rc.Flush(); err != nil {
		t.Errorf("Flush through the wrapper: %v", err)
	}
	if !inner.Flushed {
		t.Error("Flush did not reach the wrapped recorder")
	}
}

func TestRouter(t *testing.T) {
	t.Run("fresh instance redirects everything to setup", func(t *testing.T) {
		_, h := newTestServer(t)
		rec := doRequest(t, h, http.MethodGet, "/", nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/setup" {
			t.Errorf("GET /: status = %d location = %q, want 302 /setup", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("health check", func(t *testing.T) {
		_, h := newTestServer(t)
		rec := doRequest(t, h, http.MethodGet, "/up", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Errorf("GET /up: status = %d body = %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("after setup", func(t *testing.T) {
		_, h := newTestServer(t)
		completeSetup(t, h)

		tests := []struct {
			name       string
			method     string
			path       string
			wantStatus int
		}{
			{name: "unknown path is 404", method: http.MethodGet, path: "/nope", wantStatus: http.StatusNotFound},
			{name: "wrong method is 405", method: http.MethodPost, path: "/up", wantStatus: http.StatusMethodNotAllowed},
			{name: "protected page redirects to login", method: http.MethodGet, path: "/users/1/edit", wantStatus: http.StatusFound},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec := doRequest(t, h, tt.method, tt.path, nil)
				if rec.Code != tt.wantStatus {
					t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
				}
			})
		}
	})
}
