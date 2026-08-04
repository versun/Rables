package httpd

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	articlesvc "rables/internal/service/articles"
	"rables/internal/templates"
)

// newArticlesTestServer builds a Server backed by a real SQLite DB and mounts
// the article admin routes on a test-local chi router (not the integrator's
// NewRouter).
func newArticlesTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterArticlesAdminRoutes(r, s)
	return s, r
}

// articlesSessionCookie inserts a user plus session row and returns the
// cookie that satisfies RequireAuth.
func articlesSessionCookie(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	now := time.Now().Unix()
	user, err := s.Q.CreateUser(t.Context(), query.CreateUserParams{
		UserName: "admin", PasswordDigest: "x", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.Q.CreateSession(t.Context(), query.CreateSessionParams{
		Token: "articles-test-token", UserID: user.ID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: "articles-test-token"}
}

// insertArticle stores an article row directly (test setup).
func insertAdminArticle(t *testing.T, s *Server, title, slug string, status domain.Status) query.Article {
	t.Helper()
	now := time.Now().Unix()
	article, err := s.Q.CreateArticle(t.Context(), query.CreateArticleParams{
		Title:                       sql.NullString{String: title, Valid: title != ""},
		Slug:                        sql.NullString{String: slug, Valid: slug != ""},
		ContentHtml:                 sql.NullString{String: "<p>" + title + "</p>", Valid: true},
		ContentType:                 string(domain.ContentTypeRichText),
		Status:                      int64(status),
		ScheduledCrosspostPlatforms: "[]",
		CreatedAt:                   now,
		UpdatedAt:                   now,
	})
	if err != nil {
		t.Fatalf("insert article %q: %v", slug, err)
	}
	return article
}

// enableCrosspost inserts an enabled crossposts row for the platform.
func enableCrosspost(t *testing.T, s *Server, platform string) {
	t.Helper()
	now := time.Now().Unix()
	_, err := s.DB.ExecContext(t.Context(),
		`INSERT INTO crossposts (platform, enabled, created_at, updated_at) VALUES (?, 1, ?, ?)`,
		platform, now, now)
	if err != nil {
		t.Fatalf("enable crosspost %q: %v", platform, err)
	}
}

// enableNativeNewsletter inserts an enabled, fully configured native
// newsletter_settings row (NewsletterSetting#configured?).
func enableNativeNewsletter(t *testing.T, s *Server) {
	t.Helper()
	now := time.Now().Unix()
	_, err := s.DB.ExecContext(t.Context(),
		`INSERT INTO newsletter_settings (id, enabled, provider, from_email, smtp_address, smtp_port, smtp_user_name, smtp_password, created_at, updated_at)
		 VALUES (1, 1, 'native', 'a@b.c', 'smtp.example.com', 587, 'user', 'pass', ?, ?)`, now, now)
	if err != nil {
		t.Fatalf("enable newsletter: %v", err)
	}
}

// flashOf decodes the one-time flash cookie set on the response.
func flashOf(t *testing.T, rec *httptest.ResponseRecorder) templates.Flash {
	t.Helper()
	cookie := findCookie(rec, flashCookieName)
	if cookie == nil {
		return templates.Flash{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		t.Fatalf("decode flash: %v", err)
	}
	var flash templates.Flash
	if err := json.Unmarshal(payload, &flash); err != nil {
		t.Fatalf("unmarshal flash: %v", err)
	}
	return flash
}

// queuedJobs lists queued job_runs of kind with their payloads decoded.
func queuedJobs(t *testing.T, s *Server, kind string) []map[string]any {
	t.Helper()
	runs, err := s.Q.ListQueuedJobRunsByKind(t.Context(), kind)
	if err != nil {
		t.Fatalf("list queued %s: %v", kind, err)
	}
	var out []map[string]any
	for _, run := range runs {
		var payload map[string]any
		if run.Payload.Valid {
			if err := json.Unmarshal([]byte(run.Payload.String), &payload); err != nil {
				t.Fatalf("decode payload of job %d: %v", run.ID, err)
			}
			payload["_run_at"] = run.RunAt
		}
		out = append(out, payload)
	}
	return out
}

// validArticleForm is a minimal submittable create form. It carries the
// hidden "0" inputs a real browser submits for every check_box, so checking
// a box is Set(name, "1") and leaving it alone means unchecked.
func validArticleForm() url.Values {
	return url.Values{
		"title":                 {"Hello World"},
		"status":                {"draft"},
		"content_type":          {"rich_text"},
		"content":               {"<p>Hello body</p>"},
		"comment":               {"0"},
		"send_newsletter":       {"0"},
		"crosspost_mastodon":    {"0"},
		"crosspost_twitter":     {"0"},
		"crosspost_bluesky":     {"0"},
		"crosspost_xiaohongshu": {"0"},
	}
}

// TestAdminArticlesAuth: every article admin route sits behind RequireAuth.
func TestAdminArticlesAuth(t *testing.T) {
	_, h := newArticlesTestServer(t)
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin"},
		{http.MethodGet, "/admin/"},
		{http.MethodGet, "/admin/posts"},
		{http.MethodGet, "/admin/posts/new"},
		{http.MethodPost, "/admin/posts"},
		{http.MethodGet, "/admin/posts/drafts"},
		{http.MethodGet, "/admin/posts/scheduled"},
		{http.MethodGet, "/admin/posts/x/edit"},
		{http.MethodPost, "/admin/posts/x"},
		{http.MethodPost, "/admin/posts/x/destroy"},
		{http.MethodPost, "/admin/posts/x/publish"},
		{http.MethodPost, "/admin/posts/x/unpublish"},
		{http.MethodPost, "/admin/posts/x/fetch_comments"},
		{http.MethodPost, "/admin/posts/batch_destroy"},
		{http.MethodPost, "/admin/posts/batch_publish"},
		{http.MethodPost, "/admin/posts/batch_unpublish"},
		{http.MethodPost, "/admin/posts/batch_add_tags"},
		{http.MethodPost, "/admin/posts/batch_crosspost"},
		{http.MethodPost, "/admin/posts/batch_newsletter"},
	}
	for _, tt := range tests {
		rec := doRequest(t, h, tt.method, tt.path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("%s %s unauthenticated: status = %d location = %q, want 302 /session/new",
				tt.method, tt.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestAdminArticlesCreateFlow walks create through validation and the stored
// column semantics (slug, excerpt, sanitize, lazy loading, defaults).
func TestAdminArticlesCreateFlow(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()

	// New form renders with the create action and comments enabled by default.
	rec := doRequest(t, h, http.MethodGet, "/admin/posts/new", nil, session)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `action="/admin/posts"`) ||
		!strings.Contains(rec.Body.String(), `name="comment" value="1" checked`) {
		t.Fatalf("new form: status = %d", rec.Code)
	}

	// Create: slug from title, excerpt from the body, script stripped, lazy img.
	form := validArticleForm()
	form.Set("content", `<p>Hello body</p><script>alert(1)</script><p><img src="/x.png"></p>`)
	form.Set("tag_list", "go, web")
	form.Set("social_url_mastodon", "https://mastodon.social/@me/1")
	rec = doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/posts" {
		t.Fatalf("create: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if flash := flashOf(t, rec); flash.Notice != "Article was successfully created." {
		t.Errorf("create flash = %+v", flash)
	}
	article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug("hello-world"))
	if err != nil {
		t.Fatalf("created article not found: %v", err)
	}
	if article.Status != int64(domain.StatusDraft) {
		t.Errorf("status = %d, want draft", article.Status)
	}
	body := article.ContentHtml.String
	if strings.Contains(body, "script") {
		t.Errorf("content_html still contains script: %q", body)
	}
	if !strings.Contains(body, `loading="lazy"`) {
		t.Errorf("img missing loading=lazy: %q", body)
	}
	if article.Excerpt.String != "Hello body" {
		t.Errorf("excerpt = %q, want derived from body", article.Excerpt.String)
	}
	if article.Comment != 0 {
		t.Errorf("comment = %d, want 0 (checkbox unchecked)", article.Comment)
	}

	// Tags were found-or-created and linked.
	tags, err := s.Q.ListArticleTagNames(ctx, []int64{article.ID})
	if err != nil || len(tags) != 2 {
		t.Fatalf("article tags = %+v, %v", tags, err)
	}
	// The social post URL was stored.
	posts, err := s.Q.ListSocialPostsByArticleID(ctx, article.ID)
	if err != nil || len(posts) != 1 || posts[0].Url != "https://mastodon.social/@me/1" {
		t.Errorf("social posts = %+v, %v", posts, err)
	}

	// create_and_add_another redirects to the new form.
	form = validArticleForm()
	form.Set("title", "Another Post")
	form.Set("create_and_add_another", "1")
	rec = doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/posts/new" {
		t.Errorf("create_and_add_another: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}

	// Explicit description wins over the body for the excerpt.
	form = validArticleForm()
	form.Set("title", "Described")
	form.Set("description", "  Custom description  ")
	rec = doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
	described, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug("described"))
	if err != nil {
		t.Fatalf("described article: %v", err)
	}
	if described.Excerpt.String != "Custom description" {
		t.Errorf("excerpt = %q, want the squished description", described.Excerpt.String)
	}
}

// TestAdminArticlesCreateValidation covers the 422 re-render branches.
func TestAdminArticlesCreateValidation(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)

	insertAdminArticle(t, s, "Taken", "taken-slug", domain.StatusDraft)

	tests := []struct {
		name    string
		mutate  func(url.Values)
		wantErr string
	}{
		{"blank rich text content", func(f url.Values) { f.Set("content", "  ") }, "Content can&#39;t be blank"},
		{"blank html content", func(f url.Values) {
			f.Set("content_type", "html")
			f.Set("html_content", " ")
		}, "Html content can&#39;t be blank"},
		{"rich text html-only content", func(f url.Values) { f.Set("content", "<p></p>") }, "Content can&#39;t be blank"},
		{"slug taken", func(f url.Values) { f.Set("slug", "taken-slug") }, "Slug has already been taken"},
		{"slug reserved", func(f url.Values) { f.Set("slug", "admin") }, "Slug is reserved"},
		{"schedule without scheduled_at", func(f url.Values) { f.Set("status", "schedule") }, "Scheduled at can&#39;t be blank"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := validArticleForm()
			form.Set("title", "Case "+tt.name)
			tt.mutate(form)
			rec := doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.wantErr) {
				t.Errorf("body does not contain %q", tt.wantErr)
			}
			// The re-render keeps the submitted title.
			if !strings.Contains(rec.Body.String(), `value="Case `+tt.name+`"`) {
				t.Errorf("body does not keep the submitted title")
			}
		})
	}

	// Unknown status is a bad request, not a save.
	form := validArticleForm()
	form.Set("status", "bogus")
	if rec := doRequest(t, h, http.MethodPost, "/admin/posts", form, session); rec.Code != http.StatusBadRequest {
		t.Errorf("bogus status: status = %d, want 400", rec.Code)
	}
}

// TestAdminArticlesMarkdownFlow covers markdown authoring: the source lands in
// content_markdown while content_html gets the rendered, sanitized HTML, and
// the edit form shows the source again.
func TestAdminArticlesMarkdownFlow(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()

	source := "# Hello\n\nSome **bold** text.\n\n<script>alert(1)</script>\n\n![pic](/x.png)\n"
	form := validArticleForm()
	form.Set("title", "Markdown Post")
	form.Set("content_type", "markdown")
	form.Set("markdown_content", source)
	rec := doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("markdown create: status = %d", rec.Code)
	}
	article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug("markdown-post"))
	if err != nil {
		t.Fatalf("markdown article: %v", err)
	}
	if article.ContentType != string(domain.ContentTypeMarkdown) {
		t.Errorf("content_type = %q, want markdown", article.ContentType)
	}
	if article.ContentMarkdown.String != source {
		t.Errorf("content_markdown = %q, want the submitted source", article.ContentMarkdown.String)
	}
	body := article.ContentHtml.String
	for _, want := range []string{"<h1>Hello</h1>", "<strong>bold</strong>", `loading="lazy"`} {
		if !strings.Contains(body, want) {
			t.Errorf("content_html missing %q: %q", want, body)
		}
	}
	if strings.Contains(body, "script") {
		t.Errorf("content_html still contains script: %q", body)
	}

	// The edit form selects markdown and shows the source, not rendered HTML.
	rec = doRequest(t, h, http.MethodGet, "/admin/posts/markdown-post/edit", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit form: status = %d", rec.Code)
	}
	page := rec.Body.String()
	if !strings.Contains(page, `value="markdown" selected`) {
		t.Errorf("edit form does not select markdown")
	}
	if !strings.Contains(page, `name="markdown_content"`) ||
		!strings.Contains(page, "# Hello") || !strings.Contains(page, "**bold**") {
		t.Errorf("edit form does not show the markdown source")
	}

	// Blank markdown source is a validation error.
	form = validArticleForm()
	form.Set("title", "Blank Markdown")
	form.Set("content_type", "markdown")
	form.Set("markdown_content", "  ")
	rec = doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
	if rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Content can&#39;t be blank") {
		t.Errorf("blank markdown: status = %d", rec.Code)
	}

	// Switching back to rich_text clears the stored markdown source.
	form = validArticleForm()
	form.Set("title", "Markdown Post")
	form.Set("content", "<p>Back to rich</p>")
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/markdown-post", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("switch to rich_text: status = %d", rec.Code)
	}
	article, _ = s.Q.GetAdminArticleBySlug(ctx, nullSlug("markdown-post"))
	if article.ContentType != string(domain.ContentTypeRichText) {
		t.Errorf("content_type after switch = %q", article.ContentType)
	}
	if article.ContentMarkdown.Valid {
		t.Errorf("content_markdown after switch = %q, want NULL", article.ContentMarkdown.String)
	}
	if !strings.Contains(article.ContentHtml.String, "Back to rich") {
		t.Errorf("content_html after switch = %q", article.ContentHtml.String)
	}
}

