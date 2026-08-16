package twittersync

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/service/media"
)

// testJPEG carries JPEG magic bytes so content sniffing yields image/jpeg.
var testJPEG = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}, make([]byte, 64)...)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// fakeX is a scripted X API + media/redirect server.
type fakeX struct {
	t *testing.T

	userID       string // resolved for any username when non-empty
	pages        [][]map[string]any
	requests     atomic.Int64 // timeline request count
	lastTimeline *url.Values  // query params of the last timeline request
	onTimeline   func()       // optional hook called inside the timeline handler
	extra        http.Handler // media/redirect fixtures mounted last
}

func (f *fakeX) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/by/username/", func(w http.ResponseWriter, r *http.Request) {
		if f.userID == "" {
			// A 200 payload without data.id is the x gem's non-raising "not
			// found" shape (a 404 would raise inside the gem instead).
			writeJSON(w, map[string]any{"errors": []map[string]any{{"message": "User not found"}}})
			return
		}
		fmt.Fprintf(w, `{"data":{"id":%q,"name":"Alice","username":"alice"}}`, f.userID)
	})
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		q := r.URL.Query()
		f.lastTimeline = &q
		if f.onTimeline != nil {
			f.onTimeline()
		}
		page := 0
		if token := r.URL.Query().Get("pagination_token"); token != "" {
			_, _ = fmt.Sscanf(token, "page%d", &page)
		}
		if page >= len(f.pages) {
			fmt.Fprint(w, `{"meta":{"result_count":0}}`)
			return
		}
		writeJSON(w, map[string]any{
			"data": f.pages[page],
			"meta": map[string]any{"result_count": len(f.pages[page])},
		})
	})
	if f.extra != nil {
		mux.Handle("/", f.extra)
	}
	srv := httptest.NewServer(mux)
	f.t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// tweetJSON builds one API tweet object.
func tweetJSON(id int64, text string) map[string]any {
	return map[string]any{
		"id":         fmt.Sprint(id),
		"text":       text,
		"created_at": "2024-06-01T12:00:00.000Z",
	}
}

// newSyncer wires a Syncer against db and the fake server.
func newSyncer(database *sql.DB, dataDir string, srv *httptest.Server) *Syncer {
	s := NewSyncer(database, dataDir)
	s.SetBaseURL(srv.URL)
	s.SetHTTPClient(srv.Client())
	// Tests dial httptest servers on loopback: resolve every host to a
	// public IP so the redirect SSRF guard lets the fixtures through.
	s.SetLookupIP(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	return s
}

// enableSync inserts the enabled twitter_syncs + crossposts rows.
func enableSync(t *testing.T, database *sql.DB, username string) {
	t.Helper()
	now := time.Now().Unix()
	_, err := database.Exec(`INSERT INTO twitter_syncs (id, enabled, username, sync_schedule, created_at, updated_at)
		VALUES (1, 1, ?, 'every_15_minutes', ?, ?)`, username, now, now)
	if err != nil {
		t.Fatalf("insert twitter_syncs: %v", err)
	}
	_, err = database.Exec(`INSERT INTO crossposts (platform, enabled, api_key, api_key_secret, access_token, access_token_secret, created_at, updated_at)
		VALUES ('twitter', 1, 'k', 'ks', 't', 'ts', ?, ?)`, now, now)
	if err != nil {
		t.Fatalf("insert crossposts: %v", err)
	}
}

func getSyncRow(t *testing.T, database *sql.DB) query.TwitterSync {
	t.Helper()
	row, err := query.New(database).GetTwitterSync(context.Background())
	if err != nil {
		t.Fatalf("get twitter_syncs: %v", err)
	}
	return row
}

func articleBySlug(t *testing.T, database *sql.DB, slug string) query.Article {
	t.Helper()
	article, err := query.New(database).GetAdminArticleBySlug(context.Background(), sql.NullString{String: slug, Valid: true})
	if err != nil {
		t.Fatalf("get article %q: %v", slug, err)
	}
	return article
}

func articleExists(t *testing.T, database *sql.DB, slug string) bool {
	t.Helper()
	_, err := query.New(database).GetAdminArticleBySlug(context.Background(), sql.NullString{String: slug, Valid: true})
	return err == nil
}

func TestRunFirstRunLimit(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")

	var page []map[string]any
	for i := int64(15); i >= 1; i-- { // API returns newest first
		page = append(page, tweetJSON(i, fmt.Sprintf("tweet %d", i)))
	}
	fx := &fakeX{t: t, userID: "42", pages: [][]map[string]any{page}}
	s := newSyncer(database, t.TempDir(), fx.server())

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i := int64(1); i <= 15; i++ {
		slug := fmt.Sprintf("tweet-%d", i)
		if i <= 5 {
			if articleExists(t, database, slug) {
				t.Errorf("article %s exists, want skipped by first-run limit", slug)
			}
			continue
		}
		article := articleBySlug(t, database, slug)
		if article.Status != 1 || article.Comment != 1 {
			t.Errorf("%s status=%d comment=%d, want publish/1 comment/1", slug, article.Status, article.Comment)
		}
		// Tweets are archived as markdown: source in content_markdown, the
		// sanitized re-render in content_html.
		if article.ContentType != "markdown" {
			t.Errorf("%s content_type = %q, want markdown", slug, article.ContentType)
		}
		if want := fmt.Sprintf("tweet %d", i); !article.ContentMarkdown.Valid || !strings.Contains(article.ContentMarkdown.String, want) {
			t.Errorf("%s content_markdown = %+v, want containing %q", slug, article.ContentMarkdown, want)
		}
		if want := fmt.Sprintf("<p>tweet %d</p>\n", i); article.ContentHtml.String != want {
			t.Errorf("%s content = %q, want %q", slug, article.ContentHtml.String, want)
		}
		if article.CreatedAt != time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC).Unix() {
			t.Errorf("%s created_at = %d, want tweet created_at", slug, article.CreatedAt)
		}
	}
	row := getSyncRow(t, database)
	if row.SinceID.String != "15" {
		t.Errorf("since_id = %q, want 15", row.SinceID.String)
	}
	if !row.LastSyncedAt.Valid {
		t.Error("last_synced_at not stamped")
	}
	if row.LastError.Valid {
		t.Errorf("last_error = %q, want NULL", row.LastError.String)
	}
	if row.UserID.String != "42" {
		t.Errorf("user_id = %q, want resolved 42", row.UserID.String)
	}

	// social_media_posts carries the tweet's own URL.
	var postURL string
	if err := database.QueryRow(`SELECT url FROM social_media_posts WHERE platform = 'twitter'
		AND article_id = (SELECT id FROM articles WHERE slug = 'tweet-15')`).Scan(&postURL); err != nil {
		t.Fatalf("query social_media_posts: %v", err)
	}
	if want := "https://x.com/alice/status/15"; postURL != want {
		t.Errorf("social post url = %q, want %q", postURL, want)
	}
}

