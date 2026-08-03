package httpd

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	tagsvc "rables/internal/service/tags"
	"rables/internal/templates"
)

// newTagsTestServer builds a Server backed by a real SQLite DB and mounts the
// tag routes on a test-local chi router (not the integrator's NewRouter).
func newTagsTestServer(t *testing.T) (*Server, http.Handler) {
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
	s := NewServer(database, config.Config{Addr: ":8080", HMACSecret: "x"}, logger, renderer)
	r := chi.NewRouter()
	RegisterTagsRoutes(r, s)
	return s, r
}

// tagsSessionCookie inserts a user plus session row and returns the cookie
// that satisfies RequireAuth.
func tagsSessionCookie(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	now := time.Now().Unix()
	user, err := s.Q.CreateUser(t.Context(), query.CreateUserParams{
		UserName: "admin", PasswordDigest: "x", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.Q.CreateSession(t.Context(), query.CreateSessionParams{
		Token: "tags-test-token", UserID: user.ID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: "tags-test-token"}
}

// createTagViaForm posts the create form and returns the stored tag.
func createTagViaForm(t *testing.T, h http.Handler, s *Server, session *http.Cookie, name string) query.Tag {
	t.Helper()
	rec := doRequest(t, h, http.MethodPost, "/admin/tags", url.Values{"name": {name}}, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/tags" {
		t.Fatalf("create %q: status = %d location = %q", name, rec.Code, rec.Header().Get("Location"))
	}
	tag, err := s.Q.GetTagByLowerName(t.Context(), name)
	if err != nil {
		t.Fatalf("created tag %q not found: %v", name, err)
	}
	return tag
}

// TestAdminTagsAuth: every tag route sits behind RequireAuth.
func TestAdminTagsAuth(t *testing.T) {
	_, h := newTagsTestServer(t)
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/tags"},
		{http.MethodGet, "/admin/tags/new"},
		{http.MethodPost, "/admin/tags"},
		{http.MethodGet, "/admin/tags/1/edit"},
		{http.MethodPost, "/admin/tags/1"},
		{http.MethodPost, "/admin/tags/1/destroy"},
		{http.MethodPost, "/admin/tags/batch_destroy"},
	}
	for _, tt := range tests {
		rec := doRequest(t, h, tt.method, tt.path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("%s %s unauthenticated: status = %d location = %q, want 302 /session/new",
				tt.method, tt.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestAdminTagsCRUD walks the whole Admin::TagsController flow.
func TestAdminTagsCRUD(t *testing.T) {
	s, h := newTagsTestServer(t)
	session := tagsSessionCookie(t, s)

	// Empty index.
	rec := doRequest(t, h, http.MethodGet, "/admin/tags", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No tags found.") {
		t.Fatalf("empty index: status = %d", rec.Code)
	}

	// New form.
	rec = doRequest(t, h, http.MethodGet, "/admin/tags/new", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="/admin/tags"`) {
		t.Fatalf("new form: status = %d", rec.Code)
	}

	// Create.
	tag := createTagViaForm(t, h, s, session, "Go")
	if tag.Slug != "Go" {
		t.Errorf("slug = %q, want Go", tag.Slug)
	}

	// Validation failures re-render with 422 and the Rails wording (the
	// apostrophe shows up HTML-escaped, as html/template does).
	rec = doRequest(t, h, http.MethodPost, "/admin/tags", url.Values{"name": {"  "}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Name can&#39;t be blank") {
		t.Errorf("blank create: status = %d, want 422 with the blank error", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/tags", url.Values{"name": {"go"}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Name has already been taken") {
		t.Errorf("duplicate create: status = %d, want 422 with the taken error", rec.Code)
	}

	// Index lists the tag with its article count.
	rec = doRequest(t, h, http.MethodGet, "/admin/tags", nil, session)
	if body := rec.Body.String(); !strings.Contains(body, `value="`+fmt.Sprint(tag.ID)+`"`) || !strings.Contains(body, ">Go</a>") {
		t.Errorf("index does not list the created tag")
	}

	// Edit form.
	rec = doRequest(t, h, http.MethodGet, fmt.Sprintf("/admin/tags/%d/edit", tag.ID), nil, session)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), fmt.Sprintf(`action="/admin/tags/%d"`, tag.ID)) ||
		!strings.Contains(rec.Body.String(), `value="Go"`) {
		t.Fatalf("edit form: status = %d", rec.Code)
	}

	// Update; the flash notice rides the redirect onto the index.
	rec = doRequest(t, h, http.MethodPost, fmt.Sprintf("/admin/tags/%d", tag.ID), url.Values{"name": {"Golang"}}, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/tags" {
		t.Fatalf("update: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/tags", nil, session, findCookie(rec, flashCookieName))
	if !strings.Contains(rec.Body.String(), "Tag was successfully updated.") {
		t.Error("index does not show the update notice")
	}
	renamed, err := s.Q.GetTagByID(t.Context(), tag.ID)
	if err != nil {
		t.Fatalf("get tag: %v", err)
	}
	if renamed.Name != "Golang" || renamed.Slug != "Go" {
		t.Errorf("renamed tag = (%q, %q), want (Golang, Go)", renamed.Name, renamed.Slug)
	}

	// Update validation failure re-renders the edit page.
	rec = doRequest(t, h, http.MethodPost, fmt.Sprintf("/admin/tags/%d", tag.ID), url.Values{"name": {""}}, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Name can&#39;t be blank") {
		t.Errorf("blank update: status = %d, want 422 with the blank error", rec.Code)
	}

	// Unknown ids are 404 like Tag.find.
	for path, method := range map[string]string{
		"/admin/tags/999/edit":    http.MethodGet,
		"/admin/tags/999":         http.MethodPost,
		"/admin/tags/999/destroy": http.MethodPost,
	} {
		if rec := doRequest(t, h, method, path, url.Values{"name": {"x"}}, session); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", method, path, rec.Code)
		}
	}

	// Destroy redirects with 303 (Rails status: :see_other) and the row goes.
	rec = doRequest(t, h, http.MethodPost, fmt.Sprintf("/admin/tags/%d/destroy", tag.ID), nil, session)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/tags" {
		t.Fatalf("destroy: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := s.Q.GetTagByID(t.Context(), tag.ID); err == nil {
		t.Error("tag still present after destroy")
	}
}

// TestAdminTagsBatchDestroy mirrors process_batch_action(action: :destroy):
// checked ids are destroyed, unknown ids are skipped, and the notice carries
// the count.
func TestAdminTagsBatchDestroy(t *testing.T) {
	s, h := newTagsTestServer(t)
	session := tagsSessionCookie(t, s)
	ctx := t.Context()

	var ids []string
	for _, name := range []string{"alpha", "beta", "gamma"} {
		tag, err := tagsvc.Create(ctx, s.Q, name)
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		ids = append(ids, fmt.Sprint(tag.ID))
	}

	form := url.Values{}
	form["ids[]"] = []string{ids[0], ids[1], "999"}
	rec := doRequest(t, h, http.MethodPost, "/admin/tags/batch_destroy", form, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/tags" {
		t.Fatalf("batch_destroy: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/tags", nil, session, findCookie(rec, flashCookieName))
	if !strings.Contains(rec.Body.String(), "Successfully deleted 2 tag(s).") {
		t.Error("index does not show the batch notice")
	}

	rows, err := s.Q.ListTagsWithArticleCount(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "gamma" {
		t.Errorf("remaining tags = %v, want only gamma", rows)
	}

	// An empty selection is a no-op redirect with a zero count.
	rec = doRequest(t, h, http.MethodPost, "/admin/tags/batch_destroy", nil, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("empty batch_destroy: status = %d, want 302", rec.Code)
	}
}

// TestAdminTagsIndexArticleCount mirrors the left_joins(:articles) count.
func TestAdminTagsIndexArticleCount(t *testing.T) {
	s, h := newTagsTestServer(t)
	session := tagsSessionCookie(t, s)
	ctx := t.Context()

	used := createTagViaForm(t, h, s, session, "used")
	createTagViaForm(t, h, s, session, "unused")
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO articles (title, slug, created_at, updated_at) VALUES ('a', 'a', ?, ?)`, now, now)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	articleID, _ := res.LastInsertId()
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO article_tags (article_id, tag_id, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		articleID, used.ID, now, now); err != nil {
		t.Fatalf("link tag: %v", err)
	}

	rows, err := s.Q.ListTagsWithArticleCount(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	// Alphabetical: unused < used.
	if len(rows) != 2 || rows[0].Name != "unused" || rows[1].Name != "used" {
		t.Fatalf("rows = %+v, want alphabetical unused, used", rows)
	}
	if rows[0].ArticlesTotal != 0 || rows[1].ArticlesTotal != 1 {
		t.Errorf("article counts = %d, %d; want 0, 1", rows[0].ArticlesTotal, rows[1].ArticlesTotal)
	}
}