// TestAdminArticlesSchedule covers the schedule save: snapshot fields and the
// publish_article job, including old-job cancellation on reschedule.
func TestAdminArticlesSchedule(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()
	enableCrosspost(t, s, "mastodon")

	when := time.Date(2027, 1, 15, 10, 30, 0, 0, time.UTC)
	form := validArticleForm()
	form.Set("title", "Scheduled Post")
	form.Set("status", "schedule")
	form.Set("scheduled_at", "2027-01-15T10:30") // settings time zone: UTC default
	form.Set("crosspost_mastodon", "1")
	form.Set("send_newsletter", "1")
	rec := doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("schedule create: status = %d", rec.Code)
	}
	article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug("scheduled-post"))
	if err != nil {
		t.Fatalf("scheduled article: %v", err)
	}
	if !article.ScheduledAt.Valid || article.ScheduledAt.Int64 != when.Unix() {
		t.Errorf("scheduled_at = %+v, want %d", article.ScheduledAt, when.Unix())
	}
	if article.ScheduledCrosspostPlatforms != `["mastodon"]` {
		t.Errorf("snapshot platforms = %q", article.ScheduledCrosspostPlatforms)
	}
	if article.ScheduledSendNewsletter != 1 {
		t.Errorf("snapshot newsletter = %d, want 1", article.ScheduledSendNewsletter)
	}
	// No crosspost/newsletter job while scheduled.
	if n := len(queuedJobs(t, s, jobs.KindCrosspost)); n != 0 {
		t.Errorf("crosspost jobs = %d, want 0 before publication", n)
	}
	runs, err := s.Q.ListQueuedJobRunsByKind(ctx, jobs.KindPublishArticle)
	if err != nil || len(runs) != 1 {
		t.Fatalf("publish jobs = %+v, %v", runs, err)
	}
	if runs[0].RunAt != when.Unix() {
		t.Errorf("publish job run_at = %d, want %d", runs[0].RunAt, when.Unix())
	}
	var payload articlesvc.PublishArticlePayload
	if err := json.Unmarshal([]byte(runs[0].Payload.String), &payload); err != nil || payload.ArticleID != article.ID {
		t.Errorf("publish job payload = %q, %v", runs[0].Payload.String, err)
	}

	// Reschedule via update: the old queued job is replaced, not duplicated.
	later := time.Date(2027, 2, 20, 8, 0, 0, 0, time.UTC)
	form = validArticleForm()
	form.Set("title", "Scheduled Post")
	form.Set("status", "schedule")
	form.Set("scheduled_at", "2027-02-20T08:00")
	form.Set("crosspost_twitter", "1") // mastodon unchecked now
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/scheduled-post", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("reschedule: status = %d", rec.Code)
	}
	runs, err = s.Q.ListQueuedJobRunsByKind(ctx, jobs.KindPublishArticle)
	if err != nil || len(runs) != 1 {
		t.Fatalf("publish jobs after reschedule = %+v, %v", runs, err)
	}
	if runs[0].RunAt != later.Unix() {
		t.Errorf("rescheduled run_at = %d, want %d", runs[0].RunAt, later.Unix())
	}
	article, _ = s.Q.GetAdminArticleBySlug(ctx, nullSlug("scheduled-post"))
	if article.ScheduledCrosspostPlatforms != `["twitter"]` {
		t.Errorf("snapshot after update = %q", article.ScheduledCrosspostPlatforms)
	}
	if article.ScheduledSendNewsletter != 0 {
		t.Errorf("snapshot newsletter after update = %d, want 0", article.ScheduledSendNewsletter)
	}
}

