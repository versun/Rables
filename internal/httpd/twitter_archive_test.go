package httpd

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/jobs"
	"rables/internal/service/twitterarchive"
	"rables/internal/templates"
)

// newTwitterArchiveTestServer mounts only the twitter archive routes.
func newTwitterArchiveTestServer(t *testing.T, routePrefix string) (*Server, http.Handler) {
	t.Helper()
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	renderer, err := templates.New()
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := config.Config{Addr: ":8080", HMACSecret: "x", DataDir: dataDir, ArticleRoutePrefix: routePrefix}
	s := NewServer(database, cfg, logger, renderer)
	r := chi.NewRouter()
	RegisterTwitterArchiveAdminRoutes(r, s)
	RegisterTwitterArchivePublicRoutes(r, s)
	return s, r
}

// archiveZipBody returns a small valid archive zip.
func archiveZipBody(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"data/account.js": `window.YTD.account.part0 = [{"account":{"username":"archive_owner"}}]`,
		"data/tweets.js":  `window.YTD.tweets.part0 = [{"tweet":{"id":"200","id_str":"200","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Original tweet"}}]`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// postArchiveUpload posts a multipart form with the given file field; a nil
// content posts twitter_archive as a plain value (the malformed-params case
// of the Rails controller).
func postArchiveUpload(t *testing.T, h http.Handler, filename, contentType string, content []byte, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if content == nil {
		if err := mw.WriteField("twitter_archive", "not-a-hash"); err != nil {
			t.Fatalf("write field: %v", err)
		}
	} else {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="twitter_archive[file]"; filename="%s"`, filename))
		hdr.Set("Content-Type", contentType)
		part, err := mw.CreatePart(hdr)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/twitter_archives", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// readFlash decodes the flash cookie set on a redirect response; it is
// shared with the other httpd tests (comments_test.go).

func countImports(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM twitter_archive_imports`).Scan(&n); err != nil {
		t.Fatalf("count imports: %v", err)
	}
	return n
}

func seedArchiveTweet(t *testing.T, s *Server, tweetID, entryType, fullText string, tweetedAt int64) int64 {
	t.Helper()
	res, err := s.DB.Exec(`INSERT INTO twitter_archive_tweets (tweet_id, screen_name, full_text, entry_type, tweeted_at, created_at, updated_at)
		VALUES (?, 'archive_owner', ?, ?, ?, 1000, 1000)`, tweetID, fullText, entryType, tweetedAt)
	if err != nil {
		t.Fatalf("insert archive tweet: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestAdminTwitterArchivesIndex(t *testing.T) {
	s, h := newTwitterArchiveTestServer(t, "")
	session := seedSession(t, s)

	// Unauthenticated requests redirect to the login form.
	rec := get(t, h, "/admin/twitter_archives")
	if rec.Code != http.StatusFound {
		t.Fatalf("anonymous index = %d, want 302", rec.Code)
	}

	seedArchiveTweet(t, s, "300", "tweet", "Existing archive entry", 1000)
	now := time.Now().Unix()
	if _, err := s.DB.Exec(`INSERT INTO twitter_archive_imports
		(status, progress, source_filename, source_path, status_message, queued_at, started_at, active_slot, created_at, updated_at)
		VALUES ('running', 45, 'twitter-archive.zip', '/tmp/twitter-archive.zip', 'Reading archive', ?, ?, 1, ?, ?)`,
		now-60, now, now, now); err != nil {
		t.Fatalf("insert import: %v", err)
	}

	rec = get(t, h, "/admin/twitter_archives", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("index = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Twitter Archive",
		"Total archived items:",
		"(1 original,", // 1 original tweet
		"twitter-archive.zip",
		"Running",
		"45%",
		"Import History",
		`action="/admin/twitter_archives"`,
		`enctype="multipart/form-data"`,
		`/twitter/archive`, // Open Public Archive link
		`data-twitter-archive-imports`,
		`data-import-status="running"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index body misses %q", want)
		}
	}
}

func TestAdminTwitterArchivesCreateQueuesImport(t *testing.T) {
	s, h := newTwitterArchiveTestServer(t, "")
	session := seedSession(t, s)

	rec := postArchiveUpload(t, h, "twitter-archive.zip", "application/zip", archiveZipBody(t), session)
	if rec.Code != http.StatusFound {
		t.Fatalf("create = %d, want 302", rec.Code)
	}
	flash := readFlash(t, rec)
	if !strings.Contains(strings.ToLower(flash.Notice), "queued") {
		t.Fatalf("notice = %q", flash.Notice)
	}
	if countImports(t, s) != 1 {
		t.Fatalf("imports = %d, want 1", countImports(t, s))
	}
	var status, filename, sourcePath string
	var activeSlot sql.NullInt64
	if err := s.DB.QueryRow(`SELECT status, source_filename, source_path, active_slot FROM twitter_archive_imports`).Scan(&status, &filename, &sourcePath, &activeSlot); err != nil {
		t.Fatalf("load import: %v", err)
	}
	if status != "queued" || filename != "twitter-archive.zip" {
		t.Fatalf("import = %s / %s", status, filename)
	}
	if !activeSlot.Valid || activeSlot.Int64 != 1 {
		t.Fatalf("active_slot = %v", activeSlot)
	}
	if !strings.HasPrefix(sourcePath, s.Cfg.DataDir) {
		t.Fatalf("source_path = %q, want under data dir", sourcePath)
	}
	info, err := os.Stat(sourcePath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("stored upload: %v", err)
	}

	// The twitter_archive_import job was enqueued with the import id.
	var kind, payload string
	if err := s.DB.QueryRow(`SELECT kind, payload FROM job_runs`).Scan(&kind, &payload); err != nil {
		t.Fatalf("load job run: %v", err)
	}
	if kind != "twitter_archive_import" || !strings.Contains(payload, `"import_id":1`) {
		t.Fatalf("job = %s %s", kind, payload)
	}
}

func TestAdminTwitterArchivesCreateRejectsInvalidUpload(t *testing.T) {
	s, h := newTwitterArchiveTestServer(t, "")
	session := seedSession(t, s)

	// Plain text file: rejected by content type and extension.
	rec := postArchiveUpload(t, h, "sample.txt", "text/plain", []byte("hello"), session)
	if rec.Code != http.StatusFound {
		t.Fatalf("create = %d", rec.Code)
	}
	if flash := readFlash(t, rec); flash.Alert != "Please upload a valid Twitter archive ZIP file" {
		t.Fatalf("alert = %q", flash.Alert)
	}

	// Malformed params (twitter_archive as a plain string).
	rec = postArchiveUpload(t, h, "", "", nil, session)
	if flash := readFlash(t, rec); flash.Alert != "Please upload a valid Twitter archive ZIP file" {
		t.Fatalf("alert = %q", flash.Alert)
	}
	if countImports(t, s) != 0 {
		t.Fatalf("imports = %d, want 0", countImports(t, s))
	}
}

func TestAdminTwitterArchivesCreateRefusesWhileActive(t *testing.T) {
	s, h := newTwitterArchiveTestServer(t, "")
	session := seedSession(t, s)

	now := time.Now().Unix()
	if _, err := s.DB.Exec(`INSERT INTO twitter_archive_imports
		(status, progress, source_filename, status_message, queued_at, started_at, active_slot, created_at, updated_at)
		VALUES ('running', 40, 'existing.zip', 'Reading archive', ?, ?, 1, ?, ?)`, now-60, now, now, now); err != nil {
		t.Fatalf("insert import: %v", err)
	}

	rec := postArchiveUpload(t, h, "twitter-archive.zip", "application/zip", archiveZipBody(t), session)
	if rec.Code != http.StatusFound {
		t.Fatalf("create = %d", rec.Code)
	}
	flash := readFlash(t, rec)
	if !strings.Contains(strings.ToLower(flash.Alert), "already") || !strings.Contains(strings.ToLower(flash.Alert), "progress") {
		t.Fatalf("alert = %q", flash.Alert)
	}
	if countImports(t, s) != 1 {
		t.Fatalf("imports = %d, want 1", countImports(t, s))
	}
	var jobs int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM job_runs`).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("job runs = %d, want 0", jobs)
	}
}

func TestPublicTwitterArchiveTabsAndPagination(t *testing.T) {
	s, h := newTwitterArchiveTestServer(t, "")

	// 21 tweets: page 1 shows the newest 20, page 2 the oldest.
	for i := 1; i <= 21; i++ {
		seedArchiveTweet(t, s, "pagination-"+strconv.Itoa(i), "tweet", fmt.Sprintf("Archive tweet %02d only", i), int64(1000+i*100))
	}
	seedArchiveTweet(t, s, "reply-1", "reply", "A reply entry", 5000)
	seedArchiveTweet(t, s, "rq-1", "retweet_quote", "A quoted entry", 6000)
	if _, err := s.DB.Exec(`INSERT INTO twitter_archive_likes (tweet_id, full_text, expanded_url, created_at, updated_at)
		VALUES ('777', 'Liked tweet text', 'https://twitter.com/someone/status/777', 1000, 1000)`); err != nil {
		t.Fatalf("insert like: %v", err)
	}
	if _, err := s.DB.Exec(`INSERT INTO twitter_archive_likes (tweet_id, full_text, expanded_url, created_at, updated_at)
		VALUES ('778', 'XSS liked tweet', 'data:text/html,<script>alert(1)</script>', 1001, 1001)`); err != nil {
		t.Fatalf("insert like: %v", err)
	}
	if _, err := s.DB.Exec(`INSERT INTO twitter_archive_connections (account_id, relationship_type, screen_name, created_at, updated_at)
		VALUES ('900', 'follower', 'follower_handle', 1000, 1000)`); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	// Default tab: tweets.
	rec := get(t, h, "/twitter/archive")
	if rec.Code != http.StatusOK {
		t.Fatalf("show = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Tweets", "Replies", "Retweets / Quotes", "Likes", "Archive tweet 21 only", "page=2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("show body misses %q", want)
		}
	}
	if strings.Contains(body, "Archive tweet 01 only") {
		t.Fatalf("page 1 should not contain the oldest tweet")
	}
	if strings.Contains(body, "@follower_handle") {
		t.Fatalf("connections must not leak into the public page")
	}

	rec = get(t, h, "/twitter/archive?page=2")
	if !strings.Contains(rec.Body.String(), "Archive tweet 01 only") || strings.Contains(rec.Body.String(), "Archive tweet 21 only") {
		t.Fatalf("page 2 shows the wrong window")
	}

	// Reply / retweet_quote tabs.
	rec = get(t, h, "/twitter/archive?tab=reply")
	if !strings.Contains(rec.Body.String(), "A reply entry") {
		t.Fatalf("reply tab misses the reply")
	}
	rec = get(t, h, "/twitter/archive?tab=retweet_quote")
	if !strings.Contains(rec.Body.String(), "A quoted entry") {
		t.Fatalf("retweet_quote tab misses the quote")
	}

	// Like tab: safe URLs only.
	rec = get(t, h, "/twitter/archive?tab=like")
	body = rec.Body.String()
	if !strings.Contains(body, "Liked tweet text") || !strings.Contains(body, `href="https://twitter.com/someone/status/777"`) {
		t.Fatalf("like tab misses the like or its link")
	}
	if strings.Contains(body, `href="data:`) {
		t.Fatalf("unsafe expanded_url was rendered")
	}

	// Unknown and connection tabs fall back to tweets.
	for _, tab := range []string{"follower", "following", "bogus"} {
		rec = get(t, h, "/twitter/archive?tab="+tab)
		if !strings.Contains(rec.Body.String(), "Archive tweet 21 only") {
			t.Fatalf("tab=%s did not fall back to tweets", tab)
		}
		if strings.Contains(rec.Body.String(), "@follower_handle") {
			t.Fatalf("tab=%s leaked connection data", tab)
		}
	}

	// Invalid page numbers 404 (will_paginate InvalidPage).
	for _, target := range []string{"/twitter/archive?page=abc", "/twitter/archive?page=0", "/twitter/archive?page=-1"} {
		if rec := get(t, h, target); rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", target, rec.Code)
		}
	}
}

func TestPublicTwitterArchiveRendersMedia(t *testing.T) {
	s, h := newTwitterArchiveTestServer(t, "")
	tweetID := seedArchiveTweet(t, s, "300", "tweet", "Tweet with media", 1000)

	now := time.Now().Unix()
	for i, m := range []struct{ key, filename, contentType string }{
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "tweet-image.jpg", "image/jpeg"},
		{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "tweet-video.mp4", "video/mp4"},
		{"cccccccccccccccccccccccccccccccc", "tweet-file.bin", "application/octet-stream"},
	} {
		res, err := s.DB.Exec(`INSERT INTO files (key, filename, content_type, byte_size, created_at) VALUES (?, ?, ?, 10, ?)`, m.key, m.filename, m.contentType, now+int64(i))
		if err != nil {
			t.Fatalf("insert file: %v", err)
		}
		fileID, _ := res.LastInsertId()
		if _, err := s.DB.Exec(`INSERT INTO attachments (file_id, record_type, record_id, name, created_at) VALUES (?, 'TwitterArchiveTweet', ?, 'media', ?)`, fileID, tweetID, now); err != nil {
			t.Fatalf("insert attachment: %v", err)
		}
	}

	rec := get(t, h, "/twitter/archive")
	if rec.Code != http.StatusOK {
		t.Fatalf("show = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<img src="/files/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`) {
		t.Fatalf("image media not rendered")
	}
	if !strings.Contains(body, `<video src="/files/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" controls`) {
		t.Fatalf("video media not rendered")
	}
	if !strings.Contains(body, `>tweet-file.bin</a>`) {
		t.Fatalf("file media not rendered as a link")
	}
	// tweet_url links out to X.
	if !strings.Contains(body, `href="https://twitter.com/archive_owner/status/300"`) {
		t.Fatalf("View on X link missing")
	}
}

func TestPublicTwitterArchiveRoutePrefix(t *testing.T) {
	s, h := newTwitterArchiveTestServer(t, "blog")
	seedArchiveTweet(t, s, "300", "tweet", "Prefixed archive tweet", 1000)

	rec := get(t, h, "/blog/twitter/archive")
	if rec.Code != http.StatusOK {
		t.Fatalf("prefixed show = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Prefixed archive tweet") {
		t.Fatalf("prefixed page misses the tweet")
	}
	if rec := get(t, h, "/twitter/archive"); rec.Code != http.StatusNotFound {
		t.Fatalf("unprefixed path = %d, want 404 when a prefix is set", rec.Code)
	}

	// The admin page links at the prefixed public path.
	session := seedSession(t, s)
	rec = get(t, h, "/admin/twitter_archives", session)
	if !strings.Contains(rec.Body.String(), `href="/blog/twitter/archive"`) {
		t.Fatalf("admin page does not link the prefixed public path")
	}
}

// TestAdminTwitterArchivesEndToEnd drives the whole chain: multipart upload,
// import row, enqueued job, worker execution, imported tweets.
func TestAdminTwitterArchivesEndToEnd(t *testing.T) {
	s, h := newTwitterArchiveTestServer(t, "")
	session := seedSession(t, s)

	rec := postArchiveUpload(t, h, "twitter-archive.zip", "application/zip", archiveZipBody(t), session)
	if rec.Code != http.StatusFound {
		t.Fatalf("create = %d", rec.Code)
	}

	worker := jobs.NewWorker(s.DB)
	twitterarchive.RegisterImportHandler(worker, s.DB, s.Cfg.DataDir)
	claimed, err := worker.RunOnce(t.Context())
	if err != nil || !claimed {
		t.Fatalf("run once: claimed=%v err=%v", claimed, err)
	}

	var status string
	var tweetsCount, progress int64
	if err := s.DB.QueryRow(`SELECT status, tweets_count, progress FROM twitter_archive_imports`).Scan(&status, &tweetsCount, &progress); err != nil {
		t.Fatalf("load import: %v", err)
	}
	if status != "completed" || tweetsCount != 1 || progress != 100 {
		t.Fatalf("import = %s/%d tweets/%d%%", status, tweetsCount, progress)
	}

	// The imported tweet renders on the public archive page.
	rec = get(t, h, "/twitter/archive")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Original tweet") {
		t.Fatalf("public archive = %d, body misses imported tweet", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "@archive_owner") {
		t.Fatalf("public archive misses the account screen name")
	}
}
