package crosspost

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/jobs"
	"rables/internal/service/media"
)

// fakePlatform records Post calls and returns a canned result.
type fakePlatform struct {
	name  string
	url   string
	err   error
	calls []PostInput
}

func (f *fakePlatform) Name() string { return f.name }
func (f *fakePlatform) Verify(context.Context, query.Crosspost) error {
	return nil
}
func (f *fakePlatform) Post(_ context.Context, _ query.Crosspost, in PostInput) (string, error) {
	f.calls = append(f.calls, in)
	return f.url, f.err
}

// fakeTokenCache is an in-memory tokenCache.
type fakeTokenCache struct {
	mu sync.Mutex
	m  map[string]string
}

func newFakeTokenCache() *fakeTokenCache { return &fakeTokenCache{m: map[string]string{}} }

func (f *fakeTokenCache) Get(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[key]
	return v, ok, nil
}

func (f *fakeTokenCache) Set(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = value
	return nil
}

func newTestDispatcher(t *testing.T) (*Dispatcher, *sql.DB, string) {
	t.Helper()
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	d := NewDispatcher(database, dataDir)
	d.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	// Tests dial httptest servers on loopback: resolve every host to a
	// public IP so the image-download SSRF guard lets the fixtures through.
	d.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	return d, database, dataDir
}

func insertArticle(t *testing.T, database *sql.DB, slug string) int64 {
	t.Helper()
	res, err := database.ExecContext(t.Context(),
		`INSERT INTO articles (slug, status, created_at, updated_at) VALUES (?, 1, 1000, 1000)`, slug)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func enablePlatform(t *testing.T, database *sql.DB, platform string) {
	t.Helper()
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO crossposts (platform, enabled, created_at, updated_at) VALUES (?, 1, 1000, 1000)`, platform); err != nil {
		t.Fatalf("enable platform %q: %v", platform, err)
	}
}

func recordSocialURL(t *testing.T, database *sql.DB, articleID int64, platform, url string, updatedAt int64) {
	t.Helper()
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO social_media_posts (article_id, platform, url, created_at, updated_at) VALUES (?, ?, ?, 1000, ?)`,
		articleID, platform, url, updatedAt); err != nil {
		t.Fatalf("record social url: %v", err)
	}
}

func socialURL(t *testing.T, database *sql.DB, articleID int64, platform string) string {
	t.Helper()
	var url string
	err := database.QueryRowContext(t.Context(),
		`SELECT url FROM social_media_posts WHERE article_id = ? AND platform = ?`, articleID, platform).Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("load social url: %v", err)
	}
	return url
}