// TestAdminArticlesPublishUnpublish covers the member actions and the
// schedule-snapshot consumption on publish.
func TestAdminArticlesPublishUnpublish(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()
	enableCrosspost(t, s, "mastodon")
	enableNativeNewsletter(t, s)

	// Publishing a draft fires crosspost/newsletter only for form-checked,
	// enabled platforms; the member action restores nothing for drafts.
	draft := insertAdminArticle(t, s, "Draft Post", "draft-post", domain.StatusDraft)
	rec := doRequest(t, h, http.MethodPost, "/admin/posts/draft-post/publish", nil, session)
	if rec.Code != http.StatusFound || flashOf(t, rec).Notice != "Article was successfully published." {
		t.Fatalf("publish: status = %d flash = %+v", rec.Code, flashOf(t, rec))
	}
	article, _ := s.Q.GetAdminArticleByID(ctx, draft.ID)
	if article.Status != int64(domain.StatusPublish) {
		t.Errorf("status = %d, want publish", article.Status)
	}
	if n := len(queuedJobs(t, s, jobs.KindCrosspost)); n != 0 {
		t.Errorf("crosspost jobs = %d, want 0 (nothing was selected)", n)
	}

	// Unpublish returns to draft.
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/draft-post/unpublish", nil, session)
	if rec.Code != http.StatusFound || flashOf(t, rec).Notice != "Article was successfully unpublished." {
		t.Fatalf("unpublish: status = %d flash = %+v", rec.Code, flashOf(t, rec))
	}
	article, _ = s.Q.GetAdminArticleByID(ctx, draft.ID)
	if article.Status != int64(domain.StatusDraft) {
		t.Errorf("status = %d, want draft", article.Status)
	}

	// Publishing a scheduled article consumes the snapshot: crosspost and
	// newsletter jobs fire for the restored selections, the snapshot columns
	// clear, and (Rails parity) scheduled_at stays.
	form := validArticleForm()
	form.Set("title", "Snap Post")
	form.Set("status", "schedule")
	form.Set("scheduled_at", "2027-03-01T09:00")
	form.Set("crosspost_mastodon", "1")
	form.Set("send_newsletter", "1")
	if rec := doRequest(t, h, http.MethodPost, "/admin/posts", form, session); rec.Code != http.StatusFound {
		t.Fatalf("schedule create: status = %d", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/snap-post/publish", nil, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("publish scheduled: status = %d", rec.Code)
	}
	article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug("snap-post"))
	if err != nil {
		t.Fatalf("snap post: %v", err)
	}
	if article.Status != int64(domain.StatusPublish) {
		t.Errorf("status = %d, want publish", article.Status)
	}
	if article.ScheduledCrosspostPlatforms != "[]" || article.ScheduledSendNewsletter != 0 {
		t.Errorf("snapshot not cleared: %q / %d", article.ScheduledCrosspostPlatforms, article.ScheduledSendNewsletter)
	}
	if !article.ScheduledAt.Valid {
		t.Error("scheduled_at cleared; Rails keeps it on a manual publish")
	}
	crossJobs := queuedJobs(t, s, jobs.KindCrosspost)
	if len(crossJobs) != 1 || crossJobs[0]["platform"] != "mastodon" || int64(crossJobs[0]["article_id"].(float64)) != article.ID {
		t.Errorf("crosspost jobs = %+v, want one mastodon job for the article", crossJobs)
	}
	newsJobs := queuedJobs(t, s, jobs.KindSendNewsletter)
	if len(newsJobs) != 1 || int64(newsJobs[0]["article_id"].(float64)) != article.ID {
		t.Errorf("newsletter jobs = %+v, want one for the article", newsJobs)
	}

	// Unknown slugs are 404 on all member actions.
	for _, path := range []string{"/admin/posts/nope/publish", "/admin/posts/nope/unpublish", "/admin/posts/nope/destroy", "/admin/posts/nope/fetch_comments"} {
		if rec := doRequest(t, h, http.MethodPost, path, nil, session); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s: status = %d, want 404", path, rec.Code)
		}
	}
	if rec := doRequest(t, h, http.MethodGet, "/admin/posts/nope/edit", nil, session); rec.Code != http.StatusNotFound {
		t.Errorf("GET edit: status = %d, want 404", rec.Code)
	}
}