func TestRunIncrementalSinceID(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	// Poison tweet: unparseable created_at fails archiving; the run must not
	// abort and since_id must still advance past it (Rails behavior).
	fx := &fakeX{t: t, userID: "42", pages: [][]map[string]any{{
		tweetJSON(101, "good"),
		{"id": "102", "text": "poison", "created_at": "not-a-date"},
	}}}
	srv := fx.server()
	s := newSyncer(database, t.TempDir(), srv)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !articleExists(t, database, "tweet-101") {
		t.Error("tweet-101 not archived")
	}
	if articleExists(t, database, "tweet-102") {
		t.Error("poison tweet-102 archived, want skipped")
	}
	row := getSyncRow(t, database)
	if row.SinceID.String != "102" {
		t.Errorf("since_id = %q, want 102 (advances past the poison tweet)", row.SinceID.String)
	}

	// Second run: incremental, sends since_id and archives only new tweets.
	fx.pages = [][]map[string]any{{tweetJSON(103, "newer")}}
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := fx.lastTimeline.Get("since_id"); got != "102" {
		t.Errorf("timeline since_id param = %q, want 102", got)
	}
	if !articleExists(t, database, "tweet-103") {
		t.Error("tweet-103 not archived")
	}
	if row := getSyncRow(t, database); row.SinceID.String != "103" {
		t.Errorf("since_id = %q, want 103", row.SinceID.String)
	}
}