func activityRows(t *testing.T, database *sql.DB, action string) int {
	t.Helper()
	var n int
	if err := database.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM activity_logs WHERE target = 'crosspost' AND action = ?`, action).Scan(&n); err != nil {
		t.Fatalf("count activity: %v", err)
	}
	return n
}

func crosspostJobPayload(t *testing.T, p jobPayload) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}

func TestDispatcherPostsAndRecordsURL(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")
	enablePlatform(t, database, "fakedispatch1")

	fake := &fakePlatform{name: "fakedispatch1", url: "https://mastodon.social/@u/1"}
	RegisterPlatform(fake)

	payload := crosspostJobPayload(t, jobPayload{ArticleID: articleID, Platform: "fakedispatch1", RequestedAt: 2000})
	if err := d.Handle(t.Context(), payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("post calls = %d, want 1", len(fake.calls))
	}
	if got := socialURL(t, database, articleID, "fakedispatch1"); got != "https://mastodon.social/@u/1" {
		t.Errorf("recorded url = %q", got)
	}
	if n := activityRows(t, database, "posted"); n != 2 { // service row + job summary row
		t.Errorf("posted activity rows = %d, want 2", n)
	}

	// A rerun (worker retry) must not post again: the recorded URL is newer
	// than requested_at, so the platform is skipped.
	if err := d.Handle(t.Context(), payload); err != nil {
		t.Fatalf("Handle rerun: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("post calls after rerun = %d, want 1 (deduped)", len(fake.calls))
	}
}

func TestDispatcherURLRecordedSince(t *testing.T) {
	tests := []struct {
		name        string
		updatedAt   int64
		requestedAt int64
		wantSkip    bool
	}{
		{"recorded after request skips", 2000, 1000, true},
		{"recorded at request skips", 1000, 1000, true},
		{"older url does not block a re-crosspost", 999, 1000, false},
		{"legacy payload skips on any url", 1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, database, _ := newTestDispatcher(t)
			articleID := insertArticle(t, database, "hello")
			enablePlatform(t, database, "fakerec")
			recordSocialURL(t, database, articleID, "fakerec", "https://old.example/1", tt.updatedAt)

			fake := &fakePlatform{name: "fakerec", url: "https://new.example/1"}
			RegisterPlatform(fake)

			payload := crosspostJobPayload(t, jobPayload{ArticleID: articleID, Platform: "fakerec", RequestedAt: tt.requestedAt})
			if err := d.Handle(t.Context(), payload); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if tt.wantSkip && len(fake.calls) != 0 {
				t.Error("platform called, want skip")
			}
			if !tt.wantSkip {
				if len(fake.calls) != 1 {
					t.Fatalf("post calls = %d, want 1", len(fake.calls))
				}
				if got := socialURL(t, database, articleID, "fakerec"); got != "https://new.example/1" {
					t.Errorf("url = %q, want updated", got)
				}
			}
		})
	}
}

func TestDispatcherPlatformsArrayPayload(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")
	enablePlatform(t, database, "fakearr1")
	enablePlatform(t, database, "fakearr2")

	one := &fakePlatform{name: "fakearr1", url: "https://x/1"}
	two := &fakePlatform{name: "fakearr2", url: "https://x/2"}
	RegisterPlatform(one)
	RegisterPlatform(two)

	// T14 scheduled-publish shape: no requested_at.
	payload := crosspostJobPayload(t, jobPayload{ArticleID: articleID, Platforms: []string{"fakearr1", "fakearr2"}})
	if err := d.Handle(t.Context(), payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(one.calls) != 1 || len(two.calls) != 1 {
		t.Fatalf("calls = %d/%d, want 1/1", len(one.calls), len(two.calls))
	}
}

func TestDispatcherSkipsDisabledMissingAndUnknown(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")

	// disabled row
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO crossposts (platform, enabled, created_at, updated_at) VALUES ('fakedisabled', 0, 1000, 1000)`); err != nil {
		t.Fatalf("insert disabled: %v", err)
	}
	disabled := &fakePlatform{name: "fakedisabled", url: "https://x/1"}
	missing := &fakePlatform{name: "fakemissing", url: "https://x/2"} // no crossposts row
	RegisterPlatform(disabled)
	RegisterPlatform(missing)

	payload := crosspostJobPayload(t, jobPayload{
		ArticleID: articleID,
		Platforms: []string{"fakedisabled", "fakemissing", "fakeregistered-nowhere"},
	})
	if err := d.Handle(t.Context(), payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(disabled.calls) != 0 || len(missing.calls) != 0 {
		t.Errorf("calls = %d/%d, want 0/0", len(disabled.calls), len(missing.calls))
	}
}

func TestDispatcherTransientErrorPropagates(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")
	enablePlatform(t, database, "faketransient")

	fake := &fakePlatform{name: "faketransient", err: TransientError{Err: errors.New("dial tcp: timeout")}}
	RegisterPlatform(fake)

	payload := crosspostJobPayload(t, jobPayload{ArticleID: articleID, Platform: "faketransient", RequestedAt: 2000})
	err := d.Handle(t.Context(), payload)
	if err == nil || !IsTransient(err) {
		t.Fatalf("Handle error = %v, want transient", err)
	}
	if got := socialURL(t, database, articleID, "faketransient"); got != "" {
		t.Errorf("url = %q, want none recorded", got)
	}
}

