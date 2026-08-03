package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/templates"
)

// newRedirectsAdminTestServer builds a Server with the admin redirect routes
// on a test-local chi router.
func newRedirectsAdminTestServer(t *testing.T) (*Server, http.Handler) {
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
	s := NewServer(database, config.Config{Addr: ":8080", DataDir: t.TempDir(), HMACSecret: "x"}, logger, renderer)
	r := chi.NewRouter()
	RegisterRedirectsRoutes(r, s)
	return s, r
}

// redirectsSessionCookie inserts a user plus session row and returns the
// cookie that satisfies RequireAuth.
func redirectsSessionCookie(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	now := time.Now().Unix()
	user, err := s.Q.CreateUser(t.Context(), query.CreateUserParams{
		UserName: "admin", PasswordDigest: "x", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.Q.CreateSession(t.Context(), query.CreateSessionParams{
		Token: "redirects-test-token", UserID: user.ID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: "redirects-test-token"}
}

// TestAdminRedirectsAuth: every redirect route sits behind RequireAuth.
func TestAdminRedirectsAuth(t *testing.T) {
	_, h := newRedirectsAdminTestServer(t)
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/redirects"},
		{http.MethodGet, "/admin/redirects/new"},
		{http.MethodPost, "/admin/redirects"},
		{http.MethodGet, "/admin/redirects/1/edit"},
		{http.MethodPost, "/admin/redirects/1"},
		{http.MethodPost, "/admin/redirects/1/destroy"},
	}
	for _, tt := range tests {
		rec := doRequest(t, h, tt.method, tt.path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("%s %s unauthenticated: status = %d location = %q, want 302 /session/new",
				tt.method, tt.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestAdminRedirectsCRUD walks the whole Admin::RedirectsController flow.
func TestAdminRedirectsCRUD(t *testing.T) {
	s, h := newRedirectsAdminTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	// Empty index.
	rec := doRequest(t, h, http.MethodGet, "/admin/redirects", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No redirects found.") {
		t.Fatalf("empty index: status = %d", rec.Code)
	}

	// New form defaults to enabled.
	rec = doRequest(t, h, http.MethodGet, "/admin/redirects/new", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="/admin/redirects"`) {
		t.Fatalf("new form: status = %d", rec.Code)
	}

	// Create (enabled checked, permanent unchecked -> hidden 0).
	form := url.Values{
		"regex":       {"^/old$"},
		"replacement": {"/new"},
		"enabled":     {"1"},
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", form, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/redirects" {
		t.Fatalf("create: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	rows, err := s.Q.ListRedirects(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list redirects: %v rows = %d", err, len(rows))
	}
	redirect := rows[0]
	if redirect.Regex != "^/old$" || redirect.Replacement != "/new" || redirect.Enabled != 1 || redirect.Permanent != 0 {
		t.Errorf("stored redirect = %+v", redirect)
	}

	// Index lists it.
	rec = doRequest(t, h, http.MethodGet, "/admin/redirects", nil, session)
	if body := rec.Body.String(); !strings.Contains(body, "^/old$") || !strings.Contains(body, "302 Temporary") {
		t.Errorf("index does not list the created redirect")
	}

	// Validation failures re-render with 422 and the Rails wording.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"regex": {"  "}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Regex can&#39;t be blank") {
		t.Errorf("blank regex create: status = %d, want 422 with the blank error", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"regex": {"^/x$"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Replacement can&#39;t be blank") {
		t.Errorf("blank replacement create: status = %d, want 422 with the blank error", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"regex": {"(["}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "is not a valid regular expression") {
		t.Errorf("invalid regex create: status = %d, want 422 with the regex error", rec.Code)
	}

	// Edit form.
	rec = doRequest(t, h, http.MethodGet, "/admin/redirects/"+itoa(redirect.ID)+"/edit", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `value="^/old$"`) {
		t.Fatalf("edit form: status = %d", rec.Code)
	}

	// Update flips permanent/enabled.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects/"+itoa(redirect.ID),
		url.Values{"regex": {"^/older$"}, "replacement": {"/newer"}, "permanent": {"1"}}, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/redirects" {
		t.Fatalf("update: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	updated, err := s.Q.GetRedirectByID(ctx, redirect.ID)
	if err != nil {
		t.Fatalf("get redirect: %v", err)
	}
	if updated.Regex != "^/older$" || updated.Permanent != 1 || updated.Enabled != 0 {
		t.Errorf("updated redirect = %+v, want regex ^/older$ permanent 1 enabled 0", updated)
	}

	// Update validation failure re-renders the edit page.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects/"+itoa(redirect.ID),
		url.Values{"regex": {"(["}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "is not a valid regular expression") {
		t.Errorf("invalid update: status = %d, want 422 with the regex error", rec.Code)
	}

	// Unknown ids are 404 like Redirect.find.
	for path, method := range map[string]string{
		"/admin/redirects/999/edit":    http.MethodGet,
		"/admin/redirects/999":         http.MethodPost,
		"/admin/redirects/999/destroy": http.MethodPost,
	} {
		if rec := doRequest(t, h, method, path, url.Values{"regex": {"x"}, "replacement": {"y"}}, session); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", method, path, rec.Code)
		}
	}

	// Destroy redirects with 303 (Rails status: :see_other) and the row goes.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects/"+itoa(redirect.ID)+"/destroy", nil, session)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/redirects" {
		t.Fatalf("destroy: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := s.Q.GetRedirectByID(ctx, redirect.ID); err == nil {
		t.Error("redirect still present after destroy")
	}
}

// TestAdminRedirectsInvalidateCache: admin writes invalidate the middleware
// rule cache, like the Rails model's after_save/after_destroy sweep.
func TestAdminRedirectsInvalidateCache(t *testing.T) {
	s, h := newRedirectsAdminTestServer(t)
	session := redirectsSessionCookie(t, s)

	// Prime the middleware cache with an empty rule list.
	_ = s.redirectRules(t.Context())
	if _, ok := s.Ext.Load(redirectCacheKey); !ok {
		t.Fatal("redirect cache not primed")
	}

	rec := doRequest(t, h, http.MethodPost, "/admin/redirects",
		url.Values{"regex": {"^/a$"}, "replacement": {"/b"}, "enabled": {"1"}}, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("create: status = %d", rec.Code)
	}
	if _, ok := s.Ext.Load(redirectCacheKey); ok {
		t.Error("create did not invalidate the redirect cache")
	}
}

// itoa formats an id for URL paths.
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
