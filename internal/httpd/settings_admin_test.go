package httpd

import (
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
	"rables/internal/templates"
)

// newSettingsTestServer builds a Server and mounts only the settings routes,
// independent of the integrator's router wiring.
func newSettingsTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterSettingsRoutes(r, s)
	return s, r
}

// settingsSession inserts a user plus session row directly and returns the
// session cookie, skipping the login dance.
func settingsSession(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	ctx := t.Context()
	now := time.Now().Unix()
	user, err := s.Q.CreateUser(ctx, query.CreateUserParams{
		UserName: "admin", PasswordDigest: "x", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.Q.CreateSession(ctx, query.CreateSessionParams{
		Token: "settings-test-token", UserID: user.ID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: "settings-test-token"}
}

// markSetupCompleted flips the singleton flag like a finished setup.
func markSetupCompleted(t *testing.T, s *Server) {
	t.Helper()
	now := time.Now().Unix()
	if err := s.Q.EnsureSettings(t.Context(), query.EnsureSettingsParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	if _, err := s.DB.ExecContext(t.Context(), "UPDATE settings SET setup_completed = 1 WHERE id = 1"); err != nil {
		t.Fatalf("mark setup completed: %v", err)
	}
	s.Settings().Invalidate()
	s.InvalidateSetupCache()
}

func settingsForm() url.Values {
	return url.Values{
		"title":             {"My Blog"},
		"description":       {"desc"},
		"author":            {"author"},
		"url":               {"https://blog.example.com"},
		"time_zone":         {"UTC"},
		"head_code":         {"<meta name=\"x\">"},
		"custom_css":        {"body{}"},
		"tool_code":         {""},
		"giscus":            {""},
		"social_links_json": {`{"rss":{"url":"/feed.rss","icon":"fa-solid fa-square-rss"},"github":{"url":"https://github.com/versun","icon":"fa-brands fa-github"}}`},
	}
}

// TestSettingsRequiresAuth: the admin settings pages bounce anonymous users.
func TestSettingsRequiresAuth(t *testing.T) {
	_, h := newSettingsTestServer(t)

	rec := doRequest(t, h, http.MethodGet, "/admin/setting/edit", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
		t.Errorf("GET edit: status = %d location = %q, want 302 /session/new", rec.Code, rec.Header().Get("Location"))
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/setting", settingsForm())
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
		t.Errorf("POST update: status = %d location = %q, want 302 /session/new", rec.Code, rec.Header().Get("Location"))
	}
}

// TestSettingsEditShowsCurrentValues: GET edit renders the stored row.
func TestSettingsEditShowsCurrentValues(t *testing.T) {
	s, h := newSettingsTestServer(t)
	session := settingsSession(t, s)

	rec := doRequest(t, h, http.MethodGet, "/admin/setting/edit", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET edit: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `name="time_zone" value="UTC"`) {
		t.Error("fresh row does not default time_zone to UTC")
	}

	markSetupCompleted(t, s)
	form := settingsForm()
	form.Set("title", "Existing Title")
	rec = doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("seed update: status = %d, want %d", rec.Code, http.StatusFound)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/setting/edit", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET edit: status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="title" value="Existing Title"`,
		`name="url" value="https://blog.example.com"`,
		`&lt;meta name=&#34;x&#34;&gt;`,                      // head_code textarea, escaped
		`&#34;url&#34;: &#34;https://github.com/versun&#34;`, // pretty-printed social links
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit page missing %q", want)
		}
	}
}

// TestSettingsUpdateThenTitleChanges is the handler-level DoD: after a
// successful update the redirected page shows the new title and the flash.
func TestSettingsUpdateThenTitleChanges(t *testing.T) {
	s, h := newSettingsTestServer(t)
	session := settingsSession(t, s)
	markSetupCompleted(t, s)

	form := settingsForm()
	form.Set("title", "Renamed Blog")
	form.Set("time_zone", "Asia/Shanghai")
	rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/setting/edit" {
		t.Fatalf("update: status = %d location = %q, want 302 /admin/setting/edit", rec.Code, rec.Header().Get("Location"))
	}
	flash := findCookie(rec, flashCookieName)
	if flash == nil {
		t.Fatal("update did not set a flash cookie")
	}

	// Follow the redirect like a browser would.
	rec = doRequest(t, h, http.MethodGet, "/admin/setting/edit", nil, session, flash)
	body := rec.Body.String()
	if !strings.Contains(body, "Setting was successfully updated.") {
		t.Error("flash notice not shown after redirect")
	}
	if !strings.Contains(body, `name="title" value="Renamed Blog"`) {
		t.Error("edit page does not show the new title after update")
	}
	if !strings.Contains(body, `name="time_zone" value="Asia/Shanghai"`) {
		t.Error("edit page does not show the new time zone after update")
	}

	// The stored row matches, with social_links compacted and the submitted
	// entry order preserved (never sorted alphabetically).
	st, err := s.Q.GetSettings(t.Context())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if st.Title.String != "Renamed Blog" || st.TimeZone != "Asia/Shanghai" {
		t.Errorf("stored row: title = %q time_zone = %q", st.Title.String, st.TimeZone)
	}
	if want := `{"rss":{"url":"/feed.rss","icon":"fa-solid fa-square-rss"},"github":{"url":"https://github.com/versun","icon":"fa-brands fa-github"}}`; st.SocialLinks.String != want {
		t.Errorf("stored social_links = %q, want %q", st.SocialLinks.String, want)
	}
}

// TestSettingsUpdateValidation covers the failure branches of the update.
func TestSettingsUpdateValidation(t *testing.T) {
	newServer := func(t *testing.T, setupCompleted bool) (*Server, http.Handler, *http.Cookie) {
		s, h := newSettingsTestServer(t)
		session := settingsSession(t, s)
		if setupCompleted {
			markSetupCompleted(t, s)
		}
		return s, h, session
	}

	t.Run("invalid social links JSON is rejected with 422", func(t *testing.T) {
		s, h, session := newServer(t, true)
		form := settingsForm()
		form.Set("social_links_json", "{nope")
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Social links JSON is invalid") {
			t.Error("alert not shown")
		}
		if !strings.Contains(rec.Body.String(), "invalid character") {
			t.Error("alert does not include the JSON syntax error detail")
		}
		if !strings.Contains(rec.Body.String(), "{nope") {
			t.Error("submitted JSON not echoed back for fixing")
		}
		st, err := s.Q.GetSettings(t.Context())
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		if st.Title.String == "My Blog" {
			t.Error("row was written despite the invalid social links JSON")
		}
	})

	t.Run("malformed social link entries are rejected with 422", func(t *testing.T) {
		s, h, session := newServer(t, true)
		form := settingsForm()
		form.Set("social_links_json", `{"github":"https://github.com/versun"}`)
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Social links JSON is invalid") {
			t.Error("alert not shown")
		}
		if !strings.Contains(rec.Body.String(), "invalid social link") {
			t.Error("alert does not include the malformed entry detail")
		}
		if strings.Contains(rec.Body.String(), "not a JSON object") {
			t.Error("alert blames the top-level shape for an entry-level failure")
		}
		st, err := s.Q.GetSettings(t.Context())
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		if st.SocialLinks.String != "" {
			t.Errorf("malformed social links were stored: %q", st.SocialLinks.String)
		}
	})

	t.Run("null social link entry is rejected with 422", func(t *testing.T) {
		_, h, session := newServer(t, true)
		form := settingsForm()
		form.Set("social_links_json", `{"github":null}`)
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Social links JSON is invalid") {
			t.Error("alert not shown")
		}
		if !strings.Contains(rec.Body.String(), "must be an object") {
			t.Error("alert does not include the null entry detail")
		}
	})

	t.Run("blank social links keeps the stored value", func(t *testing.T) {
		s, h, session := newServer(t, true)
		form := settingsForm()
		if rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session); rec.Code != http.StatusFound {
			t.Fatalf("seed update: status = %d", rec.Code)
		}
		form.Set("social_links_json", "")
		form.Set("title", "Other Title")
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		st, err := s.Q.GetSettings(t.Context())
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		if st.SocialLinks.String == "" {
			t.Error("blank social_links_json cleared the stored links; Rails keeps them")
		}
		if st.Title.String != "Other Title" {
			t.Errorf("title = %q, want Other Title", st.Title.String)
		}
	})

	t.Run("blank url rejected once setup completed", func(t *testing.T) {
		_, h, session := newServer(t, true)
		form := settingsForm()
		form.Set("url", "")
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Url can&#39;t be blank.") {
			t.Error("url alert not shown")
		}
	})

	t.Run("blank url allowed before setup completed", func(t *testing.T) {
		_, h, session := newServer(t, false)
		form := settingsForm()
		form.Set("url", "")
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
	})

	t.Run("empty time zone falls back to UTC", func(t *testing.T) {
		s, h, session := newServer(t, true)
		form := settingsForm()
		form.Set("time_zone", "")
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		st, err := s.Q.GetSettings(t.Context())
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		if st.TimeZone != "UTC" {
			t.Errorf("time_zone = %q, want UTC", st.TimeZone)
		}
	})

	t.Run("unknown time zone stored as-is like Rails", func(t *testing.T) {
		s, h, session := newServer(t, true)
		form := settingsForm()
		form.Set("time_zone", "Not/AZone")
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		st, err := s.Q.GetSettings(t.Context())
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		if st.TimeZone != "Not/AZone" {
			t.Errorf("time_zone = %q, want Not/AZone (Rails validates nothing)", st.TimeZone)
		}
	})
}