func TestDispatcherPermanentErrorIsLoggedNotRetried(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")
	enablePlatform(t, database, "fakeperm1")
	enablePlatform(t, database, "fakeperm2")

	bad := &fakePlatform{name: "fakeperm1", err: errors.New("422 Unprocessable Entity")}
	good := &fakePlatform{name: "fakeperm2", url: "https://x/2"}
	RegisterPlatform(bad)
	RegisterPlatform(good)

	payload := crosspostJobPayload(t, jobPayload{ArticleID: articleID, Platforms: []string{"fakeperm1", "fakeperm2"}})
	if err := d.Handle(t.Context(), payload); err != nil {
		t.Fatalf("Handle: %v, want nil (permanent errors do not retry)", err)
	}
	if len(good.calls) != 1 {
		t.Error("permanent failure of one platform blocked the next")
	}
	if n := activityRows(t, database, "failed"); n != 1 {
		t.Errorf("failed activity rows = %d, want 1", n)
	}
}

// TestDispatcherRecordURLFailureIsLoggedNotRetried: the post is already
// public when recordURL fails, so returning the error would retry the job
// and — with no URL recorded — publish a duplicate. The failure is logged
// as an error activity instead and the run goes on.
func TestDispatcherRecordURLFailureIsLoggedNotRetried(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")
	enablePlatform(t, database, "fakerecfail1")
	enablePlatform(t, database, "fakerecfail2")

	// Break only the URL write; reads (urlRecordedSince) still work.
	if _, err := database.ExecContext(t.Context(),
		`CREATE TRIGGER fail_social_media_posts BEFORE INSERT ON social_media_posts BEGIN SELECT RAISE(FAIL, 'no inserts'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	bad := &fakePlatform{name: "fakerecfail1", url: "https://x/1"}
	good := &fakePlatform{name: "fakerecfail2", url: "https://x/2"}
	RegisterPlatform(bad)
	RegisterPlatform(good)

	payload := crosspostJobPayload(t, jobPayload{ArticleID: articleID, Platforms: []string{"fakerecfail1", "fakerecfail2"}, RequestedAt: 2000})
	if err := d.Handle(t.Context(), payload); err != nil {
		t.Fatalf("Handle: %v, want nil (a recordURL failure must not retry into a duplicate public post)", err)
	}
	if len(bad.calls) != 1 || len(good.calls) != 1 {
		t.Errorf("calls = %d/%d, want 1/1 (a record failure does not block the next platform)", len(bad.calls), len(good.calls))
	}
	if got := socialURL(t, database, articleID, "fakerecfail1"); got != "" {
		t.Errorf("url = %q, want none recorded", got)
	}
	if n := activityRows(t, database, "failed"); n != 2 {
		t.Errorf("failed activity rows = %d, want 2 (one per unrecorded platform)", n)
	}
	if n := activityRows(t, database, "posted"); n != 0 {
		t.Errorf("posted activity rows = %d, want 0 (unrecorded platforms are not counted as posted)", n)
	}
}

func TestDispatcherMissingArticle(t *testing.T) {
	d, _, _ := newTestDispatcher(t)
	payload := crosspostJobPayload(t, jobPayload{ArticleID: 9999, Platform: "whatever"})
	if err := d.Handle(t.Context(), payload); err != nil {
		t.Fatalf("Handle: %v, want nil for a deleted article", err)
	}
}

// TestDispatcherCanceledErrorPropagates: a platform failing because the
// worker is shutting down (context.Canceled, plain or wrapped in a
// *url.Error) must propagate the error so the job is rescheduled — logging
// it as a permanent failure would silently drop the post.
func TestDispatcherCanceledErrorPropagates(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"plain context.Canceled", context.Canceled},
		{"url.Error wrapping context.Canceled", &url.Error{Op: "Post", URL: "https://x", Err: context.Canceled}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, database, _ := newTestDispatcher(t)
			articleID := insertArticle(t, database, "hello")
			enablePlatform(t, database, "fakecancel")

			fake := &fakePlatform{name: "fakecancel", err: tt.err}
			RegisterPlatform(fake)

			payload := crosspostJobPayload(t, jobPayload{ArticleID: articleID, Platform: "fakecancel", RequestedAt: 2000})
			err := d.Handle(t.Context(), payload)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Handle error = %v, want context.Canceled (job must be rescheduled)", err)
			}
			if n := activityRows(t, database, "failed"); n != 0 {
				t.Errorf("failed activity rows = %d, want 0 (shutdown is not a permanent failure)", n)
			}
		})
	}
}

// TestDispatcherBuildsContent covers the ContentBuilder wiring: the stored
// max_characters wins, the site URL builds the Read-more link, and Chinese
// text is not double-counted for mastodon-like platforms.
func TestDispatcherBuildsContent(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO settings (id, url, setup_completed, created_at, updated_at) VALUES (1, 'https://blog.example.com', 1, 0, 0)`); err != nil {
		t.Fatalf("insert settings: %v", err)
	}
	d.RoutePrefix = "posts"

	articleID := insertArticle(t, database, "hello")
	if _, err := database.ExecContext(t.Context(),
		`UPDATE articles SET title = ?, content_html = ? WHERE id = ?`,
		"标题", "<p>"+strings.Repeat("汉", 100)+"</p>", articleID); err != nil {
		t.Fatalf("update article: %v", err)
	}
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO crossposts (platform, enabled, max_characters, created_at, updated_at) VALUES ('fakecb', 1, 70, 1000, 1000)`); err != nil {
		t.Fatalf("insert crosspost: %v", err)
	}
	fake := &fakePlatform{name: "fakecb", url: "https://x/1"}
	RegisterPlatform(fake)

	payload := crosspostJobPayload(t, jobPayload{ArticleID: articleID, Platform: "fakecb", RequestedAt: 2000})
	if err := d.Handle(t.Context(), payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(fake.calls))
	}
	text := fake.calls[0].Text
	if !strings.HasPrefix(text, "标题\n") {
		t.Errorf("text should start with the title, got %q", text)
	}
	if !strings.HasSuffix(text, "\nRead more: https://blog.example.com/posts/hello") {
		t.Errorf("text should end with the prefixed Read-more link, got %q", text)
	}
	// 70 max chars, link 48: availableLength 22, remainingLength 19 → 16 汉字 + ...
	if !strings.Contains(text, "\n"+strings.Repeat("汉", 16)+"...") {
		t.Errorf("truncated body mismatch, got %q", text)
	}
}

// TestCollectImages covers all_image_attachments: attachments first (deduped
// against HTML <img> by file id), then /files/<key> srcs, then remote URLs,
// capped at 4.
func TestCollectImages(t *testing.T) {
	d, database, dataDir := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")

	png := []byte("\x89PNG\r\n\x1a\nfakepng")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("remotedata"))
	}))
	t.Cleanup(remote.Close)
	d.HTTPClient = remote.Client()

	// Relative /files/ fallbacks resolve against the site URL; point it at a
	// 404-only server so the non-image file deterministically fails the
	// download instead of depending on whatever runs on localhost:3000.
	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO settings (id, url, setup_completed, created_at, updated_at) VALUES (1, ?, 1, 0, 0)`, notFound.URL); err != nil {
		t.Fatalf("insert settings: %v", err)
	}

	store := media.New(database, dataDir)
	// Attachment path (ActionText attachables).
	attached, err := store.Store(t.Context(), strings.NewReader(string(png)), "attached.png", "image/png")
	if err != nil {
		t.Fatalf("store attached: %v", err)
	}
	if err := store.Attach(t.Context(), attached.ID, "Article", articleID, "embeds"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// HTML path (/files/<key>); the attached file is also referenced to prove
	// the blob-id dedupe.
	htmlFile, err := store.Store(t.Context(), strings.NewReader(string(png)), "html.png", "image/png")
	if err != nil {
		t.Fatalf("store html: %v", err)
	}
	nonImage, err := store.Store(t.Context(), strings.NewReader("hello"), "note.txt", "text/plain")
	if err != nil {
		t.Fatalf("store text: %v", err)
	}
	content := fmt.Sprintf(`<p>x</p><img src="/files/%s"><img src="/files/%s"><img src="/files/%s"><img src="%s/remote.png">`,
		attached.Key, htmlFile.Key, nonImage.Key, remote.URL)
	if _, err := database.ExecContext(t.Context(),
		`UPDATE articles SET content_html = ? WHERE id = ?`, content, articleID); err != nil {
		t.Fatalf("update content: %v", err)
	}

	article, err := d.q.GetAdminArticleByID(t.Context(), articleID)
	if err != nil {
		t.Fatalf("load article: %v", err)
	}
	images := d.collectImages(t.Context(), article)
	if len(images) != 3 {
		t.Fatalf("images = %d, want 3 (attachment, html file, remote; text file skipped)", len(images))
	}
	if images[0].Filename != "attached.png" || images[1].Filename != "html.png" || images[2].Filename != "remote.png" {
		t.Errorf("order = %q, %q, %q", images[0].Filename, images[1].Filename, images[2].Filename)
	}
	if string(images[2].Data) != "remotedata" {
		t.Errorf("remote data = %q", images[2].Data)
	}
	if images[2].ContentType != "image/png" {
		t.Errorf("remote content type = %q", images[2].ContentType)
	}
}