// TestAdminArticlesPublishFormSave covers handle_crosspost/handle_newsletter
// on a direct publish save (checkbox-driven), including the xiaohongshu
// log-only skip and the disabled-platform skip.
func TestAdminArticlesPublishFormSave(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	enableCrosspost(t, s, "twitter")
	enableCrosspost(t, s, "xiaohongshu")
	enableNativeNewsletter(t, s)

	form := validArticleForm()
	form.Set("title", "Publish Now")
	form.Set("status", "publish")
	form.Set("crosspost_twitter", "1")
	form.Set("crosspost_mastodon", "1")    // not enabled -> skipped
	form.Set("crosspost_xiaohongshu", "1") // enabled but log-only -> skipped
	form.Set("send_newsletter", "1")
	rec := doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("publish create: status = %d", rec.Code)
	}
	article, err := s.Q.GetAdminArticleBySlug(t.Context(), nullSlug("publish-now"))
	if err != nil {
		t.Fatalf("article: %v", err)
	}
	jobs_ := queuedJobs(t, s, jobs.KindCrosspost)
	if len(jobs_) != 1 || jobs_[0]["platform"] != "twitter" {
		t.Errorf("crosspost jobs = %+v, want exactly one twitter job", jobs_)
	}
	if jobs_[0]["requested_at"] == nil {
		t.Errorf("crosspost payload missing requested_at: %+v", jobs_[0])
	}
	if n := len(queuedJobs(t, s, jobs.KindSendNewsletter)); n != 1 {
		t.Errorf("newsletter jobs = %d, want 1", n)
	}
	if article.ScheduledCrosspostPlatforms != "[]" {
		t.Errorf("snapshot = %q, want [] for a publish save", article.ScheduledCrosspostPlatforms)
	}
}