// TestSettingsUpdateArticleRoutePrefix covers the route prefix field: a valid
// prefix is stored and editable, slashes are trimmed, and reserved or
// URL-unsafe values are rejected with a 422.
func TestSettingsUpdateArticleRoutePrefix(t *testing.T) {
	t.Run("valid prefix is stored", func(t *testing.T) {
		s, h := newSettingsTestServer(t)
		session := settingsSession(t, s)
		markSetupCompleted(t, s)

		form := settingsForm()
		form.Set("article_route_prefix", "blog")
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		st, err := s.Q.GetSettings(t.Context())
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		if st.ArticleRoutePrefix.String != "blog" {
			t.Errorf("article_route_prefix = %q, want blog", st.ArticleRoutePrefix.String)
		}

		rec = doRequest(t, h, http.MethodGet, "/admin/setting/edit", nil, session)
		if !strings.Contains(rec.Body.String(), `name="article_route_prefix" value="blog"`) {
			t.Error("edit page does not show the stored prefix")
		}
	})

	t.Run("slashes are trimmed", func(t *testing.T) {
		s, h := newSettingsTestServer(t)
		session := settingsSession(t, s)
		markSetupCompleted(t, s)

		form := settingsForm()
		form.Set("article_route_prefix", "/blog/")
		rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		st, err := s.Q.GetSettings(t.Context())
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		if st.ArticleRoutePrefix.String != "blog" {
			t.Errorf("article_route_prefix = %q, want blog", st.ArticleRoutePrefix.String)
		}
	})

	t.Run("reserved or unsafe prefixes are rejected with 422", func(t *testing.T) {
		for _, bad := range []string{"admin", "tags", "twitter", "a/b", "a b", "a.b"} {
			s, h := newSettingsTestServer(t)
			session := settingsSession(t, s)
			markSetupCompleted(t, s)

			form := settingsForm()
			form.Set("article_route_prefix", bad)
			rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("prefix %q: status = %d, want 422", bad, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "Article route prefix is invalid") {
				t.Errorf("prefix %q: alert not shown", bad)
			}
			st, err := s.Q.GetSettings(t.Context())
			if err != nil {
				t.Fatalf("get settings: %v", err)
			}
			if st.ArticleRoutePrefix.Valid {
				t.Errorf("prefix %q was stored despite being invalid", bad)
			}
		}
	})
}