// TestCollectImagesDuplicateAttachmentNames: Rails legacy data can attach the
// same file under several names; collectImages keeps only the first row so one
// image cannot occupy several of the 4 slots.
func TestCollectImagesDuplicateAttachmentNames(t *testing.T) {
	d, database, dataDir := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")

	store := media.New(database, dataDir)
	file, err := store.Store(t.Context(), strings.NewReader("pngdata"), "dup.png", "image/png")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	for _, name := range []string{"embeds", "embeds2"} {
		if err := store.Attach(t.Context(), file.ID, "Article", articleID, name); err != nil {
			t.Fatalf("attach %q: %v", name, err)
		}
	}
	article, err := d.q.GetAdminArticleByID(t.Context(), articleID)
	if err != nil {
		t.Fatalf("load article: %v", err)
	}
	images := d.collectImages(t.Context(), article)
	if len(images) != 1 || images[0].Filename != "dup.png" {
		t.Errorf("images = %v, want just dup.png (same file attached twice)", images)
	}
}

func TestCollectImagesLimitFour(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")
	var sb strings.Builder
	for i := 0; i < 6; i++ {
		key := fmt.Sprintf("%032x", i+1)
		path := filepath.Join(d.Media.DataDir, "files", key[0:2], key[2:4])
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, key), []byte("x"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if _, err := database.ExecContext(t.Context(),
			`INSERT INTO files (key, filename, content_type, byte_size, created_at) VALUES (?, ?, 'image/png', 1, 0)`,
			key, fmt.Sprintf("img%d.png", i)); err != nil {
			t.Fatalf("insert file: %v", err)
		}
		sb.WriteString(fmt.Sprintf(`<img src="/files/%s">`, key))
	}
	if _, err := database.ExecContext(t.Context(),
		`UPDATE articles SET content_html = ? WHERE id = ?`, sb.String(), articleID); err != nil {
		t.Fatalf("update content: %v", err)
	}
	article, err := d.q.GetAdminArticleByID(t.Context(), articleID)
	if err != nil {
		t.Fatalf("load article: %v", err)
	}
	if images := d.collectImages(t.Context(), article); len(images) != 4 {
		t.Errorf("images = %d, want 4 (limit)", len(images))
	}
}