// TestAdminArticlesUpdate covers the edit/update flow: form values, tag
// reset, social post sync, slug self-conflict, and schedule-snapshot clears.
func TestAdminArticlesUpdate(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()

	form := validArticleForm()
	form.Set("title", "Original")
	form.Set("tag_list", "old")
	form.Set("social_url_twitter", "https://x.com/me/1")
	if rec := doRequest(t, h, http.MethodPost, "/admin/posts", form, session); rec.Code != http.StatusFound {
		t.Fatalf("create: status = %d", rec.Code)
	}

	// Edit form shows stored values.
	rec := doRequest(t, h, http.MethodGet, "/admin/posts/original/edit", nil, session)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `action="/admin/posts/original"`) ||
		!strings.Contains(rec.Body.String(), `value="Original"`) ||
		!strings.Contains(rec.Body.String(), `value="old"`) ||
		!strings.Contains(rec.Body.String(), `value="https://x.com/me/1"`) {
		t.Fatalf("edit form: status = %d body missing stored values", rec.Code)
	}

	// Update: tags reset (not appended), social url cleared, own slug kept.
	form = validArticleForm()
	form.Set("title", "Renamed")
	form.Set("slug", "original") // unchanged slug must not self-conflict
	form.Set("tag_list", "new1, new2")
	form.Set("social_url_twitter", "")
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/original", form, session)
	if rec.Code != http.StatusFound || flashOf(t, rec).Notice != "Article was successfully updated." {
		t.Fatalf("update: status = %d flash = %+v", rec.Code, flashOf(t, rec))
	}
	article, _ := s.Q.GetAdminArticleBySlug(ctx, nullSlug("original"))
	if article.Title.String != "Renamed" {
		t.Errorf("title = %q", article.Title.String)
	}
	tags, _ := s.Q.ListArticleTagNames(ctx, []int64{article.ID})
	if len(tags) != 2 || tags[0].Name != "new1" || tags[1].Name != "new2" {
		t.Errorf("tags after update = %+v, want new1/new2 only", tags)
	}
	posts, _ := s.Q.ListSocialPostsByArticleID(ctx, article.ID)
	if len(posts) != 0 {
		t.Errorf("social posts after blank url = %+v, want none", posts)
	}

	// Slug conflict with another article re-renders 422.
	insertAdminArticle(t, s, "Other", "other-slug", domain.StatusDraft)
	form = validArticleForm()
	form.Set("slug", "other-slug")
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/original", form, session)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Slug has already been taken") {
		t.Errorf("slug conflict update: status = %d", rec.Code)
	}

	// Schedule -> draft via the form clears the snapshot (sync callbacks).
	form = validArticleForm()
	form.Set("title", "To Schedule")
	form.Set("status", "schedule")
	form.Set("scheduled_at", "2027-04-01T12:00")
	form.Set("crosspost_mastodon", "1")
	doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
	form.Set("status", "draft")
	form.Del("scheduled_at")
	form.Del("crosspost_mastodon")
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/to-schedule", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("deschedule: status = %d", rec.Code)
	}
	article, _ = s.Q.GetAdminArticleBySlug(ctx, nullSlug("to-schedule"))
	if article.ScheduledCrosspostPlatforms != "[]" || article.ScheduledSendNewsletter != 0 {
		t.Errorf("snapshot after deschedule = %q / %d", article.ScheduledCrosspostPlatforms, article.ScheduledSendNewsletter)
	}
	// Rails parity: leaving schedule without saving a new schedule keeps the
	// stale queued job (the publish worker skips it).
	if n := len(queuedJobs(t, s, jobs.KindPublishArticle)); n != 1 {
		t.Errorf("publish jobs after deschedule = %d, want 1 (stale, self-skips)", n)
	}
}