func TestRunTagsArticleAsTwitter(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	fx := &fakeX{t: t, userID: "42", pages: [][]map[string]any{{tweetJSON(1, "hello")}}}
	s := newSyncer(database, t.TempDir(), fx.server())

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	article := articleBySlug(t, database, "tweet-1")
	var tagName string
	if err := database.QueryRow(`SELECT tags.name FROM article_tags
		JOIN tags ON tags.id = article_tags.tag_id
		WHERE article_tags.article_id = ?`, article.ID).Scan(&tagName); err != nil {
		t.Fatalf("query article tag: %v", err)
	}
	if tagName != "twitter" {
		t.Errorf("tag = %q, want twitter", tagName)
	}

	// A later sync reuses the existing tag row instead of duplicating it.
	fx.pages = [][]map[string]any{{tweetJSON(2, "again")}}
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	var tagCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM tags WHERE LOWER(name) = 'twitter'`).Scan(&tagCount); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if tagCount != 1 {
		t.Errorf("twitter tag rows = %d, want 1", tagCount)
	}
	article2 := articleBySlug(t, database, "tweet-2")
	var linked int
	if err := database.QueryRow(`SELECT COUNT(*) FROM article_tags
		WHERE article_id = ? AND tag_id = (SELECT id FROM tags WHERE name = 'twitter')`, article2.ID).Scan(&linked); err != nil {
		t.Fatalf("query article_tags: %v", err)
	}
	if linked != 1 {
		t.Errorf("tweet-2 twitter tag links = %d, want 1", linked)
	}
}

func TestRunStartDateBackfill(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	if _, err := database.Exec(`UPDATE twitter_syncs SET start_date = '2024-06-01'`); err != nil {
		t.Fatalf("set start_date: %v", err)
	}
	old := tweetJSON(50, "too old")
	old["created_at"] = "2024-05-31T23:59:59.000Z" // before the start date
	// More than FirstRunLimit tweets: the first-run cap must not apply when a
	// start date is configured.
	var page []map[string]any
	for i := int64(61); i >= 51; i-- {
		tw := tweetJSON(i, fmt.Sprintf("backfill %d", i))
		tw["created_at"] = "2024-06-02T08:00:00.000Z"
		page = append(page, tw)
	}
	page = append(page, old)
	fx := &fakeX{t: t, userID: "42", pages: [][]map[string]any{page}}
	s := newSyncer(database, t.TempDir(), fx.server())

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fx.lastTimeline.Get("start_time"); got != "2024-06-01T00:00:00Z" {
		t.Errorf("start_time param = %q, want 2024-06-01T00:00:00Z", got)
	}
	if articleExists(t, database, "tweet-50") {
		t.Error("tweet-50 archived, want filtered by start_date")
	}
	for i := int64(51); i <= 61; i++ {
		if !articleExists(t, database, fmt.Sprintf("tweet-%d", i)) {
			t.Errorf("tweet-%d not archived (start-date backfill has no 10-cap)", i)
		}
	}
}

func TestRunPagination(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	// The pagination_token the fake hands out: next_token "page1" selects the
	// second scripted page.
	fx := &fakeX{t: t, userID: "42"}
	fx.pages = [][]map[string]any{{tweetJSON(11, "page one")}, {tweetJSON(12, "page two")}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/users/by/username/"):
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
		case strings.HasPrefix(r.URL.Path, "/users/42/tweets"):
			fx.requests.Add(1)
			if r.URL.Query().Get("pagination_token") == "" {
				writeJSON(w, map[string]any{"data": fx.pages[0], "meta": map[string]any{"result_count": 1, "next_token": "page1"}})
			} else {
				writeJSON(w, map[string]any{"data": fx.pages[1], "meta": map[string]any{"result_count": 1}})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fx.requests.Load(); got != 2 {
		t.Errorf("timeline requests = %d, want 2 pages", got)
	}
	if !articleExists(t, database, "tweet-11") || !articleExists(t, database, "tweet-12") {
		t.Error("expected tweets from both pages archived")
	}
	if row := getSyncRow(t, database); row.SinceID.String != "12" {
		t.Errorf("since_id = %q, want 12", row.SinceID.String)
	}
}

func TestRunQuoteTweetMapping(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	quotedText := "quoted wisdom https://t.co/qlink " + strings.Repeat("x", 300)
	tweet := tweetJSON(200, "my take https://t.co/q")
	tweet["referenced_tweets"] = []map[string]any{{"type": "quoted", "id": "199"}}
	tweet["entities"] = map[string]any{"urls": []map[string]any{
		{"url": "https://t.co/q", "expanded_url": "https://x.com/someone/status/199"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/users/by/username/"):
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
		case strings.HasPrefix(r.URL.Path, "/users/42/tweets"):
			writeJSON(w, map[string]any{
				"data": []map[string]any{tweet},
				"includes": map[string]any{
					"tweets": []map[string]any{{
						"id": "199", "text": quotedText, "author_id": "7",
						"created_at": "2024-05-01T00:00:00.000Z",
						"entities":   map[string]any{"urls": []map[string]any{{"url": "https://t.co/qlink", "expanded_url": "https://example.com/quoted-link"}}},
					}},
					"users": []map[string]any{{"id": "7", "name": "Quoted Author", "username": "someone"}},
				},
				"meta": map[string]any{"result_count": 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	article := articleBySlug(t, database, "tweet-200")
	if want := "https://x.com/i/web/status/199"; article.SourceUrl.String != want {
		t.Errorf("source_url = %q, want %q", article.SourceUrl.String, want)
	}
	if article.SourceAuthor.String != "Quoted Author" {
		t.Errorf("source_author = %q, want Quoted Author", article.SourceAuthor.String)
	}
	content := article.SourceContent.String
	if !strings.Contains(content, "https://example.com/quoted-link") {
		t.Errorf("source_content = %q, want t.co entity-expanded", content)
	}
	if n := len([]rune(content)); n > QuotedContentLimit {
		t.Errorf("source_content length = %d runes, want <= %d", n, QuotedContentLimit)
	}
	// The quote link in the tweet's own text is redundant with the Source
	// Reference and must be removed, not expanded.
	if strings.Contains(article.ContentHtml.String, "t.co/q") || strings.Contains(article.ContentHtml.String, "status/199") {
		t.Errorf("content_html = %q, want quoted link removed", article.ContentHtml.String)
	}
	if !strings.Contains(article.ContentHtml.String, "<p>my take</p>") {
		t.Errorf("content_html = %q, want <p>my take</p>", article.ContentHtml.String)
	}
}

func TestRunTcoResolution(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	// t1: entity-expanded. t2: no entity → HEAD follow fallback. t3: own media
	// link via entity → removed.
	headHits := atomic.Int64{}
	t1 := tweetJSON(301, "entity win https://t.co/aaa")
	t1["entities"] = map[string]any{"urls": []map[string]any{
		{"url": "https://t.co/aaa", "expanded_url": "https://example.com/real"},
	}}
	t2 := tweetJSON(302, "follow me https://t.co/bbb")
	t3 := tweetJSON(303, "look at this https://t.co/ccc")
	t3["entities"] = map[string]any{"urls": []map[string]any{
		{"url": "https://t.co/ccc", "expanded_url": "https://x.com/alice/status/303/photo/1"},
	}}
	extra := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headHits.Add(1)
		if r.URL.Path == "/bbb" {
			w.Header().Set("Location", "/final") // relative join, URI.join semantics
			w.WriteHeader(http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	fx := &fakeX{t: t, userID: "42", pages: [][]map[string]any{{t1, t2, t3}}, extra: extra}
	srv := fx.server()
	s := newSyncer(database, t.TempDir(), srv)
	// t.co is not routable in tests: rewrite the host to the fake server.
	target, _ := url.Parse(srv.URL)
	s.SetHTTPClient(&http.Client{Transport: rewriteHostTransport{
		base: http.DefaultTransport, fromHost: "t.co", scheme: target.Scheme, host: target.Host,
	}})

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := articleBySlug(t, database, "tweet-301").ContentHtml.String; !strings.Contains(got, "https://example.com/real") {
		t.Errorf("tweet-301 content = %q, want entity-expanded URL", got)
	}
	// The host rewrite is test-only: /bbb 301s to /final, which resolves
	// (URI.join semantics) against the t.co base, then returns 200. The bare
	// URL is autolinked by the markdown render (GFM), like any markdown post.
	if got := articleBySlug(t, database, "tweet-302").ContentHtml.String; got != `<p>follow me <a href="https://t.co/final">https://t.co/final</a></p>`+"\n" {
		t.Errorf("tweet-302 content = %q, want HEAD-followed relative redirect", got)
	}
	if got := articleBySlug(t, database, "tweet-303").ContentHtml.String; strings.Contains(got, "t.co/ccc") || strings.Contains(got, "photo/1") {
		t.Errorf("tweet-303 content = %q, want own-media link removed", got)
	}
	if got := articleBySlug(t, database, "tweet-303").ContentHtml.String; got != "<p>look at this</p>\n" {
		t.Errorf("tweet-303 content = %q, want trailing whitespace trimmed", got)
	}
	// Only the entity-less link (t2) hit HEAD: /bbb then /final.
	if got := headHits.Load(); got != 2 {
		t.Errorf("HEAD fallback hits = %d, want 2 (entities take priority)", got)
	}
}