// TestCollectImagesBoundedAttempts: an article full of dead remote <img> srcs
// (imported articles keep remote srcs untouched) must not download them all —
// the scan stops after maxRemoteImageAttempts instead of burning the download
// timeout on every dead link.
func TestCollectImagesBoundedAttempts(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")

	var hits atomic.Int64
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(dead.Close)
	d.HTTPClient = dead.Client()

	var sb strings.Builder
	for i := 0; i < maxRemoteImageAttempts*3; i++ {
		sb.WriteString(fmt.Sprintf(`<img src="%s/dead%d.png">`, dead.URL, i))
	}
	if _, err := database.ExecContext(t.Context(),
		`UPDATE articles SET content_html = ? WHERE id = ?`, sb.String(), articleID); err != nil {
		t.Fatalf("update content: %v", err)
	}
	article, err := d.q.GetAdminArticleByID(t.Context(), articleID)
	if err != nil {
		t.Fatalf("load article: %v", err)
	}
	if images := d.collectImages(t.Context(), article); len(images) != 0 {
		t.Errorf("images = %d, want 0 (all links dead)", len(images))
	}
	if got := hits.Load(); got != maxRemoteImageAttempts {
		t.Errorf("download attempts = %d, want %d (scan bounded)", got, maxRemoteImageAttempts)
	}
}