// TestAdminArticlesDestroy covers the two-stage delete.
func TestAdminArticlesDestroy(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()

	form := validArticleForm()
	form.Set("title", "Doomed")
	form.Set("status", "schedule")
	form.Set("scheduled_at", "2027-05-01T10:00")
	form.Set("tag_list", "tagged")
	form.Set("social_url_bluesky", "https://bsky.app/p/1")
	doRequest(t, h, http.MethodPost, "/admin/posts", form, session)
	article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug("doomed"))
	if err != nil {
		t.Fatalf("article: %v", err)
	}
	// Attach a comment so the dependent destroy is observable.
	now := time.Now().Unix()
	if _, err := s.Q.CreateComment(ctx, query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: article.ID, Valid: true},
		ArticleID:       sql.NullInt64{Int64: article.ID, Valid: true},
		AuthorName:      "a", Content: "c",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	// Stage one: schedule -> trash. 303 see_other, like the Rails redirect.
	rec := doRequest(t, h, http.MethodPost, "/admin/posts/doomed/destroy", nil, session)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/posts" {
		t.Fatalf("trash: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if flash := flashOf(t, rec); flash.Notice != "Article was successfully moved to trash." {
		t.Errorf("trash flash = %+v", flash)
	}
	article, _ = s.Q.GetAdminArticleBySlug(ctx, nullSlug("doomed"))
	if article.Status != int64(domain.StatusTrash) {
		t.Errorf("status = %d, want trash", article.Status)
	}

	// Stage two: trash -> real delete, dependents included, queued job dropped.
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/doomed/destroy", nil, session)
	if rec.Code != http.StatusSeeOther || flashOf(t, rec).Notice != "Article was successfully deleted." {
		t.Fatalf("delete: status = %d flash = %+v", rec.Code, flashOf(t, rec))
	}
	if _, err := s.Q.GetAdminArticleByID(ctx, article.ID); err == nil {
		t.Error("article still present after delete")
	}
	var orphans int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM comments WHERE article_id = ?`, article.ID).Scan(&orphans); err != nil || orphans != 0 {
		t.Errorf("orphan comments = %d, %v", orphans, err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM article_tags WHERE article_id = ?`, article.ID).Scan(&orphans); err != nil || orphans != 0 {
		t.Errorf("orphan article_tags = %d, %v", orphans, err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM social_media_posts WHERE article_id = ?`, article.ID).Scan(&orphans); err != nil || orphans != 0 {
		t.Errorf("orphan social posts = %d, %v", orphans, err)
	}
	if n := len(queuedJobs(t, s, jobs.KindPublishArticle)); n != 0 {
		t.Errorf("publish jobs after delete = %d, want 0", n)
	}
	// The tag itself survives the article delete.
	if _, err := s.Q.GetTagByLowerName(ctx, "tagged"); err != nil {
		t.Error("tag row should survive the article delete")
	}
}

// TestAdminArticlesIndex covers the list: status tabs, search with LIKE
// escaping, and pagination edge cases.
func TestAdminArticlesIndex(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)

	insertAdminArticle(t, s, "Hello World", "hello-world", domain.StatusPublish)
	insertAdminArticle(t, s, "Draft Note", "draft-note", domain.StatusDraft)
	insertAdminArticle(t, s, "100% Real", "hundred", domain.StatusPublish)
	insertAdminArticle(t, s, "1000 Ways", "thousand", domain.StatusPublish)

	// Admin root renders the same list.
	rec := doRequest(t, h, http.MethodGet, "/admin", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Hello World") {
		t.Fatalf("admin root: status = %d", rec.Code)
	}

	// Status filter.
	rec = doRequest(t, h, http.MethodGet, "/admin/posts?status=draft", nil, session)
	if body := rec.Body.String(); !strings.Contains(body, "Draft Note") || strings.Contains(body, "Hello World") {
		t.Errorf("status=draft filter wrong")
	}

	// Search by title.
	rec = doRequest(t, h, http.MethodGet, "/admin/posts?q=Hello", nil, session)
	if body := rec.Body.String(); !strings.Contains(body, "Hello World") || strings.Contains(body, "Draft Note") {
		t.Errorf("q=Hello filter wrong")
	}
	// Literal % must not act as a wildcard (sanitize_sql_like escaping).
	rec = doRequest(t, h, http.MethodGet, "/admin/posts?q="+url.QueryEscape("100%"), nil, session)
	body := rec.Body.String()
	if !strings.Contains(body, "100% Real") || strings.Contains(body, "1000 Ways") {
		t.Errorf("q=100%% should match only the literal percent title")
	}
	// Search combines with the status filter.
	rec = doRequest(t, h, http.MethodGet, "/admin/posts?status=draft&q=Hello", nil, session)
	if strings.Contains(rec.Body.String(), "Hello World") {
		t.Errorf("status=draft&q=Hello should be empty")
	}
	// Search hits content_html too.
	rec = doRequest(t, h, http.MethodGet, "/admin/posts?q="+url.QueryEscape("<p>Draft"), nil, session)
	if !strings.Contains(rec.Body.String(), "Draft Note") {
		t.Errorf("content search should find the draft")
	}

	// Invalid pages 404 (WillPaginate::InvalidPage).
	for _, p := range []string{"0", "-1", "abc"} {
		if rec := doRequest(t, h, http.MethodGet, "/admin/posts?page="+p, nil, session); rec.Code != http.StatusNotFound {
			t.Errorf("page=%s: status = %d, want 404", p, rec.Code)
		}
	}
	// A page beyond the total renders empty (will_paginate does not raise).
	rec = doRequest(t, h, http.MethodGet, "/admin/posts?page=99", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No posts found.") {
		t.Errorf("page=99: status = %d, want 200 empty", rec.Code)
	}
}

// TestAdminArticlesPagination covers the 100-per-page split.
func TestAdminArticlesPagination(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)

	base := time.Now().Unix() - 200000
	for i := 1; i <= 101; i++ {
		// Distinct created_at values keep the created_at DESC order total.
		if _, err := s.Q.CreateArticle(t.Context(), query.CreateArticleParams{
			Title:                       sql.NullString{String: fmt.Sprintf("Post %03d", i), Valid: true},
			Slug:                        sql.NullString{String: fmt.Sprintf("post-%03d", i), Valid: true},
			ContentHtml:                 sql.NullString{String: "<p>x</p>", Valid: true},
			ContentType:                 string(domain.ContentTypeRichText),
			Status:                      int64(domain.StatusDraft),
			ScheduledCrosspostPlatforms: "[]",
			CreatedAt:                   base + int64(i),
			UpdatedAt:                   base + int64(i),
		}); err != nil {
			t.Fatalf("insert article %d: %v", i, err)
		}
	}
	rec := doRequest(t, h, http.MethodGet, "/admin/posts", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Post 101") || strings.Contains(rec.Body.String(), "Post 001") {
		t.Errorf("page 1 should show the newest 100")
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/posts?page=2", nil, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Post 001") || strings.Contains(rec.Body.String(), "Post 101") {
		t.Errorf("page 2 should show only the oldest one")
	}
}

// TestAdminArticlesDraftsScheduled covers the scoped collection pages.
func TestAdminArticlesDraftsScheduled(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)

	insertAdminArticle(t, s, "A Draft", "a-draft", domain.StatusDraft)
	insertAdminArticle(t, s, "A Scheduled", "a-scheduled", domain.StatusSchedule)

	rec := doRequest(t, h, http.MethodGet, "/admin/posts/drafts", nil, session)
	if body := rec.Body.String(); !strings.Contains(body, "A Draft") || strings.Contains(body, "A Scheduled") {
		t.Errorf("drafts page wrong")
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/posts/scheduled", nil, session)
	if body := rec.Body.String(); !strings.Contains(body, "A Scheduled") || strings.Contains(body, "A Draft") {
		t.Errorf("scheduled page wrong")
	}
	// A mismatched status filter ANDs with the scope and empties the page.
	rec = doRequest(t, h, http.MethodGet, "/admin/posts/drafts?status=publish", nil, session)
	if !strings.Contains(rec.Body.String(), "No posts found.") {
		t.Errorf("drafts?status=publish should be empty")
	}
}

// TestAdminArticlesBatchDestroy covers the two-stage batch delete.
func TestAdminArticlesBatchDestroy(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()

	insertAdminArticle(t, s, "Keep", "keep", domain.StatusPublish)
	insertAdminArticle(t, s, "Trash Me", "trash-me", domain.StatusPublish)
	insertAdminArticle(t, s, "Already Trashed", "already-trashed", domain.StatusTrash)

	// Empty selection redirects with the alert.
	rec := doRequest(t, h, http.MethodPost, "/admin/posts/batch_destroy", nil, session)
	if rec.Code != http.StatusFound || flashOf(t, rec).Alert != "请至少选择一个文章。" {
		t.Fatalf("empty batch_destroy: status = %d flash = %+v", rec.Code, flashOf(t, rec))
	}

	form := url.Values{}
	form["ids"] = []string{"trash-me", "already-trashed", "missing-slug"}
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_destroy", form, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/posts" {
		t.Fatalf("batch_destroy: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	flash := flashOf(t, rec)
	if !strings.Contains(flash.Notice, "成功将 1 篇文章移动到垃圾箱。") || !strings.Contains(flash.Notice, "成功删除 1 篇文章。") {
		t.Errorf("batch_destroy notice = %q", flash.Notice)
	}
	trashed, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug("trash-me"))
	if err != nil || trashed.Status != int64(domain.StatusTrash) {
		t.Errorf("trash-me = %+v, %v", trashed, err)
	}
	if _, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug("already-trashed")); err == nil {
		t.Error("already-trashed should be really deleted")
	}
	if _, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug("keep")); err != nil {
		t.Error("keep should be untouched")
	}
}

// TestAdminArticlesBatchPublishUnpublish covers process_batch_action for the
// two status transitions, including snapshot consumption on batch publish.
func TestAdminArticlesBatchPublishUnpublish(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()
	enableCrosspost(t, s, "mastodon")

	insertAdminArticle(t, s, "One", "one", domain.StatusDraft)
	insertAdminArticle(t, s, "Two", "two", domain.StatusPublish)

	form := url.Values{}
	form["ids"] = []string{"one", "two", "gone"}
	rec := doRequest(t, h, http.MethodPost, "/admin/posts/batch_publish", form, session)
	if rec.Code != http.StatusFound || flashOf(t, rec).Notice != "Successfully published 2 article(s)." {
		t.Fatalf("batch_publish: status = %d flash = %+v", rec.Code, flashOf(t, rec))
	}
	one, _ := s.Q.GetAdminArticleBySlug(ctx, nullSlug("one"))
	if one.Status != int64(domain.StatusPublish) {
		t.Errorf("one status = %d, want publish", one.Status)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_unpublish", form, session)
	if rec.Code != http.StatusFound || flashOf(t, rec).Notice != "Successfully unpublished 2 article(s)." {
		t.Fatalf("batch_unpublish: status = %d flash = %+v", rec.Code, flashOf(t, rec))
	}
	one, _ = s.Q.GetAdminArticleBySlug(ctx, nullSlug("one"))
	if one.Status != int64(domain.StatusDraft) {
		t.Errorf("one status = %d, want draft", one.Status)
	}

	// Batch publishing a scheduled article restores the snapshot (crosspost).
	cform := validArticleForm()
	cform.Set("title", "Sched Batch")
	cform.Set("status", "schedule")
	cform.Set("scheduled_at", "2027-06-01T10:00")
	cform.Set("crosspost_mastodon", "1")
	doRequest(t, h, http.MethodPost, "/admin/posts", cform, session)
	form = url.Values{}
	form["ids"] = []string{"sched-batch"}
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_publish", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("batch publish scheduled: status = %d", rec.Code)
	}
	article, _ := s.Q.GetAdminArticleBySlug(ctx, nullSlug("sched-batch"))
	if article.Status != int64(domain.StatusPublish) || article.ScheduledCrosspostPlatforms != "[]" {
		t.Errorf("scheduled batch publish: status = %d snapshot = %q", article.Status, article.ScheduledCrosspostPlatforms)
	}
	jobs_ := queuedJobs(t, s, jobs.KindCrosspost)
	if len(jobs_) != 1 || jobs_[0]["platform"] != "mastodon" {
		t.Errorf("crosspost jobs = %+v, want one mastodon job from the snapshot", jobs_)
	}
}

// TestAdminArticlesBatchAddTags covers batch_add_tags: validation alerts and
// the append-only tag merge.
func TestAdminArticlesBatchAddTags(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()

	form := validArticleForm()
	form.Set("title", "Tagged One")
	form.Set("tag_list", "existing")
	doRequest(t, h, http.MethodPost, "/admin/posts", form, session)

	// Empty selection / blank tag names both alert.
	rec := doRequest(t, h, http.MethodPost, "/admin/posts/batch_add_tags", url.Values{"tag_names": {"x"}}, session)
	if flashOf(t, rec).Alert != "请至少选择一个文章。" {
		t.Errorf("empty ids alert = %+v", flashOf(t, rec))
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_add_tags", url.Values{"ids": {"tagged-one"}}, session)
	if flashOf(t, rec).Alert != "请输入至少一个标签。" {
		t.Errorf("blank tag_names alert = %+v", flashOf(t, rec))
	}

	form = url.Values{}
	form["ids"] = []string{"tagged-one"}
	form.Set("tag_names", "added, existing")
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_add_tags", form, session)
	if flash := flashOf(t, rec); !strings.Contains(flash.Notice, "成功添加标签到 1 篇文章。") {
		t.Fatalf("batch_add_tags notice = %q", flash.Notice)
	}
	article, _ := s.Q.GetAdminArticleBySlug(ctx, nullSlug("tagged-one"))
	tags, err := s.Q.ListArticleTagNames(ctx, []int64{article.ID})
	if err != nil || len(tags) != 2 {
		t.Fatalf("tags = %+v, %v; want existing + added", tags, err)
	}
	names := map[string]bool{tags[0].Name: true, tags[1].Name: true}
	if !names["existing"] || !names["added"] {
		t.Errorf("tags = %+v, want existing kept and added appended", tags)
	}
}

// TestAdminArticlesBatchCrosspost covers batch_crosspost: validation alerts,
// the publish requirement, and per-platform enqueueing.
func TestAdminArticlesBatchCrosspost(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	enableCrosspost(t, s, "mastodon")

	insertAdminArticle(t, s, "Pub", "pub", domain.StatusPublish)
	insertAdminArticle(t, s, "Drft", "drft", domain.StatusDraft)

	// Validation alerts.
	rec := doRequest(t, h, http.MethodPost, "/admin/posts/batch_crosspost", url.Values{"platforms": {"mastodon"}}, session)
	if flashOf(t, rec).Alert != "请至少选择一个文章。" {
		t.Errorf("empty ids alert = %+v", flashOf(t, rec))
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_crosspost", url.Values{"ids": {"pub"}}, session)
	if flashOf(t, rec).Alert != "请至少选择一个平台。" {
		t.Errorf("empty platforms alert = %+v", flashOf(t, rec))
	}

	// A disabled platform queues nothing but does not error (Rails:
	// jobs_queued stays false, count unchanged).
	form := url.Values{}
	form["ids"] = []string{"pub"}
	form["platforms"] = []string{"bluesky"}
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_crosspost", form, session)
	if flash := flashOf(t, rec); !strings.Contains(flash.Notice, "成功提交 0 篇文章进行跨平台发布。") {
		t.Errorf("disabled platform notice = %q", flash.Notice)
	}
	if n := len(queuedJobs(t, s, jobs.KindCrosspost)); n != 0 {
		t.Errorf("jobs for disabled platform = %d, want 0", n)
	}

	// Published article + enabled platform queues one job; the draft errors.
	form["ids"] = []string{"pub", "drft"}
	form["platforms"] = []string{"mastodon"}
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_crosspost", form, session)
	flash := flashOf(t, rec)
	if !strings.Contains(flash.Alert, "成功提交 1 篇文章进行跨平台发布。") || !strings.Contains(flash.Alert, "文章未发布，无法进行跨平台发布") {
		t.Errorf("mixed batch alert = %q", flash.Alert)
	}
	jobs_ := queuedJobs(t, s, jobs.KindCrosspost)
	if len(jobs_) != 1 || jobs_[0]["platform"] != "mastodon" || jobs_[0]["requested_at"] == nil {
		t.Errorf("crosspost jobs = %+v", jobs_)
	}
}

// TestAdminArticlesBatchNewsletter covers batch_newsletter: the publish
// requirement and the enabled+configured gate.
func TestAdminArticlesBatchNewsletter(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)

	insertAdminArticle(t, s, "Pub", "pub", domain.StatusPublish)
	insertAdminArticle(t, s, "Drft", "drft", domain.StatusDraft)

	rec := doRequest(t, h, http.MethodPost, "/admin/posts/batch_newsletter", nil, session)
	if flashOf(t, rec).Alert != "请至少选择一个文章。" {
		t.Errorf("empty ids alert = %+v", flashOf(t, rec))
	}

	// Not configured: every published article errors.
	form := url.Values{}
	form["ids"] = []string{"pub", "drft"}
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_newsletter", form, session)
	flash := flashOf(t, rec)
	if !strings.Contains(flash.Alert, "Newsletter未配置或未启用") || !strings.Contains(flash.Alert, "文章未发布，无法发送邮件") {
		t.Errorf("not-configured alert = %q", flash.Alert)
	}
	if n := len(queuedJobs(t, s, jobs.KindSendNewsletter)); n != 0 {
		t.Errorf("newsletter jobs = %d, want 0", n)
	}

	// Enabled + configured: the published article queues, the draft errors.
	enableNativeNewsletter(t, s)
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/batch_newsletter", form, session)
	flash = flashOf(t, rec)
	if !strings.Contains(flash.Alert, "成功提交 1 篇文章发送邮件。") {
		t.Errorf("partial alert = %q", flash.Alert)
	}
	jobs_ := queuedJobs(t, s, jobs.KindSendNewsletter)
	if len(jobs_) != 1 {
		t.Errorf("newsletter jobs = %+v, want 1", jobs_)
	}
}

// TestAdminArticlesFetchComments covers the JSON endpoint: 404, the empty
// 422, per-post enqueueing, and the platform filter.
func TestAdminArticlesFetchComments(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	ctx := t.Context()

	article := insertAdminArticle(t, s, "Social", "social", domain.StatusPublish)

	// No social posts: 422 JSON.
	rec := doRequest(t, h, http.MethodPost, "/admin/posts/social/fetch_comments", nil, session)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no posts: status = %d, want 422", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["success"] != false {
		t.Errorf("no posts body = %q", rec.Body.String())
	}

	now := time.Now().Unix()
	for _, post := range []struct{ platform, url string }{
		{"mastodon", "https://mastodon.social/@me/1"},
		{"bluesky", "https://bsky.app/p/2"},
		{"xiaohongshu", "https://xhs.link/3"}, // no fetcher: skipped like Rails
	} {
		if _, err := s.DB.ExecContext(ctx,
			`INSERT INTO social_media_posts (article_id, platform, url, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			article.ID, post.platform, post.url, now, now); err != nil {
			t.Fatalf("insert social post: %v", err)
		}
	}

	// All posts: one job per fetchable platform.
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/social/fetch_comments", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch all: status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["success"] != true {
		t.Errorf("fetch all body = %q", rec.Body.String())
	}
	jobs_ := queuedJobs(t, s, jobs.KindFetchSocialComments)
	if len(jobs_) != 2 {
		t.Fatalf("fetch jobs = %+v, want mastodon + bluesky", jobs_)
	}
	platforms := map[string]bool{}
	for _, j := range jobs_ {
		platforms[j["platform"].(string)] = true
		if int64(j["article_id"].(float64)) != article.ID {
			t.Errorf("job article_id = %v, want %d", j["article_id"], article.ID)
		}
	}
	if !platforms["mastodon"] || !platforms["bluesky"] {
		t.Errorf("job platforms = %v", platforms)
	}

	// Platform filter narrows the fetch.
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/social/fetch_comments", url.Values{"platform": {"bluesky"}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch bluesky: status = %d", rec.Code)
	}
	if n := len(queuedJobs(t, s, jobs.KindFetchSocialComments)); n != 3 {
		t.Errorf("jobs after filtered fetch = %d, want 3 (2 + 1)", n)
	}

	// A platform with no posts is the 422 branch.
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/social/fetch_comments", url.Values{"platform": {"twitter"}}, session)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("fetch twitter: status = %d, want 422", rec.Code)
	}
}
