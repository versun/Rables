package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
		{http.MethodPost, "/admin/redirects/reorder"},
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

	// New form defaults to enabled and simple mode.
	rec = doRequest(t, h, http.MethodGet, "/admin/redirects/new", nil, session)
	newForm := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(newForm, `action="/admin/redirects"`) {
		t.Fatalf("new form: status = %d", rec.Code)
	}
	if !strings.Contains(newForm, `<option value="simple" selected`) {
		t.Errorf("new form does not default to simple mode")
	}
	if !strings.Contains(newForm, `id="enabled" name="enabled" value="1" checked`) {
		t.Errorf("new form does not default to enabled")
	}

	// Create a regex path rule (enabled checked, permanent unchecked -> hidden 0).
	form := url.Values{
		"kind":        {"regex"},
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
	if redirect.MatchOn != "path" {
		t.Errorf("stored match_on = %q, want path (auto-detected)", redirect.MatchOn)
	}
	if redirect.Position != 1 {
		t.Errorf("first redirect position = %d, want 1", redirect.Position)
	}

	// A regex pattern not starting with "/" is a host+path rule.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects",
		url.Values{"kind": {"regex"}, "regex": {`^blog\.example\.com(?:/.*)?$`}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("create host+path rule: status = %d", rec.Code)
	}
	rows, err = s.Q.ListRedirects(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list redirects: %v rows = %d", err, len(rows))
	}
	var hostRule *query.Redirect
	for i := range rows {
		if rows[i].Regex == `^blog\.example\.com(?:/.*)?$` {
			hostRule = &rows[i]
			break
		}
	}
	if hostRule == nil {
		t.Fatal("host+path redirect not stored")
	}
	if hostRule.MatchOn != "host_path" {
		t.Errorf("host+path pattern stored as %q, want host_path", hostRule.MatchOn)
	}
	if err := s.Q.DeleteRedirect(ctx, hostRule.ID); err != nil {
		t.Fatalf("delete host+path redirect: %v", err)
	}

	// Create a simple rule: the host part of match_from is lowercased.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{
		"kind":        {"simple"},
		"match_from":  {"Blog.Example.COM/Blog/"},
		"match_mode":  {"prefix"},
		"replacement": {"/blog/"},
		"enabled":     {"1"},
	}, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("create simple rule: status = %d", rec.Code)
	}
	rows, err = s.Q.ListRedirects(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list redirects: %v rows = %d", err, len(rows))
	}
	var simpleRule *query.Redirect
	for i := range rows {
		if rows[i].MatchOn == "simple" {
			simpleRule = &rows[i]
			break
		}
	}
	if simpleRule == nil {
		t.Fatal("simple redirect not stored")
	}
	if simpleRule.MatchFrom != "blog.example.com/Blog/" || simpleRule.MatchPrefix != 1 || simpleRule.Regex != "" {
		t.Errorf("stored simple redirect = %+v", simpleRule)
	}

	// Index lists both in evaluation order with row numbers.
	rec = doRequest(t, h, http.MethodGet, "/admin/redirects", nil, session)
	body := rec.Body.String()
	regexAt := strings.Index(body, "^/old$")
	simpleAt := strings.Index(body, "blog.example.com/Blog/")
	if regexAt < 0 || simpleAt < 0 {
		t.Errorf("index does not list the created redirects")
	} else if regexAt > simpleAt {
		t.Errorf("index lists the simple rule before the older regex rule, want evaluation (position) order")
	}
	if !strings.Contains(body, `data-redirect-order-url-value="/admin/redirects/reorder"`) {
		t.Errorf("index is missing the drag-and-drop controller hooks")
	}

	// Validation failures re-render with 422 and the Rails wording.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"kind": {"regex"}, "regex": {"  "}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Regex can&#39;t be blank") {
		t.Errorf("blank regex create: status = %d, want 422 with the blank error", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"kind": {"regex"}, "regex": {"^/x$"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Replacement can&#39;t be blank") {
		t.Errorf("blank replacement create: status = %d, want 422 with the blank error", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"kind": {"regex"}, "regex": {"(["}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "is not a valid regular expression") {
		t.Errorf("invalid regex create: status = %d, want 422 with the regex error", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"kind": {"simple"}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Match from can&#39;t be blank") {
		t.Errorf("blank match_from create: status = %d, want 422 with the blank error", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"kind": {"simple"}, "match_from": {"https://x.com"}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "must be a path") {
		t.Errorf("scheme in match_from: status = %d, want 422 with the shape error", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"kind": {"simple"}, "match_from": {"/old"}, "replacement": {"tags/blog"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "must start with /") {
		t.Errorf("relative-word replacement: status = %d, want 422 with the target error", rec.Code)
	}
	// An empty port leaves a dangling colon that would never match.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects", url.Values{"kind": {"simple"}, "match_from": {"blog.example.com:"}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "must be a path") {
		t.Errorf("empty-port match_from: status = %d, want 422 with the shape error", rec.Code)
	}

	// Edit form of the simple rule opens in simple mode with its values.
	rec = doRequest(t, h, http.MethodGet, "/admin/redirects/"+itoa(simpleRule.ID)+"/edit", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `value="blog.example.com/Blog/"`) {
		t.Fatalf("edit form: status = %d", rec.Code)
	}

	// Update the regex rule, flipping permanent/enabled.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects/"+itoa(redirect.ID),
		url.Values{"kind": {"regex"}, "regex": {"^/older$"}, "replacement": {"/newer"}, "permanent": {"1"}}, session)
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
	if updated.MatchOn != "path" {
		t.Errorf("update stored match_on %q, want path", updated.MatchOn)
	}

	// Switching a rule to simple mode clears the regex.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects/"+itoa(redirect.ID),
		url.Values{"kind": {"simple"}, "match_from": {"/oldest"}, "replacement": {"/newest"}}, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("update to simple: status = %d", rec.Code)
	}
	updated, err = s.Q.GetRedirectByID(ctx, redirect.ID)
	if err != nil {
		t.Fatalf("get redirect: %v", err)
	}
	if updated.MatchOn != "simple" || updated.MatchFrom != "/oldest" || updated.Regex != "" {
		t.Errorf("updated-to-simple redirect = %+v", updated)
	}

	// Update validation failure re-renders the edit page.
	rec = doRequest(t, h, http.MethodPost, "/admin/redirects/"+itoa(redirect.ID),
		url.Values{"kind": {"regex"}, "regex": {"(["}, "replacement": {"/x"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "is not a valid regular expression") {
		t.Errorf("invalid update: status = %d, want 422 with the regex error", rec.Code)
	}

	// Unknown ids are 404 like Redirect.find.
	for path, method := range map[string]string{
		"/admin/redirects/999/edit":    http.MethodGet,
		"/admin/redirects/999":         http.MethodPost,
		"/admin/redirects/999/destroy": http.MethodPost,
	} {
		if rec := doRequest(t, h, method, path, url.Values{"kind": {"regex"}, "regex": {"x"}, "replacement": {"y"}}, session); rec.Code != http.StatusNotFound {
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

// TestAdminRedirectsReorder: the drag-and-drop endpoint renumbers positions to
// the posted id order and invalidates the middleware rule cache.
func TestAdminRedirectsReorder(t *testing.T) {
	s, h := newRedirectsAdminTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	var ids []int64
	for _, regex := range []string{"^/a$", "^/b$", "^/c$"} {
		rec := doRequest(t, h, http.MethodPost, "/admin/redirects",
			url.Values{"kind": {"regex"}, "regex": {regex}, "replacement": {"/new"}}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("create %s: status = %d", regex, rec.Code)
		}
	}
	rows, err := s.Q.ListRedirects(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatalf("list redirects: %v rows = %d", err, len(rows))
	}
	for _, row := range rows {
		ids = append(ids, row.ID)
	}

	// Prime the middleware cache, then reorder [c, a, b].
	_ = s.redirectRules(ctx)
	if _, ok := s.Ext.Load(redirectCacheKey); !ok {
		t.Fatal("redirect cache not primed")
	}
	rec := postRedirectReorder(t, h, session, `{"ids":[`+itoa(ids[2])+`,`+itoa(ids[0])+`,`+itoa(ids[1])+`]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("reorder: status = %d body = %q", rec.Code, rec.Body.String())
	}
	if _, ok := s.Ext.Load(redirectCacheKey); ok {
		t.Error("reorder did not invalidate the redirect cache")
	}

	rows, err = s.Q.ListRedirects(ctx)
	if err != nil {
		t.Fatalf("list redirects: %v", err)
	}
	want := []struct {
		id       int64
		position int64
	}{{ids[2], 1}, {ids[0], 2}, {ids[1], 3}}
	for i, w := range want {
		if rows[i].ID != w.id || rows[i].Position != w.position {
			t.Errorf("row %d = id %d position %d, want id %d position %d",
				i, rows[i].ID, rows[i].Position, w.id, w.position)
		}
	}

	// A malformed JSON body is a 400.
	if rec := postRedirectReorder(t, h, session, `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed reorder: status = %d, want 400", rec.Code)
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
		url.Values{"kind": {"regex"}, "regex": {"^/a$"}, "replacement": {"/b"}, "enabled": {"1"}}, session)
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

// postRedirectReorder posts a JSON body to the reorder endpoint and returns
// the recorded response.
func postRedirectReorder(t *testing.T, h http.Handler, session *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/redirects/reorder", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRedirectFormHelpers covers the pure helpers behind the form: match_from
// host normalization, regex subject auto-detection, and simple-target shape.
func TestRedirectFormHelpers(t *testing.T) {
	fromCases := map[string]string{
		"Blog.Example.COM/Blog/":  "blog.example.com/Blog/",
		"blog.example.com:8080/x": "blog.example.com/x",
		"blog.example.com.":       "blog.example.com",
		"[::1]/v6":                "::1/v6",
		"[::1]:8080":              "::1",
		"  /Path/  ":              "/Path/",
		"blog.example.com":        "blog.example.com",
	}
	for in, want := range fromCases {
		if got := normalizeMatchFrom(in); got != want {
			t.Errorf("normalizeMatchFrom(%q) = %q, want %q", in, got, want)
		}
	}

	for _, p := range []string{
		"/old", "^/old$", "(?i)/old", "^(?i)/old$", "(?i)^/old", "(?s-i)/x",
		// Group-wrapped path patterns: the opener is unwrapped before the
		// leading "/" decides.
		"(?:/a|/b)", "(/x)", "(?P<id>/posts/\\d+)", "(?<id>/x)", "(?i:/old)",
	} {
		if got := detectRedirectMatchOn(p); got != "path" {
			t.Errorf("detectRedirectMatchOn(%q) = %q, want path", p, got)
		}
	}
	for _, p := range []string{`blog.example.com`, `^blog\.example\.com$`, `(?i)blog\.example\.com`, `^(?:a|b)\.example\.com`, ""} {
		if got := detectRedirectMatchOn(p); got != "host_path" {
			t.Errorf("detectRedirectMatchOn(%q) = %q, want host_path", p, got)
		}
	}

	for _, target := range []string{"/x", "/", "/a/b", "https://a.com/x", "http://a.com"} {
		if !validSimpleRedirectTarget(target) {
			t.Errorf("validSimpleRedirectTarget(%q) = false, want true", target)
		}
	}
	for _, target := range []string{"//evil.com", "tags/blog", "javascript:alert(1)", "ftp://a.com"} {
		if validSimpleRedirectTarget(target) {
			t.Errorf("validSimpleRedirectTarget(%q) = true, want false", target)
		}
	}
}

// TestAdminRedirectsReorderEdgeCases: empty and unknown ids are accepted
// without disturbing the existing order.
func TestAdminRedirectsReorderEdgeCases(t *testing.T) {
	s, h := newRedirectsAdminTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	for _, regex := range []string{"^/a$", "^/b$"} {
		doRequest(t, h, http.MethodPost, "/admin/redirects",
			url.Values{"kind": {"regex"}, "regex": {regex}, "replacement": {"/new"}}, session)
	}
	before, err := s.Q.ListRedirects(ctx)
	if err != nil || len(before) != 2 {
		t.Fatalf("list redirects: %v rows = %d", err, len(before))
	}

	if rec := postRedirectReorder(t, h, session, `{"ids":[]}`); rec.Code != http.StatusOK {
		t.Errorf("empty ids: status = %d, want 200", rec.Code)
	}
	if rec := postRedirectReorder(t, h, session, `{"ids":[9999]}`); rec.Code != http.StatusOK {
		t.Errorf("unknown id: status = %d, want 200", rec.Code)
	}
	after, err := s.Q.ListRedirects(ctx)
	if err != nil {
		t.Fatalf("list redirects: %v", err)
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].Position != before[i].Position {
			t.Errorf("row %d changed by unknown-id reorder: %+v -> %+v", i, before[i], after[i])
		}
	}
}

// TestAdminRedirectsUpdateKeepsStoredMatchOn: re-saving a regex rule with its
// pattern untouched keeps the stored match subject, so a legacy path rule
// like "posts/(\d+)" (no leading "/") is not silently reclassified to
// host+path — whose whole-subject anchoring could never match. Editing the
// pattern re-derives the subject.
func TestAdminRedirectsUpdateKeepsStoredMatchOn(t *testing.T) {
	s, h := newRedirectsAdminTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	// A legacy match_on='path' row whose pattern has no leading "/".
	legacy := insertRedirect(t, s, query.Redirect{Regex: `posts/(\d+)`, Replacement: `/articles/\1`, Enabled: 1, MatchOn: "path"})

	post := func(regex string) *httptest.ResponseRecorder {
		return doRequest(t, h, http.MethodPost, "/admin/redirects/"+itoa(legacy.ID),
			url.Values{"kind": {"regex"}, "regex": {regex}, "replacement": {`/articles/\1`}}, session)
	}

	if rec := post(`posts/(\d+)`); rec.Code != http.StatusFound {
		t.Fatalf("update unchanged pattern: status = %d", rec.Code)
	}
	r, err := s.Q.GetRedirectByID(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("get redirect: %v", err)
	}
	if r.MatchOn != "path" {
		t.Errorf("unchanged pattern: match_on = %q, want path preserved", r.MatchOn)
	}

	if rec := post(`posts/(\w+)`); rec.Code != http.StatusFound {
		t.Fatalf("update changed pattern: status = %d", rec.Code)
	}
	r, err = s.Q.GetRedirectByID(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("get redirect: %v", err)
	}
	if r.MatchOn != "host_path" {
		t.Errorf("changed pattern: match_on = %q, want host_path re-derived", r.MatchOn)
	}
}