// TestCollectImagesLocalPastBudget: the remote-download budget bounds remote
// attempts only — an over-budget remote src skips like a per-image failure,
// so local /files/<key> images after it are still collected.
func TestCollectImagesLocalPastBudget(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")

	var hits atomic.Int64
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(dead.Close)
	d.HTTPClient = dead.Client()

	key := fmt.Sprintf("%032x", 1)
	path := filepath.Join(d.Media.DataDir, "files", key[0:2], key[2:4])
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, key), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO files (key, filename, content_type, byte_size, created_at) VALUES (?, 'local.png', 'image/png', 1, 0)`,
		key); err != nil {
		t.Fatalf("insert file: %v", err)
	}

	var sb strings.Builder
	for i := 0; i < maxRemoteImageAttempts+1; i++ {
		sb.WriteString(fmt.Sprintf(`<img src="%s/dead%d.png">`, dead.URL, i))
	}
	sb.WriteString(fmt.Sprintf(`<img src="/files/%s">`, key))
	if _, err := database.ExecContext(t.Context(),
		`UPDATE articles SET content_html = ? WHERE id = ?`, sb.String(), articleID); err != nil {
		t.Fatalf("update content: %v", err)
	}
	article, err := d.q.GetAdminArticleByID(t.Context(), articleID)
	if err != nil {
		t.Fatalf("load article: %v", err)
	}
	images := d.collectImages(t.Context(), article)
	if len(images) != 1 || images[0].Filename != "local.png" {
		t.Errorf("images = %v, want just local.png (local srcs collect past the budget)", images)
	}
	if got := hits.Load(); got != maxRemoteImageAttempts {
		t.Errorf("download attempts = %d, want %d (scan bounded)", got, maxRemoteImageAttempts)
	}
}

// TestDownloadRemoteImageSchemeRelative: a protocol-relative <img
// src="//host/path"> inherits the site URL's scheme (URI.join) instead of
// being concatenated onto the site URL as a path.
func TestDownloadRemoteImageSchemeRelative(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("schemepng"))
	}))
	t.Cleanup(remote.Close)
	d.HTTPClient = remote.Client()
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO settings (id, url, setup_completed, created_at, updated_at) VALUES (1, ?, 1, 0, 0)`, remote.URL); err != nil {
		t.Fatalf("insert settings: %v", err)
	}

	img, ok := d.downloadRemoteImage(t.Context(), strings.TrimPrefix(remote.URL, "http:")+"/pic.png")
	if !ok {
		t.Fatal("scheme-relative download failed")
	}
	if string(img.Data) != "schemepng" {
		t.Errorf("data = %q", img.Data)
	}
	if img.Filename != "pic.png" {
		t.Errorf("filename = %q", img.Filename)
	}
}