// rewriteHostTransport rewrites requests for fromHost to the fake server.
type rewriteHostTransport struct {
	base     http.RoundTripper
	fromHost string
	scheme   string
	host     string
}

func (t rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.fromHost {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = t.scheme
		clone.URL.Host = t.host
		req = clone
	}
	return t.base.RoundTrip(req)
}

func TestFollowRedirectSchemeGuard(t *testing.T) {
	database := newTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "ftp://evil.example.com/file")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)
	if got := s.followRedirect(context.Background(), srv.URL+"/short", redirectLimit); got != srv.URL+"/short" {
		t.Errorf("followRedirect to non-http scheme = %q, want original URL kept", got)
	}
}

// TestFollowRedirectHopLimit: the redirect budget buys hops, not a discarded
// final HEAD — a chain of exactly redirectLimit redirects resolves to the
// last target with exactly redirectLimit requests.
func TestFollowRedirectHopLimit(t *testing.T) {
	database := newTestDB(t)
	var hits atomic.Int64
	mux := http.NewServeMux()
	for i := 0; i <= redirectLimit; i++ {
		mux.HandleFunc(fmt.Sprintf("/hop/%d", i), func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if i < redirectLimit {
				w.Header().Set("Location", fmt.Sprintf("/hop/%d", i+1))
				w.WriteHeader(http.StatusFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)

	want := srv.URL + fmt.Sprintf("/hop/%d", redirectLimit)
	if got := s.followRedirect(context.Background(), srv.URL+"/hop/0", redirectLimit); got != want {
		t.Errorf("followRedirect = %q, want %q (%d hops followed)", got, want, redirectLimit)
	}
	if got := hits.Load(); got != int64(redirectLimit) {
		t.Errorf("HEAD requests = %d, want %d (no wasted request)", got, redirectLimit)
	}
}

// TestFollowRedirectBlockedTarget: a redirect chain that lands on an
// internal address (literal or DNS-resolved) is refused before any request
// to it, and followRedirect returns "" so the caller keeps the t.co link —
// the blocked URL must not leak into the article content.
func TestFollowRedirectBlockedTarget(t *testing.T) {
	database := newTestDB(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/literal":
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data")
		case "/named":
			w.Header().Set("Location", "http://metadata.internal/latest")
		}
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)
	// The guard must see the real target: IP literals pass through to the
	// blocklist check, names resolve to a public IP (metadata.internal to
	// the link-local metadata address).
	s.SetLookupIP(func(_ context.Context, host string) ([]netip.Addr, error) {
		if addr, err := netip.ParseAddr(host); err == nil {
			return []netip.Addr{addr}, nil
		}
		if host == "metadata.internal" {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	// t.co is not routable in tests: rewrite the host to the fake server.
	target, _ := url.Parse(srv.URL)
	s.SetHTTPClient(&http.Client{Transport: rewriteHostTransport{
		base: http.DefaultTransport, fromHost: "t.co", scheme: target.Scheme, host: target.Host,
	}})

	for _, short := range []string{"https://t.co/literal", "https://t.co/named"} {
		if got := s.followRedirect(context.Background(), short, redirectLimit); got != "" {
			t.Errorf("followRedirect(%q) = %q, want \"\" (blocked target keeps the t.co link)", short, got)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("HEAD requests = %d, want 2 (internal targets never requested)", got)
	}
}

// TestFollowRedirectLastHopScreened: a chain of exactly redirectLimit
// redirects exhausts the hop budget, so the final target never gets a HEAD —
// but it is still screened, and an internal address there yields "" instead
// of leaking into the article content.
func TestFollowRedirectLastHopScreened(t *testing.T) {
	database := newTestDB(t)
	var hits atomic.Int64
	mux := http.NewServeMux()
	for i := 0; i < redirectLimit; i++ {
		mux.HandleFunc(fmt.Sprintf("/hop/%d", i), func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if i < redirectLimit-1 {
				w.Header().Set("Location", fmt.Sprintf("/hop/%d", i+1))
			} else {
				w.Header().Set("Location", "http://169.254.169.254/latest/meta-data")
			}
			w.WriteHeader(http.StatusFound)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)
	// The guard must see the real target: IP literals pass through to the
	// blocklist check, names resolve to a public IP so the hops pass.
	s.SetLookupIP(func(_ context.Context, host string) ([]netip.Addr, error) {
		if addr, err := netip.ParseAddr(host); err == nil {
			return []netip.Addr{addr}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	// t.co is not routable in tests: rewrite the host to the fake server.
	target, _ := url.Parse(srv.URL)
	s.SetHTTPClient(&http.Client{Transport: rewriteHostTransport{
		base: http.DefaultTransport, fromHost: "t.co", scheme: target.Scheme, host: target.Host,
	}})

	if got := s.followRedirect(context.Background(), "https://t.co/hop/0", redirectLimit); got != "" {
		t.Errorf("followRedirect = %q, want \"\" (blocked final target keeps the t.co link)", got)
	}
	if got := hits.Load(); got != int64(redirectLimit) {
		t.Errorf("HEAD requests = %d, want %d (blocked final target never requested)", got, redirectLimit)
	}
}

// TestResolveTcoLinksBudget: one text resolves at most tcoResolveBudget
// distinct short links via HEAD; links past the budget keep their t.co
// text, so a link-stuffed tweet cannot stall the sync.
func TestResolveTcoLinksBudget(t *testing.T) {
	database := newTestDB(t)
	hits := atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/dest/") {
			w.Header().Set("Location", "/dest"+r.URL.Path)
			w.WriteHeader(http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)
	// t.co is not routable in tests: rewrite the host to the fake server.
	target, _ := url.Parse(srv.URL)
	s.SetHTTPClient(&http.Client{Transport: rewriteHostTransport{
		base: http.DefaultTransport, fromHost: "t.co", scheme: target.Scheme, host: target.Host,
	}})

	links := make([]string, 0, tcoResolveBudget+5)
	wantParts := make([]string, 0, tcoResolveBudget+5)
	for i := 0; i < tcoResolveBudget+5; i++ {
		short := fmt.Sprintf("https://t.co/l%02d", i)
		links = append(links, short)
		if i < tcoResolveBudget {
			wantParts = append(wantParts, fmt.Sprintf("https://t.co/dest/l%02d", i))
		} else {
			wantParts = append(wantParts, short)
		}
	}
	got := s.resolveTcoLinks(context.Background(), strings.Join(links, " "), apiTweet{ID: "1"}, "")
	if want := strings.Join(wantParts, " "); got != want {
		t.Errorf("resolveTcoLinks = %q, want %q (links past the budget keep their t.co text)", got, want)
	}
	if got := hits.Load(); got != int64(2*tcoResolveBudget) {
		t.Errorf("HEAD requests = %d, want %d (budget %d x 2 hops)", got, 2*tcoResolveBudget, tcoResolveBudget)
	}
}

// TestResolveTcoLinksMemo: a repeated short link is followed once per text —
// the memo keeps duplicates from spending the budget and the network twice.
func TestResolveTcoLinksMemo(t *testing.T) {
	database := newTestDB(t)
	hits := atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/dup" {
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)
	// t.co is not routable in tests: rewrite the host to the fake server.
	target, _ := url.Parse(srv.URL)
	s.SetHTTPClient(&http.Client{Transport: rewriteHostTransport{
		base: http.DefaultTransport, fromHost: "t.co", scheme: target.Scheme, host: target.Host,
	}})

	text := "one https://t.co/dup two https://t.co/dup three https://t.co/dup"
	want := "one https://t.co/final two https://t.co/final three https://t.co/final"
	if got := s.resolveTcoLinks(context.Background(), text, apiTweet{ID: "1"}, ""); got != want {
		t.Errorf("resolveTcoLinks = %q, want %q", got, want)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("HEAD requests = %d, want 2 (memoized duplicate)", got)
	}
}

func TestRunMediaDownload(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	dataDir := t.TempDir()
	tweet := tweetJSON(401, "with media")
	tweet["attachments"] = map[string]any{"media_keys": []string{"mk_photo", "mk_video"}}
	extra := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/img/pic.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(testJPEG)
		case "/vid/low.mp4", "/vid/high.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("mp4-bytes"))
		default:
			http.NotFound(w, r)
		}
	})
	fx := &fakeX{t: t, userID: "42", extra: extra}
	fx.pages = [][]map[string]any{{tweet}}
	var mediaSrv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/users/by/username/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"id":"42"}}`)
	})
	mux.HandleFunc("/users/42/tweets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"data": []map[string]any{tweet},
			"includes": map[string]any{"media": []map[string]any{
				{"media_key": "mk_photo", "type": "photo", "url": mediaSrv.URL + "/img/pic.jpg"},
				{"media_key": "mk_video", "type": "video", "variants": []map[string]any{
					{"content_type": "video/mp4", "bitrate": 100, "url": mediaSrv.URL + "/vid/low.mp4"},
					{"content_type": "video/mp4", "bitrate": 900, "url": mediaSrv.URL + "/vid/high.mp4"},
					{"content_type": "application/x-mpegURL", "bitrate": 1200, "url": mediaSrv.URL + "/vid/stream.m3u8"},
				}},
			}},
			"meta": map[string]any{"result_count": 1},
		})
	})
	mux.Handle("/", extra)
	mediaSrv = httptest.NewServer(mux)
	t.Cleanup(mediaSrv.Close)
	s := newSyncer(database, dataDir, mediaSrv)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	article := articleBySlug(t, database, "tweet-401")
	html := article.ContentHtml.String
	if !strings.Contains(html, `<img src="/files/`) || !strings.Contains(html, `loading="lazy"`) {
		t.Errorf("content = %q, want embedded image", html)
	}
	if !strings.Contains(html, `<video src="/files/`) || !strings.Contains(html, "controls") {
		t.Errorf("content = %q, want embedded video", html)
	}
	// Highest-bitrate mp4 variant wins: only one video file stored.
	var videoFiles int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files WHERE content_type = 'video/mp4'`).Scan(&videoFiles); err != nil {
		t.Fatalf("count video files: %v", err)
	}
	if videoFiles != 1 {
		t.Errorf("video files = %d, want 1 (highest-bitrate variant only)", videoFiles)
	}
	// Attachments link the files to the article.
	var attachments int
	if err := database.QueryRow(`SELECT COUNT(*) FROM attachments WHERE record_type = 'Article' AND record_id = ? AND name = 'embeds'`, article.ID).Scan(&attachments); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attachments != 2 {
		t.Errorf("attachments = %d, want 2", attachments)
	}
	// Media files landed on disk under the media layout.
	rows, err := database.Query(`SELECT key FROM files`)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan key: %v", err)
		}
		if p := media.New(database, dataDir).PathFor(key); !fileExists(p) {
			t.Errorf("file %s missing on disk at %s", key, p)
		}
	}
}

// TestDownloadMediaSizeLimit refuses an over-100MB download instead of
// storing a truncated, permanently corrupt file.
func TestDownloadMediaSizeLimit(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		chunk := make([]byte, 1<<20)
		for i := 0; i < maxMediaBytes/(1<<20)+1; i++ {
			_, _ = w.Write(chunk)
		}
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, dataDir, srv)

	if _, err := s.downloadMedia(context.Background(), srv.URL+"/vid/big.mp4", "video/mp4", "999"); err == nil ||
		!strings.Contains(err.Error(), "100MB") {
		t.Fatalf("err = %v, want the 100MB limit", err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("files rows = %d, want 0 (oversized media must not be stored)", n)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "files")); !os.IsNotExist(err) {
		t.Errorf("media blob left on disk after a refused download")
	}
}

// TestDownloadMediaStripsQueryFromFilename: video variant URLs carry a query
// string (?tag=12); the stored filename takes its extension from the URL path
// only, so no query text leaks into it.
func TestDownloadMediaStripsQueryFromFilename(t *testing.T) {
	database := newTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4-bytes"))
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)

	stored, err := s.downloadMedia(context.Background(), srv.URL+"/vid/clip.mp4?tag=12", "video/mp4", "999")
	if err != nil {
		t.Fatalf("downloadMedia: %v", err)
	}
	if ext := filepath.Ext(stored.filename); ext != ".mp4" {
		t.Errorf("filename = %q, want a plain .mp4 extension, got %q", stored.filename, ext)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// noisyJPEG encodes an image whose variant re-encode is guaranteed to be
// smaller than the original (random noise does not compress well), so Store
// also creates a variant row/blob for it.
func noisyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2048, 2048))
	if _, err := rand.Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// countBlobs returns the number of regular files under <dataDir>/files.
func countBlobs(t *testing.T, dataDir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(dataDir, "files"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	return n
}

// TestRunMediaReclaimedWhenArchiveFails: when the archive transaction fails
// after the tweet's media was stored (here the social_media_posts insert is
// forced to fail), the article rolls back and the stored media rows and
// blobs are reclaimed instead of staying orphaned.
func TestRunMediaReclaimedWhenArchiveFails(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	dataDir := t.TempDir()

	tweet := tweetJSON(501, "with media")
	tweet["attachments"] = map[string]any{"media_keys": []string{"mk_photo"}}
	var mediaSrv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/users/by/username/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"id":"42"}}`)
	})
	mux.HandleFunc("/users/42/tweets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"data": []map[string]any{tweet},
			"includes": map[string]any{"media": []map[string]any{
				{"media_key": "mk_photo", "type": "photo", "url": mediaSrv.URL + "/img/pic.jpg"},
			}},
			"meta": map[string]any{"result_count": 1},
		})
	})
	mux.HandleFunc("/img/pic.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(noisyJPEG(t))
	})
	mediaSrv = httptest.NewServer(mux)
	t.Cleanup(mediaSrv.Close)
	s := newSyncer(database, dataDir, mediaSrv)

	// Force the social_media_posts write to abort the archive transaction.
	if _, err := database.Exec(`CREATE TRIGGER fail_social_post BEFORE INSERT ON social_media_posts
		BEGIN SELECT RAISE(ABORT, 'forced social post failure'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if articleExists(t, database, "tweet-501") {
		t.Error("article committed despite the failed archive transaction")
	}
	var files int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if files != 0 {
		t.Errorf("files rows = %d, want 0 (orphan media reclaimed)", files)
	}
	if n := countBlobs(t, dataDir); n != 0 {
		t.Errorf("blobs on disk = %d, want 0 (orphan media reclaimed)", n)
	}
	// Poison-tweet semantics are unchanged: since_id advances past the tweet.
	if row := getSyncRow(t, database); row.SinceID.String != "501" {
		t.Errorf("since_id = %q, want 501", row.SinceID.String)
	}

	// Sanity: with the failure gone, the same setup archives the tweet and
	// its media (original + variant), proving the zero counts above came
	// from the reclaim and not from a failed download.
	if _, err := database.Exec(`DROP TRIGGER fail_social_post`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := database.Exec(`UPDATE twitter_syncs SET since_id = NULL`); err != nil {
		t.Fatalf("reset since_id: %v", err)
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !articleExists(t, database, "tweet-501") {
		t.Error("tweet-501 not archived after the failure was removed")
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if files != 2 {
		t.Errorf("files rows = %d, want 2 (original + variant)", files)
	}
}

// TestRunMediaReclaimedWhenAttachmentFails: when an attachment insert fails
// inside the archive transaction, the transaction rolls back and the stored
// media rows and blobs are reclaimed — committing would leave the media
// without an attachment reference, orphaned forever.
func TestRunMediaReclaimedWhenAttachmentFails(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	dataDir := t.TempDir()

	tweet := tweetJSON(501, "with media")
	tweet["attachments"] = map[string]any{"media_keys": []string{"mk_photo"}}
	var mediaSrv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/users/by/username/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"id":"42"}}`)
	})
	mux.HandleFunc("/users/42/tweets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"data": []map[string]any{tweet},
			"includes": map[string]any{"media": []map[string]any{
				{"media_key": "mk_photo", "type": "photo", "url": mediaSrv.URL + "/img/pic.jpg"},
			}},
			"meta": map[string]any{"result_count": 1},
		})
	})
	mux.HandleFunc("/img/pic.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(noisyJPEG(t))
	})
	mediaSrv = httptest.NewServer(mux)
	t.Cleanup(mediaSrv.Close)
	s := newSyncer(database, dataDir, mediaSrv)

	// Force the attachment write to abort the archive transaction.
	if _, err := database.Exec(`CREATE TRIGGER fail_attachment BEFORE INSERT ON attachments
		BEGIN SELECT RAISE(ABORT, 'forced attachment failure'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if articleExists(t, database, "tweet-501") {
		t.Error("article committed despite the failed attachment insert")
	}
	var files int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if files != 0 {
		t.Errorf("files rows = %d, want 0 (orphan media reclaimed)", files)
	}
	if n := countBlobs(t, dataDir); n != 0 {
		t.Errorf("blobs on disk = %d, want 0 (orphan media reclaimed)", n)
	}
	// Poison-tweet semantics are unchanged: since_id advances past the tweet.
	if row := getSyncRow(t, database); row.SinceID.String != "501" {
		t.Errorf("since_id = %q, want 501", row.SinceID.String)
	}

	// Sanity: with the failure gone, the same setup archives the tweet and
	// attaches its media, proving the zero counts above came from the
	// reclaim and not from a failed download.
	if _, err := database.Exec(`DROP TRIGGER fail_attachment`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := database.Exec(`UPDATE twitter_syncs SET since_id = NULL`); err != nil {
		t.Fatalf("reset since_id: %v", err)
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	article := articleBySlug(t, database, "tweet-501")
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if files != 2 {
		t.Errorf("files rows = %d, want 2 (original + variant)", files)
	}
	var attachments int
	if err := database.QueryRow(`SELECT COUNT(*) FROM attachments WHERE record_type = 'Article' AND record_id = ? AND name = 'embeds'`, article.ID).Scan(&attachments); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attachments != 1 {
		t.Errorf("attachments = %d, want 1", attachments)
	}
}

// TestRunCursorWriteDiscardedAfterAdminReset: the admin changes start_date
// while a run is in flight (the config overlay + cursor reset of
// twitterSyncUpdate). The run's closing SetTwitterSyncSuccess still computes
// its cursor from the values read at run start, so the CAS guard rejects it:
// the reset must stand — writing the old cursor back would silently skip the
// backfill the reset was meant to trigger. The tweets the run already
// archived stay (slug dedup prevents re-archiving).
func TestRunCursorWriteDiscardedAfterAdminReset(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	var once sync.Once
	fx := &fakeX{t: t, userID: "42", pages: [][]map[string]any{{tweetJSON(11, "hello")}}}
	fx.onTimeline = func() {
		once.Do(func() {
			if _, err := database.Exec(`UPDATE twitter_syncs SET start_date = '2024-06-01',
				since_id = NULL, last_synced_at = NULL, last_error = NULL`); err != nil {
				t.Errorf("admin reset: %v", err)
			}
		})
	}
	s := newSyncer(database, t.TempDir(), fx.server())

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !articleExists(t, database, "tweet-11") {
		t.Error("tweet-11 not archived (archived tweets survive a discarded cursor write)")
	}
	row := getSyncRow(t, database)
	if row.SinceID.Valid {
		t.Errorf("since_id = %q, want NULL (stale cursor write must be discarded)", row.SinceID.String)
	}
	if row.LastSyncedAt.Valid {
		t.Error("last_synced_at stamped despite the discarded write")
	}
	if row.StartDate.String != "2024-06-01" {
		t.Errorf("start_date = %q, want the admin's 2024-06-01", row.StartDate.String)
	}
	// The user_id write happened before the reset and the username did not
	// change, so its guard lets it through.
	if row.UserID.String != "42" {
		t.Errorf("user_id = %q, want 42 (username unchanged, write allowed)", row.UserID.String)
	}
}

// TestRunUserIDWriteDiscardedAfterUsernameChange: the admin switches the
// account while the users/by/username lookup is in flight. The resolved id
// belongs to the old account; the CAS guard on SetTwitterSyncUserID drops it
// (leaving the admin's clear in place) and the run aborts quietly instead of
// syncing the old timeline under the new username.
func TestRunUserIDWriteDiscardedAfterUsernameChange(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	var once sync.Once
	var timelineHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/users/by/username/"):
			once.Do(func() {
				// twitterSyncUpdate's net effect on a username change: config
				// overlay + cursor reset + user_id clear.
				if _, err := database.Exec(`UPDATE twitter_syncs SET username = 'bob',
					since_id = NULL, last_synced_at = NULL, last_error = NULL, user_id = NULL`); err != nil {
					t.Errorf("admin update: %v", err)
				}
			})
			fmt.Fprint(w, `{"data":{"id":"42"}}`) // id of the OLD account
		case strings.HasPrefix(r.URL.Path, "/users/"):
			timelineHits.Add(1)
			writeJSON(w, map[string]any{"data": []map[string]any{tweetJSON(11, "old account tweet")}, "meta": map[string]any{"result_count": 1}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	row := getSyncRow(t, database)
	if row.Username.String != "bob" {
		t.Errorf("username = %q, want bob (admin update stands)", row.Username.String)
	}
	if row.UserID.Valid {
		t.Errorf("user_id = %q, want NULL (stale id of the old account discarded)", row.UserID.String)
	}
	if got := timelineHits.Load(); got != 0 {
		t.Errorf("timeline requests = %d, want 0 (run aborted before syncing the old account)", got)
	}
	if articleExists(t, database, "tweet-11") {
		t.Error("old account tweet archived under the new username")
	}
	if row.LastError.Valid {
		t.Errorf("last_error = %q, want NULL (a config change aborts quietly, not a failure)", row.LastError.String)
	}
}

func TestRunUserNotFound(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "ghost")
	fx := &fakeX{t: t, userID: ""}
	s := newSyncer(database, t.TempDir(), fx.server())

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	row := getSyncRow(t, database)
	if row.LastError.String != "user not found: ghost" {
		t.Errorf("last_error = %q, want %q", row.LastError.String, "user not found: ghost")
	}
}

func TestRunDisabled(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	if _, err := database.Exec(`UPDATE twitter_syncs SET enabled = 0`); err != nil {
		t.Fatalf("disable sync: %v", err)
	}
	fx := &fakeX{t: t, userID: "42", pages: [][]map[string]any{{tweetJSON(1, "x")}}}
	s := newSyncer(database, t.TempDir(), fx.server())
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fx.requests.Load(); got != 0 {
		t.Errorf("timeline requests = %d, want 0 for a disabled sync", got)
	}
}

func TestRunAPIError(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users/by/username/") {
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
			return
		}
		writeJSON(w, map[string]any{"errors": []map[string]any{{"message": "Rate limit exceeded"}}})
	}))
	t.Cleanup(srv.Close)
	s := newSyncer(database, t.TempDir(), srv)

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	row := getSyncRow(t, database)
	if row.LastError.String != "Rate limit exceeded" {
		t.Errorf("last_error = %q, want %q", row.LastError.String, "Rate limit exceeded")
	}
	if row.LastSyncedAt.Valid {
		t.Error("last_synced_at stamped on a failed run")
	}
}

func TestRunMutex(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")
	entered := make(chan struct{})
	release := make(chan struct{})
	fx := &fakeX{t: t, userID: "42", pages: [][]map[string]any{{tweetJSON(1, "x")}}}
	fx.onTimeline = func() {
		close(entered)
		<-release
	}
	s := newSyncer(database, t.TempDir(), fx.server())

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first run never reached the timeline request")
	}
	// The second Run overlaps the first and must be skipped entirely.
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := fx.requests.Load(); got != 1 {
		t.Errorf("timeline requests = %d, want 1 (concurrent run skipped)", got)
	}
}
