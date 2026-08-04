package httpd

import (
	"database/sql"
	"fmt"
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
	"rables/internal/domain"
	"rables/internal/templates"
)

// newPagesTestServer builds a Server backed by a real SQLite DB and mounts
// the admin page routes on a test-local chi router (not NewRouter).
func newPagesTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterPageAdminRoutes(r, s)
	return s, r
}

// pagesSessionCookie inserts a user plus session row and returns the cookie
// that satisfies RequireAuth.
func pagesSessionCookie(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	now := time.Now().Unix()
	user, err := s.Q.CreateUser(t.Context(), query.CreateUserParams{
		UserName: "admin", PasswordDigest: "x", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.Q.CreateSession(t.Context(), query.CreateSessionParams{
		Token: "pages-test-token", UserID: user.ID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: "pages-test-token"}
}

// validPageForm is a passing rich_text draft submission; tests override keys.
func validPageForm() url.Values {
	return url.Values{
		"title":        {"About"},
		"slug":         {"about"},
		"content_type": {"rich_text"},
		"content":      {"<p>Hello</p>"},
		"html_content": {""},
		"page_order":   {"0"},
		"redirect_url": {""},
		"status":       {"draft"},
		"comment":      {"1"},
		"scheduled_at": {""},
	}
}

// createPageViaForm posts the create form and returns the stored page.
func createPageViaForm(t *testing.T, h http.Handler, s *Server, session *http.Cookie, form url.Values) query.Page {
	t.Helper()
	rec := doRequest(t, h, http.MethodPost, "/admin/pages", form, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/pages" {
		t.Fatalf("create %q: status = %d location = %q body = %s",
			form.Get("slug"), rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	page, err := s.Q.GetAdminPageBySlug(t.Context(), sql.NullString{String: form.Get("slug"), Valid: true})
	if err != nil {
		t.Fatalf("created page %q not found: %v", form.Get("slug"), err)
	}
	return page
}

// TestAdminPagesAuth: every admin page route sits behind RequireAuth.
func TestAdminPagesAuth(t *testing.T) {
	_, h := newPagesTestServer(t)
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/pages"},
		{http.MethodGet, "/admin/pages/new"},
		{http.MethodPost, "/admin/pages"},
		{http.MethodGet, "/admin/pages/about/edit"},
		{http.MethodPost, "/admin/pages/about"},
		{http.MethodPost, "/admin/pages/about/destroy"},
		{http.MethodPost, "/admin/pages/batch_destroy"},
		{http.MethodPost, "/admin/pages/batch_publish"},
		{http.MethodPost, "/admin/pages/batch_unpublish"},
	}
	for _, tt := range tests {
		rec := doRequest(t, h, tt.method, tt.path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("%s %s unauthenticated: status = %d location = %q, want 302 /session/new",
				tt.method, tt.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestAdminPagesCRUD walks the whole Admin::PagesController flow.
func TestAdminPagesCRUD(t *testing.T) {
	s, h := newPagesTestServer(t)
	session := pagesSessionCookie(t, s)
	ctx := t.Context()

	// Empty index.
	rec := doRequest(t, h, http.MethodGet, "/admin/pages", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No pages found.") {
		t.Fatalf("empty index: status = %d", rec.Code)
	}

	// New form defaults to an enabled comment checkbox (Page.new(comment: true)).
	rec = doRequest(t, h, http.MethodGet, "/admin/pages/new", nil, session)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `action="/admin/pages"`) ||
		!strings.Contains(rec.Body.String(), `name="comment" value="1" checked`) {
		t.Fatalf("new form: status = %d", rec.Code)
	}

	// Create a rich_text page; content is sanitized and images lazy-loaded at
	// write time (spec 4.4).
	form := validPageForm()
	form.Set("content", `<p>Hi</p><script>alert(1)</script><p><img src="https://x.test/a.png"></p>`)
	form.Set("redirect_url", "https://example.com/elsewhere")
	form.Set("page_order", "7")
	page := createPageViaForm(t, h, s, session, form)
	stored := page.ContentHtml.String
	if strings.Contains(stored, "script") {
		t.Errorf("stored content still has script: %s", stored)
	}
	if !strings.Contains(stored, `img src="https://x.test/a.png" loading="lazy"`) {
		t.Errorf("stored content missing lazy loading: %s", stored)
	}
	if page.RedirectUrl.String != "https://example.com/elsewhere" {
		t.Errorf("redirect_url = %q", page.RedirectUrl.String)
	}
	if page.PageOrder != 7 || page.Status != int64(domain.StatusDraft) || page.Comment != 1 {
		t.Errorf("page = order %d status %d comment %d, want 7/0/1", page.PageOrder, page.Status, page.Comment)
	}

	// Index lists the page (title, slug checkbox, truncated redirect).
	rec = doRequest(t, h, http.MethodGet, "/admin/pages", nil, session)
	body := rec.Body.String()
	if !strings.Contains(body, `value="about"`) || !strings.Contains(body, ">About</a>") ||
		!strings.Contains(body, "https://example.com/elsewhere") {
		t.Errorf("index does not list the created page")
	}

	// Edit form is prefilled; the rich text textarea carries the content
	// (HTML-escaped by html/template inside the textarea).
	rec = doRequest(t, h, http.MethodGet, "/admin/pages/about/edit", nil, session)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `action="/admin/pages/about"`) ||
		!strings.Contains(rec.Body.String(), `value="About"`) ||
		!strings.Contains(rec.Body.String(), `loading=&#34;lazy&#34;`) {
		t.Fatalf("edit form: status = %d", rec.Code)
	}

	// Update, renaming the slug; the notice rides the redirect onto the index.
	update := validPageForm()
	update.Set("title", "About Us")
	update.Set("slug", "about-us")
	update.Set("redirect_url", "")
	update.Set("status", "publish")
	rec = doRequest(t, h, http.MethodPost, "/admin/pages/about", update, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/pages" {
		t.Fatalf("update: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/pages", nil, session, findCookie(rec, flashCookieName))
	if !strings.Contains(rec.Body.String(), "Page was successfully updated.") {
		t.Error("index does not show the update notice")
	}
	page, err := s.Q.GetAdminPageBySlug(ctx, sql.NullString{String: "about-us", Valid: true})
	if err != nil {
		t.Fatalf("get renamed page: %v", err)
	}
	if page.Title.String != "About Us" || page.RedirectUrl.String != "" ||
		page.Status != int64(domain.StatusPublish) {
		t.Errorf("updated page = %+v", page)
	}

	// Unknown slugs are 404 like Page.find_by!.
	for path, method := range map[string]string{
		"/admin/pages/nope/edit":    http.MethodGet,
		"/admin/pages/nope":         http.MethodPost,
		"/admin/pages/nope/destroy": http.MethodPost,
	} {
		if rec := doRequest(t, h, method, path, validPageForm(), session); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", method, path, rec.Code)
		}
	}

	// Two-stage destroy (spec 4.1): first moves to trash, second deletes for
	// real (comments included).
	now := time.Now().Unix()
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO comments (commentable_type, commentable_id, author_name, content, status, created_at, updated_at)
		 VALUES ('Page', ?, 'ann', 'hi', 1, ?, ?)`, page.ID, now, now); err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/pages/about-us/destroy", nil, session)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/pages" {
		t.Fatalf("trash: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/pages", nil, session, findCookie(rec, flashCookieName))
	if !strings.Contains(rec.Body.String(), "Page was successfully moved to trash.") {
		t.Error("index does not show the trash notice")
	}
	page, err = s.Q.GetAdminPageBySlug(ctx, sql.NullString{String: "about-us", Valid: true})
	if err != nil || page.Status != int64(domain.StatusTrash) {
		t.Fatalf("after trash: status = %d err = %v, want trash", page.Status, err)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/pages/about-us/destroy", nil, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: status = %d, want 303", rec.Code)
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/pages", nil, session, findCookie(rec, flashCookieName))
	if !strings.Contains(rec.Body.String(), "Page was successfully deleted.") {
		t.Error("index does not show the delete notice")
	}
	if _, err := s.Q.GetAdminPageBySlug(ctx, sql.NullString{String: "about-us", Valid: true}); err == nil {
		t.Error("page still present after delete")
	}
	var commentCount int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comments WHERE commentable_type = 'Page' AND commentable_id = ?`, page.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("comments left behind: %d", commentCount)
	}
}

// TestAdminPagesValidation covers the page.rb validation branches; each
// submission re-renders the form with 422 and the Rails wording.
func TestAdminPagesValidation(t *testing.T) {
	s, h := newPagesTestServer(t)
	session := pagesSessionCookie(t, s)

	existing := validPageForm()
	existing.Set("slug", "taken")
	createPageViaForm(t, h, s, session, existing)

	tests := []struct {
		name   string
		mutate func(url.Values)
		want   string
	}{
		{"blank title", func(f url.Values) { f.Set("title", "  ") }, "Title can&#39;t be blank"},
		{"blank slug", func(f url.Values) { f.Set("slug", "") }, "Slug can&#39;t be blank"},
		{"taken slug", func(f url.Values) { f.Set("slug", "taken") }, "Slug has already been taken"},
		{"bad redirect", func(f url.Values) { f.Set("redirect_url", "notaurl") }, "Redirect url is not a valid URL"},
		{"ftp redirect", func(f url.Values) { f.Set("redirect_url", "ftp://example.com/x") }, "Redirect url is not a valid URL"},
		{"bad content type", func(f url.Values) { f.Set("content_type", "textile") }, "Content type is not included in the list"},
		{"bad status", func(f url.Values) { f.Set("status", "archived") }, "Status is not included in the list"},
		{"schedule without time", func(f url.Values) { f.Set("status", "schedule") }, "Scheduled at can&#39;t be blank"},
		{"schedule with bad time", func(f url.Values) {
			f.Set("status", "schedule")
			f.Set("scheduled_at", "tomorrow")
		}, "Scheduled at can&#39;t be blank"},
		{"html without content", func(f url.Values) {
			f.Set("content_type", "html")
			f.Set("html_content", "  ")
		}, "Html content can&#39;t be blank"},
		{"rich text without content", func(f url.Values) { f.Set("content", "<p>  </p>") }, "Content can&#39;t be blank"},
		{"markdown without content", func(f url.Values) {
			f.Set("content_type", "markdown")
			f.Set("markdown_content", "  ")
		}, "Content can&#39;t be blank"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := validPageForm()
			form.Set("slug", "case-"+strconv.Itoa(int(time.Now().UnixNano()%1_000_000)))
			tt.mutate(form)
			rec := doRequest(t, h, http.MethodPost, "/admin/pages", form, session)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("create: status = %d, want 422 containing %q", rec.Code, tt.want)
			}
		})
	}

	// Update re-renders the edit form with 422; the record keeps its values.
	form := validPageForm()
	form.Set("slug", "keep")
	createPageViaForm(t, h, s, session, form)
	bad := validPageForm()
	bad.Set("slug", "taken")
	rec := doRequest(t, h, http.MethodPost, "/admin/pages/keep", bad, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Slug has already been taken") {
		t.Errorf("update with taken slug: status = %d, want 422", rec.Code)
	}
	page, err := s.Q.GetAdminPageBySlug(t.Context(), sql.NullString{String: "keep", Valid: true})
	if err != nil {
		t.Fatalf("page lost after failed update: %v", err)
	}
	if page.Title.String != "About" {
		t.Errorf("failed update still changed the row: title = %q", page.Title.String)
	}
	// Updating the row itself with its own slug is not a uniqueness conflict.
	ok := validPageForm()
	ok.Set("slug", "keep")
	ok.Set("title", "Keep Renamed")
	rec = doRequest(t, h, http.MethodPost, "/admin/pages/keep", ok, session)
	if rec.Code != http.StatusFound {
		t.Errorf("update keeping own slug: status = %d, want 302", rec.Code)
	}
}

// TestAdminPagesIndexOrderAndFilter mirrors fetch_articles(sort_by:
// :page_order): page_order DESC and the status tabs.
func TestAdminPagesIndexOrderAndFilter(t *testing.T) {
	s, h := newPagesTestServer(t)
	session := pagesSessionCookie(t, s)

	for _, tc := range []struct {
		slug  string
		order string
	}{
		{"low", "1"}, {"high", "9"}, {"mid", "5"},
	} {
		form := validPageForm()
		form.Set("slug", tc.slug)
		form.Set("title", tc.slug)
		form.Set("page_order", tc.order)
		createPageViaForm(t, h, s, session, form)
	}
	pub := validPageForm()
	pub.Set("slug", "published")
	pub.Set("title", "published")
	pub.Set("status", "publish")
	createPageViaForm(t, h, s, session, pub)

	rec := doRequest(t, h, http.MethodGet, "/admin/pages", nil, session)
	body := rec.Body.String()
	hi := strings.Index(body, `value="high"`)
	mid := strings.Index(body, `value="mid"`)
	low := strings.Index(body, `value="low"`)
	if hi < 0 || mid < 0 || low < 0 || !(hi < mid && mid < low) {
		t.Errorf("index order = high@%d mid@%d low@%d, want page_order DESC", hi, mid, low)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/pages?status=publish", nil, session)
	body = rec.Body.String()
	if !strings.Contains(body, `value="published"`) || strings.Contains(body, `value="high"`) {
		t.Errorf("status filter does not isolate published pages")
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/pages?status=bogus", nil, session)
	if !strings.Contains(rec.Body.String(), `value="high"`) {
		t.Errorf("unknown status should fall back to the unfiltered list")
	}
}

// TestAdminPagesBatch mirrors process_batch_action for pages: records are
// found by slug, unknown slugs are skipped, the notice carries the count.
func TestAdminPagesBatch(t *testing.T) {
	s, h := newPagesTestServer(t)
	session := pagesSessionCookie(t, s)
	ctx := t.Context()

	for _, slug := range []string{"alpha", "beta", "gamma"} {
		form := validPageForm()
		form.Set("slug", slug)
		createPageViaForm(t, h, s, session, form)
	}
	statusOf := func(slug string) int64 {
		page, err := s.Q.GetAdminPageBySlug(ctx, sql.NullString{String: slug, Valid: true})
		if err != nil {
			t.Fatalf("get %q: %v", slug, err)
		}
		return page.Status
	}

	// batch_publish.
	form := url.Values{}
	form["ids"] = []string{"alpha", "beta", "missing"}
	rec := doRequest(t, h, http.MethodPost, "/admin/pages/batch_publish", form, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/pages" {
		t.Fatalf("batch_publish: status = %d", rec.Code)
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/pages", nil, session, findCookie(rec, flashCookieName))
	if !strings.Contains(rec.Body.String(), "Successfully published 2 page(s).") {
		t.Error("index does not show the publish notice")
	}
	if statusOf("alpha") != int64(domain.StatusPublish) || statusOf("beta") != int64(domain.StatusPublish) {
		t.Error("batch_publish did not publish alpha and beta")
	}

	// batch_unpublish returns them to draft.
	form = url.Values{}
	form["ids[]"] = []string{"alpha", "beta"}
	rec = doRequest(t, h, http.MethodPost, "/admin/pages/batch_unpublish", form, session)
	rec = doRequest(t, h, http.MethodGet, "/admin/pages", nil, session, findCookie(rec, flashCookieName))
	if !strings.Contains(rec.Body.String(), "Successfully unpublished 2 page(s).") {
		t.Error("index does not show the unpublish notice")
	}
	if statusOf("alpha") != int64(domain.StatusDraft) {
		t.Error("batch_unpublish did not return alpha to draft")
	}

	// batch_destroy is a real delete (BaseController#perform_destroy).
	form = url.Values{}
	form["ids"] = []string{"alpha", "gamma"}
	rec = doRequest(t, h, http.MethodPost, "/admin/pages/batch_destroy", form, session)
	rec = doRequest(t, h, http.MethodGet, "/admin/pages", nil, session, findCookie(rec, flashCookieName))
	if !strings.Contains(rec.Body.String(), "Successfully deleted 2 page(s).") {
		t.Error("index does not show the delete notice")
	}
	if _, err := s.Q.GetAdminPageBySlug(ctx, sql.NullString{String: "alpha", Valid: true}); err == nil {
		t.Error("alpha still present after batch_destroy")
	}
	if statusOf("beta") != int64(domain.StatusDraft) {
		t.Error("beta should have survived batch_destroy")
	}

	// An empty selection is a no-op redirect with a zero count.
	rec = doRequest(t, h, http.MethodPost, "/admin/pages/batch_destroy", nil, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("empty batch_destroy: status = %d, want 302", rec.Code)
	}
}

// publishPageJob is one job_runs row of kind publish_page.
type publishPageJob struct {
	id      int64
	payload string
	runAt   int64
	status  string
}

func listPublishPageJobs(t *testing.T, s *Server) []publishPageJob {
	t.Helper()
	rows, err := s.DB.QueryContext(t.Context(),
		`SELECT id, payload, run_at, status FROM job_runs WHERE kind = 'publish_page' ORDER BY id`)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	defer rows.Close()
	var jobs []publishPageJob
	for rows.Next() {
		var j publishPageJob
		if err := rows.Scan(&j.id, &j.payload, &j.runAt, &j.status); err != nil {
			t.Fatalf("scan job: %v", err)
		}
		jobs = append(jobs, j)
	}
	return jobs
}

// TestAdminPagesScheduleEnqueue mirrors Page#schedule_publication with the
// article-side cancel semantics of task T10: saving a scheduled page enqueues
// publish_page at scheduled_at, and rescheduling cancels the old queued job.
func TestAdminPagesScheduleEnqueue(t *testing.T) {
	s, h := newPagesTestServer(t)
	session := pagesSessionCookie(t, s)

	// Create a scheduled page (site time zone defaults to UTC).
	form := validPageForm()
	form.Set("slug", "later")
	form.Set("status", "schedule")
	form.Set("scheduled_at", "2030-01-02T03:04")
	page := createPageViaForm(t, h, s, session, form)

	wantRunAt := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC).Unix()
	jobs := listPublishPageJobs(t, s)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want exactly one", jobs)
	}
	if jobs[0].status != "queued" || jobs[0].runAt != wantRunAt ||
		jobs[0].payload != fmt.Sprintf(`{"page_id":%d}`, page.ID) {
		t.Errorf("job = %+v, want queued at %d with page_id %d", jobs[0], wantRunAt, page.ID)
	}

	// Reschedule: the old job is cancelled (failed) and a new one queued.
	update := validPageForm()
	update.Set("slug", "later")
	update.Set("status", "schedule")
	update.Set("scheduled_at", "2031-06-07T08:09")
	rec := doRequest(t, h, http.MethodPost, "/admin/pages/later", update, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("reschedule: status = %d", rec.Code)
	}
	jobs = listPublishPageJobs(t, s)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %+v, want the cancelled original plus the new one", jobs)
	}
	wantRunAt = time.Date(2031, 6, 7, 8, 9, 0, 0, time.UTC).Unix()
	if jobs[0].status != "failed" {
		t.Errorf("old job status = %q, want failed", jobs[0].status)
	}
	if jobs[1].status != "queued" || jobs[1].runAt != wantRunAt {
		t.Errorf("new job = %+v, want queued at %d", jobs[1], wantRunAt)
	}

	// Saving a non-scheduled page enqueues nothing.
	plain := validPageForm()
	plain.Set("slug", "plain")
	createPageViaForm(t, h, s, session, plain)
	if got := len(listPublishPageJobs(t, s)); got != 2 {
		t.Errorf("jobs after plain create = %d, want 2", got)
	}
}

// TestAdminPagesHTMLMode: html content_type stores html_content (sanitized).
func TestAdminPagesHTMLMode(t *testing.T) {
	s, h := newPagesTestServer(t)
	session := pagesSessionCookie(t, s)

	form := validPageForm()
	form.Set("slug", "raw")
	form.Set("content_type", "html")
	form.Set("content", "<p>ignored rich text</p>")
	form.Set("html_content", `<div><img src="/files/pic.png"><iframe src="javascript:alert(1)"></iframe></div>`)
	page := createPageViaForm(t, h, s, session, form)
	if page.ContentType != "html" {
		t.Fatalf("content_type = %q, want html", page.ContentType)
	}
	stored := page.ContentHtml.String
	if !strings.Contains(stored, `img src="/files/pic.png" loading="lazy"`) {
		t.Errorf("stored html missing lazy image: %s", stored)
	}
	if strings.Contains(stored, "javascript:") {
		t.Errorf("stored html kept a javascript: iframe src: %s", stored)
	}

	// The edit form puts the content into the html textarea for html pages.
	rec := doRequest(t, h, http.MethodGet, "/admin/pages/raw/edit", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `option value="html" selected`) {
		t.Fatalf("edit form for html page: status = %d", rec.Code)
	}
}

// TestAdminPagesMarkdown covers markdown-authored pages: the source lands in
// content_markdown, content_html gets the rendered sanitized HTML, the edit
// form shows the source again, and switching types clears the source.
func TestAdminPagesMarkdown(t *testing.T) {
	s, h := newPagesTestServer(t)
	session := pagesSessionCookie(t, s)
	ctx := t.Context()

	source := "# About\n\nSome **bold** text.\n\n<script>alert(1)</script>\n"
	form := validPageForm()
	form.Set("slug", "about-md")
	form.Set("content_type", "markdown")
	form.Set("content", "<p>ignored rich text</p>")
	form.Set("markdown_content", source)
	page := createPageViaForm(t, h, s, session, form)
	if page.ContentType != string(domain.ContentTypeMarkdown) {
		t.Fatalf("content_type = %q, want markdown", page.ContentType)
	}
	if page.ContentMarkdown.String != source {
		t.Errorf("content_markdown = %q, want the submitted source", page.ContentMarkdown.String)
	}
	stored := page.ContentHtml.String
	if !strings.Contains(stored, "<h1>About</h1>") || !strings.Contains(stored, "<strong>bold</strong>") {
		t.Errorf("stored content_html is not rendered markdown: %s", stored)
	}
	if strings.Contains(stored, "script") {
		t.Errorf("stored content_html kept the script: %s", stored)
	}

	// The edit form selects markdown and shows the source in its textarea.
	rec := doRequest(t, h, http.MethodGet, "/admin/pages/about-md/edit", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit form: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `option value="markdown" selected`) {
		t.Errorf("edit form does not select markdown")
	}
	if !strings.Contains(body, `name="markdown_content"`) ||
		!strings.Contains(body, "# About") || !strings.Contains(body, "**bold**") {
		t.Errorf("edit form does not show the markdown source")
	}

	// Switching back to rich_text clears the stored markdown source.
	form = validPageForm()
	form.Set("slug", "about-md")
	form.Set("content", "<p>Back to rich</p>")
	rec = doRequest(t, h, http.MethodPost, "/admin/pages/about-md", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("switch to rich_text: status = %d", rec.Code)
	}
	page, err := s.Q.GetAdminPageBySlug(ctx, sql.NullString{String: "about-md", Valid: true})
	if err != nil {
		t.Fatalf("page after switch: %v", err)
	}
	if page.ContentType != string(domain.ContentTypeRichText) || page.ContentMarkdown.Valid {
		t.Errorf("after switch: content_type = %q content_markdown = %+v, want rich_text/NULL",
			page.ContentType, page.ContentMarkdown)
	}
	if !strings.Contains(page.ContentHtml.String, "Back to rich") {
		t.Errorf("content_html after switch = %q", page.ContentHtml.String)
	}
}