// TestDownloadRemoteImageSchemelessSiteURL: settings.url is stored verbatim
// from the settings form and may lack a scheme ("example.com"); a relative
// <img src> must still resolve to an https URL instead of being rejected by
// the scheme check.
func TestDownloadRemoteImageSchemelessSiteURL(t *testing.T) {
	d, database, _ := newTestDispatcher(t)
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("schemelesspng"))
	}))
	t.Cleanup(remote.Close)
	d.HTTPClient = remote.Client()
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO settings (id, url, setup_completed, created_at, updated_at) VALUES (1, ?, 1, 0, 0)`,
		strings.TrimPrefix(remote.URL, "https://")); err != nil {
		t.Fatalf("insert settings: %v", err)
	}

	img, ok := d.downloadRemoteImage(t.Context(), "/pic.png")
	if !ok {
		t.Fatal("relative download with scheme-less site URL failed")
	}
	if string(img.Data) != "schemelesspng" {
		t.Errorf("data = %q", img.Data)
	}
	if img.Filename != "pic.png" {
		t.Errorf("filename = %q", img.Filename)
	}
}

// TestDownloadRemoteImageBlockedTarget: an <img src> pointing at an internal
// address (RSS imports keep such srcs untouched) is refused by the SSRF guard
// before any request goes out, and the image is skipped like any other
// download failure.
func TestDownloadRemoteImageBlockedTarget(t *testing.T) {
	d, _, _ := newTestDispatcher(t)
	d.lookupIP = nil // the guard must see the real loopback address
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	d.HTTPClient = srv.Client()

	if _, ok := d.downloadRemoteImage(t.Context(), srv.URL+"/x.png"); ok {
		t.Error("loopback image download succeeded, want skipped")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("requests to blocked target = %d, want 0", got)
	}
}

// TestDownloadRemoteImageRedirectBlocked: a public image host that 302s to an
// internal address is refused at the redirect hop (downloadClient re-checks
// every hop), so the response bytes never leave the server.
func TestDownloadRemoteImageRedirectBlocked(t *testing.T) {
	d, _, _ := newTestDispatcher(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	t.Cleanup(remote.Close)
	d.HTTPClient = downloadClient // the client with the SSRF-checking CheckRedirect

	if _, ok := d.downloadRemoteImage(t.Context(), remote.URL+"/x.png"); ok {
		t.Error("redirect to internal address succeeded, want skipped")
	}
}

func TestImageSrcs(t *testing.T) {
	tests := []struct {
		name string
		html string
		want []string
	}{
		{"empty", "", nil},
		{"single", `<p><img src="/files/abc123" alt="x"></p>`, []string{"/files/abc123"}},
		{"blank skipped", `<img src="  "><img src="/files/a">`, []string{"/files/a"}},
		{"document order", `<img src="1"><div><img src="2"></div><img src="3">`, []string{"1", "2", "3"}},
		{"no images", `<p>text</p>`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := imageSrcs(tt.html)
			if len(got) != len(tt.want) {
				t.Fatalf("imageSrcs = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("src[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsTransientClassification(t *testing.T) {
	if IsTransient(errors.New("422 Unprocessable Entity")) {
		t.Error("plain error must not be transient")
	}
	if !IsTransient(TransientError{Err: errors.New("boom")}) {
		t.Error("TransientError must be transient")
	}
	if !IsTransient(fmt.Errorf("wrap: %w", TransientError{Err: errors.New("boom")})) {
		t.Error("wrapped TransientError must be transient")
	}
}

// TestTransientNetErrorClassification: http.Client.Do failures arrive as
// *url.Error (which implements net.Error), so the whitelist must classify the
// inner error — and context.Canceled (worker shutdown) is never retryable.
func TestTransientNetErrorClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"context.Canceled", context.Canceled, false},
		{"url.Error wrapping context.Canceled", &url.Error{Op: "Get", URL: "https://x", Err: context.Canceled}, false},
		{"url.Error wrapping a permanent failure", &url.Error{Op: "Get", URL: "https://x", Err: errors.New("refusing redirect")}, false},
		{"url.Error wrapping a timeout", &url.Error{Op: "Get", URL: "https://x", Err: &net.DNSError{IsTimeout: true}}, true},
		{"url.Error wrapping EOF", &url.Error{Op: "Post", URL: "https://x", Err: io.ErrUnexpectedEOF}, true},
		{"url.Error wrapping a TLS record error", &url.Error{Op: "Get", URL: "https://x", Err: tls.RecordHeaderError{Msg: "bad"}}, true},
		{"url.Error wrapping an expired certificate", &url.Error{Op: "Get", URL: "https://x", Err: &tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Reason: x509.Expired}}}, true},
		{"syscall error", &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := transientNetError(tt.err)
			if tt.err == nil {
				if err != nil {
					t.Errorf("transientNetError(nil) = %v, want nil", err)
				}
				return
			}
			if IsTransient(err) != tt.transient {
				t.Errorf("IsTransient = %v, want %v", IsTransient(err), tt.transient)
			}
		})
	}
}

// TestRegisterCrosspostHandlersEndToEnd drives the kind=crosspost job through
// the real worker: success completes and records the URL, a transient
// failure reschedules with the backoff ladder, and the retried run does not
// duplicate the post.
func TestRegisterCrosspostHandlersEndToEnd(t *testing.T) {
	_, database, dataDir := newTestDispatcher(t)
	articleID := insertArticle(t, database, "hello")
	enablePlatform(t, database, "fakee2e")
	fake := &fakePlatform{name: "fakee2e", url: "https://mastodon.social/@u/9"}
	RegisterPlatform(fake)

	worker := jobs.NewWorker(database)
	RegisterCrosspostHandlers(worker, database, dataDir)
	enq := jobs.NewEnqueuer(database)

	// Transient failure first: the worker reschedules with attempts=1.
	fake.err = TransientError{Err: errors.New("dial tcp: i/o timeout")}
	jobID, err := enq.Enqueue(t.Context(), jobs.KindCrosspost,
		map[string]any{"article_id": articleID, "platform": "fakee2e", "requested_at": 2000}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := worker.RunOnce(t.Context())
	if err != nil || !claimed {
		t.Fatalf("RunOnce: claimed=%v err=%v", claimed, err)
	}
	var attempts int64
	var status string
	if err := database.QueryRowContext(t.Context(),
		`SELECT attempts, status FROM job_runs WHERE id = ?`, jobID).Scan(&attempts, &status); err != nil {
		t.Fatalf("load job: %v", err)
	}
	if attempts != 1 || status != "queued" {
		t.Fatalf("job = attempts %d status %q, want 1/queued (backoff)", attempts, status)
	}

	// Now succeed: make the job due and rerun.
	fake.err = nil
	if _, err := database.ExecContext(t.Context(), `UPDATE job_runs SET run_at = 0 WHERE id = ?`, jobID); err != nil {
		t.Fatalf("force due: %v", err)
	}
	if _, err := worker.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce retry: %v", err)
	}
	if got := socialURL(t, database, articleID, "fakee2e"); got != "https://mastodon.social/@u/9" {
		t.Fatalf("recorded url = %q", got)
	}
	if err := database.QueryRowContext(t.Context(),
		`SELECT status FROM job_runs WHERE id = ?`, jobID).Scan(&status); err != nil {
		t.Fatalf("load job: %v", err)
	}
	if status != "done" {
		t.Errorf("job status = %q, want done", status)
	}
	callsAfterSuccess := len(fake.calls) // 1 failed attempt + 1 success

	// A second crosspost job for the same request is deduped by the recorded
	// URL (updated_at >= requested_at).
	if _, err := enq.Enqueue(t.Context(), jobs.KindCrosspost,
		map[string]any{"article_id": articleID, "platform": "fakee2e", "requested_at": 2000}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := worker.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce duplicate: %v", err)
	}
	if len(fake.calls) != callsAfterSuccess {
		t.Errorf("post calls = %d, want %d (idempotent retry)", len(fake.calls), callsAfterSuccess)
	}
}
