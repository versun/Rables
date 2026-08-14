package httpd

import (
	"database/sql"
	"encoding/base64"
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
	"rables/internal/templates"
)

// newPublicTestServer builds a Server with only the public routes mounted, in
// the order the integrator must use (RegisterPublicRoutes before
// RegisterArticleRoutes so static paths beat the /{slug} catch-all).
func newPublicTestServer(t *testing.T, routePrefix string) (*Server, http.Handler) {
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
	cfg := config.Config{Addr: ":8080", HMACSecret: "x", ArticleRoutePrefix: routePrefix}
	s := NewServer(database, cfg, logger, renderer)
	r := chi.NewRouter()
	RegisterPublicRoutes(r, s)
	RegisterArticleRoutes(r, s)
	return s, r
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type seedArticleOpts struct {
	slug, title, content, description, excerpt string
	sourceAuthor, sourceURL, sourceContent     string
	status, comment                            int64
	createdAt, updatedAt                       int64
}

func seedArticle(t *testing.T, s *Server, o seedArticleOpts) int64 {
	t.Helper()
	if o.createdAt == 0 {
		o.createdAt = 1000
	}
	if o.updatedAt == 0 {
		o.updatedAt = o.createdAt
	}
	res, err := s.DB.Exec(`INSERT INTO articles
		(title, slug, content_html, description, excerpt, source_author, source_url, source_content, status, comment, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullStr(o.title), nullStr(o.slug), nullStr(o.content), nullStr(o.description), nullStr(o.excerpt),
		nullStr(o.sourceAuthor), nullStr(o.sourceURL), nullStr(o.sourceContent),
		o.status, o.comment, o.createdAt, o.updatedAt)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

type seedPageOpts struct {
	slug, title, content, redirectURL string
	status, comment                   int64
}

func seedPage(t *testing.T, s *Server, o seedPageOpts) int64 {
	t.Helper()
	res, err := s.DB.Exec(`INSERT INTO pages (title, slug, content_html, redirect_url, status, comment, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1000, 1000)`,
		nullStr(o.title), nullStr(o.slug), nullStr(o.content), nullStr(o.redirectURL), o.status, o.comment)
	if err != nil {
		t.Fatalf("insert page: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedTag(t *testing.T, s *Server, name, slug string) int64 {
	t.Helper()
	res, err := s.DB.Exec(`INSERT INTO tags (name, slug, created_at, updated_at) VALUES (?, ?, 1000, 1000)`, name, slug)
	if err != nil {
		t.Fatalf("insert tag: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func tagArticle(t *testing.T, s *Server, articleID, tagID int64) {
	t.Helper()
	if _, err := s.DB.Exec(`INSERT INTO article_tags (article_id, tag_id, created_at, updated_at) VALUES (?, ?, 1000, 1000)`, articleID, tagID); err != nil {
		t.Fatalf("insert article_tag: %v", err)
	}
}

// seedSession creates a user plus session row directly and returns the cookie.
func seedSession(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	if _, err := s.DB.Exec(`INSERT INTO users (user_name, password_digest, created_at, updated_at) VALUES ('admin', 'x', 1, 1)`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	now := time.Now().Unix()
	if _, err := s.DB.Exec(`INSERT INTO sessions (token, user_id, created_at, updated_at) VALUES ('tok-public-test', 1, ?, ?)`, now, now); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: "tok-public-test"}
}

// setSiteURL configures settings.url (the settings row is created lazily).
func setSiteURL(t *testing.T, s *Server, rawURL string) {
	t.Helper()
	if _, err := s.Settings().Get(t.Context()); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	if _, err := s.DB.Exec(`UPDATE settings SET url = ? WHERE id = 1`, rawURL); err != nil {
		t.Fatalf("set site url: %v", err)
	}
	s.Settings().Invalidate()
}

func get(t *testing.T, h http.Handler, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodGet, target, nil, cookies...)
}

func countListItems(body string) int { return strings.Count(body, `class="timeline-item"`) }

// TestPublicVisibilityMatrix walks plan section 4.1 cell by cell.
func TestPublicVisibilityMatrix(t *testing.T) {
	statuses := []struct {
		name       string
		status     domain.Status
		anonShow   int
		loggedShow int
		listed     bool // index / feed / sitemap / tag page
	}{
		{"publish", domain.StatusPublish, http.StatusOK, http.StatusOK, true},
		{"shared", domain.StatusShared, http.StatusOK, http.StatusOK, false},
		{"draft", domain.StatusDraft, http.StatusNotFound, http.StatusOK, false},
		{"schedule", domain.StatusSchedule, http.StatusNotFound, http.StatusOK, false},
		{"trash", domain.StatusTrash, http.StatusNotFound, http.StatusOK, false},
	}

	s, h := newPublicTestServer(t, "")
	session := seedSession(t, s)
	tagID := seedTag(t, s, "Go", "go")
	for _, st := range statuses {
		id := seedArticle(t, s, seedArticleOpts{slug: "a-" + st.name, title: "Title " + st.name, status: int64(st.status)})
		tagArticle(t, s, id, tagID)
	}
	setSiteURL(t, s, "https://blog.example.com")

	for _, st := range statuses {
		t.Run("show/"+st.name, func(t *testing.T) {
			if rec := get(t, h, "/a-"+st.name); rec.Code != st.anonShow {
				t.Errorf("anon: status = %d, want %d", rec.Code, st.anonShow)
			}
			if rec := get(t, h, "/a-"+st.name, session); rec.Code != st.loggedShow {
				t.Errorf("logged in: status = %d, want %d", rec.Code, st.loggedShow)
			}
		})
	}

	t.Run("index lists only publish", func(t *testing.T) {
		body := get(t, h, "/").Body.String()
		for _, st := range statuses {
			if strings.Contains(body, `href="/a-`+st.name+`"`) != st.listed {
				t.Errorf("index contains /a-%s: want listed=%v", st.name, st.listed)
			}
		}
	})

	t.Run("feed lists only publish", func(t *testing.T) {
		body := get(t, h, "/feed.xml").Body.String()
		for _, st := range statuses {
			if strings.Contains(body, "/a-"+st.name) != st.listed {
				t.Errorf("feed contains a-%s: want listed=%v", st.name, st.listed)
			}
		}
	})

	t.Run("sitemap lists only publish", func(t *testing.T) {
		body := get(t, h, "/sitemap.xml").Body.String()
		for _, st := range statuses {
			if strings.Contains(body, "/a-"+st.name) != st.listed {
				t.Errorf("sitemap contains a-%s: want listed=%v", st.name, st.listed)
			}
		}
	})

	t.Run("tag page lists only publish", func(t *testing.T) {
		body := get(t, h, "/tags/go").Body.String()
		for _, st := range statuses {
			if strings.Contains(body, `href="/a-`+st.name+`"`) != st.listed {
				t.Errorf("tag page contains /a-%s: want listed=%v", st.name, st.listed)
			}
		}
	})

	t.Run("unknown slug is 404", func(t *testing.T) {
		if rec := get(t, h, "/no-such-article"); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

// TestPublicPageVisibility covers pages/show with the same status rules.
func TestPublicPageVisibility(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	session := seedSession(t, s)
	seedPage(t, s, seedPageOpts{slug: "p-pub", title: "Pub", status: int64(domain.StatusPublish), content: "<p>page body</p>"})
	seedPage(t, s, seedPageOpts{slug: "p-shared", title: "Shared", status: int64(domain.StatusShared), content: "<p>shared body</p>"})
	seedPage(t, s, seedPageOpts{slug: "p-draft", title: "Draft", status: int64(domain.StatusDraft), content: "<p>draft body</p>"})

	tests := []struct {
		name     string
		target   string
		cookies  []*http.Cookie
		wantCode int
		wantBody string
	}{
		{"publish anon", "/pages/p-pub", nil, http.StatusOK, "page body"},
		{"shared anon", "/pages/p-shared", nil, http.StatusOK, "shared body"},
		{"draft anon", "/pages/p-draft", nil, http.StatusNotFound, ""},
		{"draft logged in", "/pages/p-draft", []*http.Cookie{session}, http.StatusOK, "draft body"},
		{"unknown page", "/pages/nope", nil, http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, h, tt.target, tt.cookies...)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body missing %q", tt.wantBody)
			}
		})
	}

	t.Run("redirect_url answers 302", func(t *testing.T) {
		seedPage(t, s, seedPageOpts{slug: "p-redir", title: "Redir", status: int64(domain.StatusPublish), redirectURL: "https://elsewhere.example.com/x"})
		rec := get(t, h, "/pages/p-redir")
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "https://elsewhere.example.com/x" {
			t.Errorf("Location = %q", loc)
		}
	})

	t.Run("draft redirect page stays 404 for anon", func(t *testing.T) {
		seedPage(t, s, seedPageOpts{slug: "p-redir-draft", title: "Redir", status: int64(domain.StatusDraft), redirectURL: "https://elsewhere.example.com/y"})
		if rec := get(t, h, "/pages/p-redir-draft"); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

// TestPublicIndexPagination covers 10-per-page and invalid page numbers.
func TestPublicIndexPagination(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	for i := int64(1); i <= 11; i++ {
		seedArticle(t, s, seedArticleOpts{slug: "post-" + pad2(i), title: "Post " + pad2(i), status: int64(domain.StatusPublish), createdAt: i})
	}

	t.Run("page 1 shows newest 10", func(t *testing.T) {
		body := get(t, h, "/").Body.String()
		if n := countListItems(body); n != 10 {
			t.Errorf("page 1 items = %d, want 10", n)
		}
		if strings.Contains(body, "/post-01") {
			t.Error("page 1 contains the oldest article")
		}
		if !strings.Contains(body, "?page=2") {
			t.Error("page 1 has no next-page link")
		}
	})

	t.Run("page 2 shows the rest", func(t *testing.T) {
		body := get(t, h, "/?page=2").Body.String()
		if n := countListItems(body); n != 1 {
			t.Errorf("page 2 items = %d, want 1", n)
		}
		if !strings.Contains(body, "/post-01") {
			t.Error("page 2 missing the oldest article")
		}
	})

	t.Run("invalid pages 404", func(t *testing.T) {
		// The huge value overflows int64 parsing, so rubyToI saturates past
		// maxPageNumber and the range check must 404 it, not clamp it.
		for _, target := range []string{"/?page=abc", "/?page=0", "/?page=-2", "/?page=99999999999999999999"} {
			if rec := get(t, h, target); rec.Code != http.StatusNotFound {
				t.Errorf("GET %s: status = %d, want 404", target, rec.Code)
			}
		}
	})

	t.Run("to_i semantics: partial number parses", func(t *testing.T) {
		// Ruby "2.5".to_i == 2, so this is page 2, not an error.
		if rec := get(t, h, "/?page=2.5"); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("out of range page renders empty", func(t *testing.T) {
		rec := get(t, h, "/?page=99")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (will_paginate only 404s invalid numbers)", rec.Code)
		}
		if n := countListItems(rec.Body.String()); n != 0 {
			t.Errorf("items = %d, want 0", n)
		}
	})
}

func pad2(n int64) string { return fmt.Sprintf("%02d", n) }

// TestPublicIndexCacheControl: the index is publicly cacheable, but a
// response that renders a one-time flash must degrade to private, no-cache
// so shared caches never store per-user content.
func TestPublicIndexCacheControl(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedArticle(t, s, seedArticleOpts{slug: "post-01", title: "Post 01", status: int64(domain.StatusPublish), createdAt: 1})

	rec := get(t, h, "/")
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300, s-maxage=900" {
		t.Errorf("Cache-Control = %q, want public, max-age=300, s-maxage=900", cc)
	}

	cookie := signedFlashCookie(t, s, templates.Flash{Notice: "Your comment will be reviewed before being published."})
	rec = get(t, h, "/", cookie)
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("Cache-Control with flash = %q, want private, no-cache", cc)
	}
	if !strings.Contains(rec.Body.String(), "flash-notice") {
		t.Error("flash notice not rendered on the index page")
	}
}

// TestPublicIndexCacheControlMalformedFlash: a flash cookie that fails to
// decode pops a zero Flash, yet PopFlash still emits the clearing Set-Cookie
// — so the response must degrade to private, no-cache on cookie presence
// alone, or a shared cache could store a response carrying Set-Cookie.
func TestPublicIndexCacheControlMalformedFlash(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedArticle(t, s, seedArticleOpts{slug: "post-01", title: "Post 01", status: int64(domain.StatusPublish), createdAt: 1})

	values := map[string]string{
		"invalid base64": "!!!not-base64!!!",
		"invalid json":   base64.RawURLEncoding.EncodeToString([]byte("{not json")),
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			rec := get(t, h, "/", &http.Cookie{Name: flashCookieName, Value: value})
			if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache" {
				t.Errorf("Cache-Control = %q, want private, no-cache", cc)
			}
			if cleared := findCookie(rec, flashCookieName); cleared == nil || cleared.Value != "" {
				t.Errorf("flash cookie not cleared: %+v", cleared)
			}
		})
	}
}

// TestPublicShowCacheControlFlash: like the index, an article/page show
// response that renders a one-time flash must degrade to private, no-cache
// instead of its long max-age — otherwise the cached copy keeps re-showing
// a stale flash after the flash cookie is gone.
func TestPublicShowCacheControlFlash(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedArticle(t, s, seedArticleOpts{slug: "post-01", title: "Post 01", status: int64(domain.StatusPublish), createdAt: 1})
	seedPage(t, s, seedPageOpts{slug: "about", title: "About", status: int64(domain.StatusPublish)})

	flashCookie := signedFlashCookie(t, s, templates.Flash{Notice: "Your comment will be reviewed before being published."})

	for _, tc := range []struct{ path, wantCacheable string }{
		{"/post-01", "private, max-age=3600"},
		{"/pages/about", "private, max-age=86400"},
	} {
		if cc := get(t, h, tc.path).Header().Get("Cache-Control"); cc != tc.wantCacheable {
			t.Errorf("GET %s Cache-Control = %q, want %q", tc.path, cc, tc.wantCacheable)
		}
		rec := get(t, h, tc.path, flashCookie)
		if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache" {
			t.Errorf("GET %s Cache-Control with flash = %q, want private, no-cache", tc.path, cc)
		}
		if !strings.Contains(rec.Body.String(), "flash-notice") {
			t.Errorf("GET %s: flash notice not rendered", tc.path)
		}
	}
}

// TestPublicShowPercentEncodedSlug: when the client's escaping differs from
// Go's canonical form (lowercase hex here), net/url keeps the original bytes
// in URL.RawPath and chi routes on it without decoding — slugParam must
// unescape the parameter in that case, or the DB lookup misses a slug that
// exists (Rails-migrated sites carry CJK slugs).
func TestPublicShowPercentEncodedSlug(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedArticle(t, s, seedArticleOpts{slug: "2024年", title: "Year", status: int64(domain.StatusPublish), createdAt: 1})

	rec := get(t, h, "/2024%e5%b9%b4")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Year") {
		t.Error("article title not rendered")
	}
}

// TestPublicShowErrorNotCached: Cache-Control is set only after every
// fallible query, because http.Error does not clear headers already set — a
// 500 carrying a cacheable header would be stored by a CDN.
func TestPublicShowErrorNotCached(t *testing.T) {
	t.Run("article", func(t *testing.T) {
		s, h := newPublicTestServer(t, "")
		seedArticle(t, s, seedArticleOpts{slug: "post-01", title: "Post 01", status: int64(domain.StatusPublish), createdAt: 1})
		if _, err := s.DB.Exec(`DROP TABLE article_tags`); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		rec := get(t, h, "/post-01")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "" {
			t.Errorf("Cache-Control = %q, want empty on a 500", cc)
		}
	})
	t.Run("page", func(t *testing.T) {
		s, h := newPublicTestServer(t, "")
		seedPage(t, s, seedPageOpts{slug: "about", title: "About", status: int64(domain.StatusPublish), comment: 1})
		if _, err := s.DB.Exec(`DROP TABLE comments`); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		rec := get(t, h, "/pages/about")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "" {
			t.Errorf("Cache-Control = %q, want empty on a 500", cc)
		}
	})
}

// TestPublicTagsCacheControlFlash: like the article index, the tag pages are
// publicly cacheable, but a response that renders a one-time flash must
// degrade to private, no-cache so shared caches never store per-user content.
func TestPublicTagsCacheControlFlash(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedTag(t, s, "Go", "go")

	flashCookie := signedFlashCookie(t, s, templates.Flash{Notice: "Your comment will be reviewed before being published."})

	for _, path := range []string{"/tags", "/tags/go"} {
		if cc := get(t, h, path).Header().Get("Cache-Control"); cc != "public, max-age=300, s-maxage=900" {
			t.Errorf("GET %s Cache-Control = %q, want public, max-age=300, s-maxage=900", path, cc)
		}
		rec := get(t, h, path, flashCookie)
		if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache" {
			t.Errorf("GET %s Cache-Control with flash = %q, want private, no-cache", path, cc)
		}
	}
}

// TestPublicTagPagination covers 20-per-page and strict Integer() parsing.
func TestPublicTagPagination(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	tagID := seedTag(t, s, "Go", "go")
	for i := int64(1); i <= 21; i++ {
		id := seedArticle(t, s, seedArticleOpts{slug: "tp-" + pad2(i), title: "TP " + pad2(i), status: int64(domain.StatusPublish), createdAt: i})
		tagArticle(t, s, id, tagID)
	}

	t.Run("page 1 has 20", func(t *testing.T) {
		if n := countListItems(get(t, h, "/tags/go").Body.String()); n != 20 {
			t.Errorf("items = %d, want 20", n)
		}
	})

	t.Run("page 2 has 1", func(t *testing.T) {
		if n := countListItems(get(t, h, "/tags/go?page=2").Body.String()); n != 1 {
			t.Errorf("items = %d, want 1", n)
		}
	})

	t.Run("strict page parsing 404s", func(t *testing.T) {
		// The huge value overflows int64 parsing, so rubyInteger saturates past
		// maxPageNumber and the range check must 404 it, not clamp it.
		for _, target := range []string{"/tags/go?page=abc", "/tags/go?page=", "/tags/go?page=2.5", "/tags/go?page=0", "/tags/go?page=99999999999999999999"} {
			if rec := get(t, h, target); rec.Code != http.StatusNotFound {
				t.Errorf("GET %s: status = %d, want 404", target, rec.Code)
			}
		}
	})

	t.Run("unknown tag 404", func(t *testing.T) {
		if rec := get(t, h, "/tags/nope"); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("tags index lists tags with published counts", func(t *testing.T) {
		body := get(t, h, "/tags").Body.String()
		if !strings.Contains(body, `href="/tags/go"`) || !strings.Contains(body, "21 articles") {
			t.Errorf("tags index missing tag or count\ngot:\n%s", body)
		}
	})
}

// TestPublicSearch covers Article.search_content escaping (plan section 4.3).
func TestPublicSearch(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedArticle(t, s, seedArticleOpts{slug: "percent", title: "100% coverage", status: 1})
	seedArticle(t, s, seedArticleOpts{slug: "under", title: "under_score", status: 1})
	seedArticle(t, s, seedArticleOpts{slug: "back", title: "plain", content: "<p>back\\slash path</p>", status: 1})
	seedArticle(t, s, seedArticleOpts{slug: "other", title: "nothing special", status: 1})
	seedArticle(t, s, seedArticleOpts{slug: "draft-percent", title: "100% draft", status: 0})

	tests := []struct {
		name    string
		q       string
		wantHit []string
		wantNot []string
	}{
		{"literal percent", "100%", []string{"/percent"}, []string{"/under", "/back", "/other", "/draft-percent"}},
		{"bare percent wildcard escaped", "%", []string{"/percent"}, []string{"/under", "/back", "/other"}},
		{"bare underscore wildcard escaped", "_", []string{"/under"}, []string{"/percent", "/back", "/other"}},
		{"backslash escaped", `\`, []string{"/back"}, []string{"/percent", "/under", "/other"}},
		{"plain substring", "cover", []string{"/percent"}, []string{"/under", "/other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, h, "/?q="+url.QueryEscape(tt.q))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, hit := range tt.wantHit {
				if !strings.Contains(body, `href="`+hit+`"`) {
					t.Errorf("q=%q missing hit %s", tt.q, hit)
				}
			}
			for _, miss := range tt.wantNot {
				if strings.Contains(body, `href="`+miss+`"`) {
					t.Errorf("q=%q unexpectedly contains %s", tt.q, miss)
				}
			}
		})
	}

	t.Run("blank q lists everything", func(t *testing.T) {
		body := get(t, h, "/?q=+").Body.String() // whitespace-only is not "present"
		for _, slug := range []string{"/percent", "/under", "/back", "/other"} {
			if !strings.Contains(body, `href="`+slug+`"`) {
				t.Errorf("blank search missing %s", slug)
			}
		}
	})
}

// TestPublicSearchTruncatesLongQuery covers the ?q= length cap: the term is
// cut to publicSearchMaxRunes runes (without splitting a multi-byte
// character) before the LIKE pattern is built, so an overlong term behaves
// exactly like its truncation.
func TestPublicSearchTruncatesLongQuery(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedArticle(t, s, seedArticleOpts{slug: "long", content: "<p>" + strings.Repeat("a", 300) + "</p>", status: 1})

	t.Run("overlong q matches its truncation", func(t *testing.T) {
		// The full term cannot match (the content has no trailing "z"s), but
		// its 200-rune truncation does.
		long := strings.Repeat("a", 300) + "zzz"
		rec := get(t, h, "/?q="+url.QueryEscape(long))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `href="/long"`) {
			t.Errorf("overlong search did not behave like its truncation")
		}
	})

	t.Run("multi-byte character at the cap stays whole", func(t *testing.T) {
		mixed := strings.Repeat("中", publicSearchMaxRunes) + "文extra"
		want := get(t, h, "/?q="+url.QueryEscape(strings.Repeat("中", publicSearchMaxRunes)))
		got := get(t, h, "/?q="+url.QueryEscape(mixed))
		if got.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", got.Code)
		}
		if got.Body.String() != want.Body.String() {
			t.Errorf("overlong multi-byte search differs from its truncation")
		}
	})
}
func TestPublicCommentsSection(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	artID := seedArticle(t, s, seedArticleOpts{slug: "with-comments", title: "WC", status: 1, comment: 1})
	insertComment(t, s, query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: artID, Valid: true},
		AuthorName:      "Ann", Content: "approved hello",
		Status:      int64(domain.CommentApproved),
		PublishedAt: sql.NullInt64{Int64: 500, Valid: true},
	})
	insertComment(t, s, query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: artID, Valid: true},
		AuthorName:      "Bob", Content: "pending secret",
		Status:      int64(domain.CommentPending),
		PublishedAt: sql.NullInt64{Int64: 600, Valid: true},
	})

	t.Run("publish + comment=1 shows approved tree and form", func(t *testing.T) {
		body := get(t, h, "/with-comments").Body.String()
		for _, want := range []string{"approved hello", "Comments (1)", `id="comment-form-container"`, `action="/comments?article_id=with-comments"`, `name="captcha[token]"`} {
			if !strings.Contains(body, want) {
				t.Errorf("show page missing %q", want)
			}
		}
		if strings.Contains(body, "pending secret") {
			t.Error("pending comment leaked into the page")
		}
	})

	t.Run("comment=0 hides the section", func(t *testing.T) {
		seedArticle(t, s, seedArticleOpts{slug: "no-comments", title: "NC", status: 1, comment: 0})
		body := get(t, h, "/no-comments").Body.String()
		if strings.Contains(body, "comment-form-container") || strings.Contains(body, "comments-section") {
			t.Error("comment=0 article renders the comments section")
		}
	})

	t.Run("page comment form uses page_id", func(t *testing.T) {
		seedPage(t, s, seedPageOpts{slug: "about", title: "About", status: 1, comment: 1, content: "<p>about</p>"})
		body := get(t, h, "/pages/about").Body.String()
		if !strings.Contains(body, `action="/comments?page_id=about"`) {
			t.Error("page comment form missing page_id action")
		}
	})
}

// TestRenderCache covers the rendered-content cache (plan section 4.4):
// keyed on id + updated_at, so same-version edits stay cached and an
// updated_at bump re-renders.
func TestRenderCache(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedArticle(t, s, seedArticleOpts{slug: "cached", title: "Cached", status: 1, content: "<p>first version</p>", updatedAt: 100})

	body := get(t, h, "/cached").Body.String()
	if !strings.Contains(body, "first version") {
		t.Fatal("initial render missing content")
	}

	if _, err := s.DB.Exec(`UPDATE articles SET content_html = '<p>second version</p>' WHERE slug = 'cached'`); err != nil {
		t.Fatal(err)
	}
	body = get(t, h, "/cached").Body.String()
	if !strings.Contains(body, "first version") {
		t.Error("cache miss expected: same updated_at must hit the cache")
	}

	if _, err := s.DB.Exec(`UPDATE articles SET updated_at = 200 WHERE slug = 'cached'`); err != nil {
		t.Fatal(err)
	}
	body = get(t, h, "/cached").Body.String()
	if !strings.Contains(body, "second version") {
		t.Error("updated_at bump must invalidate the cached render")
	}

	t.Run("entries expire after the 7-day TTL", func(t *testing.T) {
		now := time.Now()
		cache := newRenderCache()
		cache.now = func() time.Time { return now }
		s.Ext.Store(renderCacheExtKey, cache)
		if got := cache.fetch("article", 1, 1, "<p>v1</p>"); got != "<p>v1</p>" {
			t.Fatalf("first fetch = %q", got)
		}
		if got := cache.fetch("article", 1, 1, "<p>v2</p>"); got != "<p>v1</p>" {
			t.Fatalf("cached fetch = %q, want v1", got)
		}
		cache.now = func() time.Time { return now.Add(renderCacheTTL + time.Hour) }
		if got := cache.fetch("article", 1, 1, "<p>v2</p>"); got != "<p>v2</p>" {
			t.Fatalf("expired fetch = %q, want v2", got)
		}
	})
}

// TestPublicChrome covers the site chrome: head_code/custom_css/tool_code
// injection, giscus, title composition, sidebar (social links, nav pages,
// search box) and the settings time zone on rendered dates.
func TestPublicChrome(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedArticle(t, s, seedArticleOpts{slug: "chromed", title: "Chromed Post", status: 1, comment: 1, createdAt: 1700000000})
	seedPage(t, s, seedPageOpts{slug: "about", title: "About", status: 1})
	seedPage(t, s, seedPageOpts{slug: "elsewhere", title: "Elsewhere", status: 1, redirectURL: "https://elsewhere.example.com/"})
	seedTag(t, s, "Go", "go")

	if _, err := s.Settings().Get(t.Context()); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	_, err := s.DB.Exec(`UPDATE settings SET
		title = 'Example Site', description = 'a test blog', time_zone = 'Asia/Shanghai',
		head_code = '<script data-head="1"></script>', custom_css = '.foo{color:red}',
		tool_code = '<script data-tool="1"></script>', giscus = '<script data-giscus="1"></script>',
		social_links = '{"mastodon":{"url":"https://m.example.com/@a","icon":"fa-brands fa-mastodon"}}'
		WHERE id = 1`)
	if err != nil {
		t.Fatalf("update settings: %v", err)
	}
	s.Settings().Invalidate()

	body := get(t, h, "/chromed").Body.String()
	wants := []string{
		`<title>Chromed Post | Example Site</title>`, // content_for :title composition
		`<script data-head="1"></script>`,            // head_code raw
		`.foo{color:red}`,                            // custom_css in a style tag
		`<script data-tool="1"></script>`,            // tool_code before </body>
		`<script data-giscus="1"></script>`,          // giscus in the comments section
		`<p class="site-description">a test blog</p>`,
		`href="https://m.example.com/@a"`,       // social link
		`href="/pages/about"`,                   // nav page
		`href="https://elsewhere.example.com/"`, // nav redirect page
		`target="_blank" rel="noopener"`,
		`href="/tags"`, // tags nav link (tags exist)
		`name="q"`,     // search box
		`<link rel="alternate" type="application/rss+xml" title="Example Site" href="/feed.xml">`,
		`<span class="article-date">2023-11-15</span>`, // 1700000000 in Asia/Shanghai
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("show page missing %q", want)
		}
	}

	t.Run("index title is the bare site title", func(t *testing.T) {
		body := get(t, h, "/").Body.String()
		if !strings.Contains(body, `<title>Example Site</title>`) {
			t.Error("index <title> mismatch")
		}
	})

	t.Run("index flash renders after comment redirect", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.SetFlash(w, templates.Flash{Notice: "Your comment will be reviewed before being published."})
		flash := findCookie(w, flashCookieName)
		body := get(t, h, "/", flash).Body.String()
		if !strings.Contains(body, "flash-notice") {
			t.Error("flash notice not rendered on the index page")
		}
	})
}

// TestPublicChromeBadSocialLinks covers pre-existing malformed social_links
// rows (stored before the admin form validated entry shapes): the bad entry
// is skipped like Rails instead of failing every public page with a 500.
func TestPublicChromeBadSocialLinks(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	if _, err := s.Settings().Get(t.Context()); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	if _, err := s.DB.Exec(`UPDATE settings SET social_links = '{"github":"https://github.com/versun","rss":{"url":"/feed.rss","icon":"fa-solid fa-square-rss"}}' WHERE id = 1`); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	s.Settings().Invalidate()

	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "https://github.com/versun") {
		t.Error("malformed entry rendered; want it skipped")
	}
	if !strings.Contains(body, `href="/feed.rss"`) {
		t.Error("well-formed entry not rendered")
	}
}

// TestPublicChromeUndecodableSocialLinks covers a social_links row whose top
// level is not valid JSON (copied as-is from an old database by the Rails
// migration): it degrades to no social links instead of a 500 on every
// public page.
func TestPublicChromeUndecodableSocialLinks(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	if _, err := s.Settings().Get(t.Context()); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	if _, err := s.DB.Exec(`UPDATE settings SET social_links = '{nope' WHERE id = 1`); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	s.Settings().Invalidate()

	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestPublicChromeSocialLinksOrder covers the sidebar rendering the social
// links in the stored JSON's entry order, never re-sorted alphabetically.
func TestPublicChromeSocialLinksOrder(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	if _, err := s.Settings().Get(t.Context()); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	if _, err := s.DB.Exec(`UPDATE settings SET social_links = '{"rss":{"url":"https://example.com/feed","icon":"fa-solid fa-square-rss"},"github":{"url":"https://github.com/versun","icon":"fa-brands fa-github"}}' WHERE id = 1`); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	s.Settings().Invalidate()

	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	rss := strings.Index(body, `href="https://example.com/feed"`)
	github := strings.Index(body, `href="https://github.com/versun"`)
	if rss == -1 || github == -1 {
		t.Fatalf("social links not rendered: rss at %d, github at %d", rss, github)
	}
	if rss > github {
		t.Error("social links re-sorted alphabetically; want the stored entry order (rss before github)")
	}
}

// TestBuildSourceReferenceUnsafeURL: a source_url persisted from an
// attacker-controlled import (railsmigrate/transfer) must not become an href
// unless it is an absolute http(s) URL with a host.
func TestBuildSourceReferenceUnsafeURL(t *testing.T) {
	out := string(buildSourceReference("quoted", "javascript:alert(1)"))
	if strings.Contains(out, "<a href=") {
		t.Errorf("javascript: source_url rendered a link: %s", out)
	}
	if !strings.Contains(out, "引用") {
		t.Error("quote label dropped together with the unsafe link")
	}

	out = string(buildSourceReference("quoted", "https://example.com/post"))
	if !strings.Contains(out, `href="https://example.com/post"`) {
		t.Errorf("https source_url not linked: %s", out)
	}
	if !strings.Contains(out, "fa-external-link-alt") {
		t.Errorf("jump icon missing next to the 引用 link: %s", out)
	}
}