// TestSettingsUpdateFeedAllArticles covers the feed size checkbox: checked
// stores 1, unchecked (hidden "0" only) stores 0, and the edit page reflects
// the stored state.
func TestSettingsUpdateFeedAllArticles(t *testing.T) {
	s, h := newSettingsTestServer(t)
	session := settingsSession(t, s)
	markSetupCompleted(t, s)

	form := settingsForm()
	form["feed_all_articles"] = []string{"0", "1"} // Rails hidden field + checkbox
	rec := doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	st, err := s.Q.GetSettings(t.Context())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if st.FeedAllArticles != 1 {
		t.Errorf("feed_all_articles = %d, want 1", st.FeedAllArticles)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/setting/edit", nil, session)
	if !strings.Contains(rec.Body.String(), `name="feed_all_articles" value="1" checked`) {
		t.Error("edit page does not show the stored checkbox state")
	}

	// Unchecked: the browser submits only the hidden "0".
	form = settingsForm()
	form.Set("feed_all_articles", "0")
	rec = doRequest(t, h, http.MethodPost, "/admin/setting", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	st, err = s.Q.GetSettings(t.Context())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if st.FeedAllArticles != 0 {
		t.Errorf("feed_all_articles = %d, want 0", st.FeedAllArticles)
	}
}

// TestSettingsSharedAccessor: Settings() returns the same cache instance and
// writes through it are visible to reads (the T12 integration contract).
func TestSettingsSharedAccessor(t *testing.T) {
	s, _ := newSettingsTestServer(t)
	first, second := s.Settings(), s.Settings()
	if first != second {
		t.Error("Settings() did not return the shared instance")
	}
	if _, err := first.Get(t.Context()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := s.DB.ExecContext(t.Context(), "UPDATE settings SET title = 'bypassed' WHERE id = 1"); err != nil {
		t.Fatalf("direct update: %v", err)
	}
	first.Invalidate()
	st, err := second.Get(t.Context())
	if err != nil {
		t.Fatalf("Get after Invalidate: %v", err)
	}
	if st.Title.String != "bypassed" {
		t.Errorf("title = %q, want bypassed", st.Title.String)
	}
}
