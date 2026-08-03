package railsmigrate

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"rables/internal/db"
)

// railsSchema is the minimal old-schema subset the migrator reads, matching
// RAILS_ROOT/db/schema.rb (legacy settings columns included on purpose).
const railsSchema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, user_name TEXT, password_digest TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT, slug TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE action_text_rich_texts (id INTEGER PRIMARY KEY, record_type TEXT, record_id INTEGER, name TEXT, body TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE active_storage_blobs (id INTEGER PRIMARY KEY, key TEXT, filename TEXT, content_type TEXT, byte_size INTEGER, checksum TEXT, metadata TEXT, service_name TEXT, created_at TEXT);
CREATE TABLE active_storage_attachments (id INTEGER PRIMARY KEY, name TEXT, record_type TEXT, record_id INTEGER, blob_id INTEGER, created_at TEXT);
CREATE TABLE active_storage_variant_records (id INTEGER PRIMARY KEY, blob_id INTEGER, variation_digest TEXT);
CREATE TABLE articles (id INTEGER PRIMARY KEY, title TEXT, slug TEXT, content_type TEXT, description TEXT, excerpt TEXT,
  html_content TEXT, meta_description TEXT, meta_title TEXT, meta_image TEXT,
  source_author TEXT, source_url TEXT, source_content TEXT, status INTEGER, comment INTEGER,
  scheduled_at TEXT, scheduled_crosspost_platforms TEXT, scheduled_send_newsletter INTEGER,
  created_at TEXT, updated_at TEXT);
CREATE TABLE pages (id INTEGER PRIMARY KEY, title TEXT, slug TEXT, content_type TEXT, html_content TEXT,
  redirect_url TEXT, page_order INTEGER, status INTEGER, comment INTEGER, scheduled_at TEXT,
  created_at TEXT, updated_at TEXT);
CREATE TABLE comments (id INTEGER PRIMARY KEY, commentable_type TEXT, commentable_id INTEGER, article_id INTEGER,
  parent_id INTEGER, author_name TEXT, author_email TEXT, author_url TEXT, author_username TEXT,
  author_avatar_url TEXT, content TEXT, status INTEGER, platform TEXT, external_id TEXT, url TEXT,
  published_at TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE article_tags (id INTEGER PRIMARY KEY, article_id INTEGER, tag_id INTEGER, created_at TEXT, updated_at TEXT);
CREATE TABLE subscribers (id INTEGER PRIMARY KEY, email TEXT, confirmation_token TEXT, unsubscribe_token TEXT,
  confirmed_at TEXT, unsubscribed_at TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE subscriber_tags (id INTEGER PRIMARY KEY, subscriber_id INTEGER, tag_id INTEGER, created_at TEXT, updated_at TEXT);
CREATE TABLE social_media_posts (id INTEGER PRIMARY KEY, article_id INTEGER, platform TEXT, url TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE redirects (id INTEGER PRIMARY KEY, regex TEXT, replacement TEXT, enabled INTEGER, permanent INTEGER, created_at TEXT, updated_at TEXT);
CREATE TABLE static_files (id INTEGER PRIMARY KEY, filename TEXT, description TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE settings (id INTEGER PRIMARY KEY, title TEXT, description TEXT, author TEXT, url TEXT, time_zone TEXT,
  head_code TEXT, custom_css TEXT, tool_code TEXT, giscus TEXT, social_links TEXT, setup_completed INTEGER,
  github_token TEXT, static_files TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE newsletter_settings (id INTEGER PRIMARY KEY, enabled INTEGER, provider TEXT, from_email TEXT,
  smtp_address TEXT, smtp_port INTEGER, smtp_user_name TEXT, smtp_password TEXT, smtp_domain TEXT,
  smtp_authentication TEXT, smtp_enable_starttls INTEGER, created_at TEXT, updated_at TEXT);
CREATE TABLE crossposts (id INTEGER PRIMARY KEY, platform TEXT, enabled INTEGER, api_key TEXT, api_key_secret TEXT,
  access_token TEXT, access_token_secret TEXT, client_id TEXT, client_key TEXT, client_secret TEXT,
  app_password TEXT, refresh_token TEXT, token_expires_at TEXT, server_url TEXT, username TEXT,
  max_characters INTEGER, auto_fetch_comments INTEGER, comment_fetch_schedule TEXT, settings TEXT,
  created_at TEXT, updated_at TEXT);
CREATE TABLE listmonks (id INTEGER PRIMARY KEY, url TEXT, username TEXT, api_key TEXT, list_id INTEGER,
  template_id INTEGER, enabled INTEGER, created_at TEXT, updated_at TEXT);
CREATE TABLE twitter_syncs (id INTEGER PRIMARY KEY, enabled INTEGER, username TEXT, user_id TEXT, since_id TEXT,
  start_date TEXT, sync_schedule TEXT, last_synced_at TEXT, last_error TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE twitter_archive_tweets (id INTEGER PRIMARY KEY, tweet_id TEXT, screen_name TEXT, full_text TEXT,
  entry_type TEXT, tweeted_at TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE twitter_archive_connections (id INTEGER PRIMARY KEY, account_id TEXT, screen_name TEXT,
  user_link TEXT, relationship_type TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE twitter_archive_likes (id INTEGER PRIMARY KEY, tweet_id TEXT, full_text TEXT, expanded_url TEXT,
  created_at TEXT, updated_at TEXT);
CREATE TABLE twitter_archive_imports (id INTEGER PRIMARY KEY, status TEXT, progress INTEGER, total_items_count INTEGER,
  tweets_count INTEGER, followers_count INTEGER, following_count INTEGER, likes_count INTEGER,
  source_filename TEXT, source_path TEXT, status_message TEXT, error_message TEXT,
  queued_at TEXT, started_at TEXT, finished_at TEXT, active_slot INTEGER, created_at TEXT, updated_at TEXT);
`

const (
	ts1 = "2025-02-18 07:32:15"        // plain
	ts2 = "2025-03-07 12:42:19.715000" // fractional
	ts3 = "2025-04-09 00:26:00"        // scheduled
)

func unix1() int64 { return time.Date(2025, 2, 18, 7, 32, 15, 0, time.UTC).Unix() }
func unix2() int64 { return time.Date(2025, 3, 7, 12, 42, 19, 715000000, time.UTC).Unix() }

// buildFixture creates the synthetic old database and returns its path.
func buildFixture(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/old.sqlite3"
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := old.Exec(railsSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	stmts := []string{
		// users / tags
		`INSERT INTO users VALUES (1, 'versun', '$2a$12$digest', '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO tags VALUES (1, 'go', 'go', '` + ts1 + `', '` + ts1 + `'), (2, 'rails', 'rails', '` + ts1 + `', '` + ts1 + `')`,
		// blobs: 1 image, 2 pdf, 3 jpeg variant of 1
		`INSERT INTO active_storage_blobs (id, key, filename, content_type, byte_size, checksum, created_at) VALUES
		  (1, 'aaaa1111bbbb', 'pic.png', 'image/png', 100, 'sum1', '` + ts1 + `'),
		  (2, 'cccc2222dddd', 'doc.pdf', 'application/pdf', 200, 'sum2', '` + ts1 + `'),
		  (3, 'eeee3333ffff', 'pic.png', 'image/jpeg', 50, 'sum3', '` + ts1 + `')`,
		`INSERT INTO active_storage_variant_records VALUES (1, 1, 'digestxyz')`,
		// rich texts: rt1 -> Article 1, rt2 -> Article 3, rt3 -> Page 1
		`INSERT INTO action_text_rich_texts (id, record_type, record_id, name, body, created_at, updated_at) VALUES
		  (1, 'Article', 1, 'content', '<p>hi</p><action-text-attachment sgid="` + makeSGID("gid://rables/ActiveStorage::Blob/1?expires_in") + `" content-type="image/png"></action-text-attachment><action-text-attachment sgid="` + makeSGID("gid://rables/ActiveStorage::Blob/2?expires_in") + `" content-type="application/pdf"></action-text-attachment><action-text-attachment sgid="broken"></action-text-attachment><p><img src="/rails/active_storage/blobs/redirect/` + realSignedID + `/pic.png"/></p>', '` + ts1 + `', '` + ts1 + `'),
		  (2, 'Article', 3, 'content', '<p>plain body</p>', '` + ts1 + `', '` + ts1 + `'),
		  (3, 'Page', 1, 'content', '<p>page body</p>', '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO active_storage_attachments (id, name, record_type, record_id, blob_id, created_at) VALUES
		  (1, 'embeds', 'ActionText::RichText', 1, 1, '` + ts1 + `'),
		  (2, 'embeds', 'ActionText::RichText', 1, 2, '` + ts1 + `'),
		  (3, 'image', 'ActiveStorage::VariantRecord', 1, 3, '` + ts1 + `'),
		  (4, 'file', 'StaticFile', 1, 2, '` + ts1 + `'),
		  (5, 'media', 'TwitterArchiveTweet', 1, 1, '` + ts1 + `')`,
		// articles: 1 rich_text w/ attachments, 2 html, 3 plain rich_text
		`INSERT INTO articles (id, title, slug, content_type, html_content, status, comment,
		  scheduled_crosspost_platforms, scheduled_send_newsletter, created_at, updated_at) VALUES
		  (1, 'first', 'first', 'rich_text', NULL, 1, 1, '[]', 0, '` + ts1 + `', '` + ts2 + `'),
		  (2, 'second', 'second', 'html', '<p>raw <i>html</i></p>', 1, 0, '[]', 0, '` + ts1 + `', '` + ts1 + `'),
		  (3, 'third', 'third', 'rich_text', NULL, 2, 0, '["twitter"]', 1, '` + ts1 + `', '` + ts1 + `')`,
		`UPDATE articles SET scheduled_at = '` + ts3 + `' WHERE id = 3`,
		// pages: 1 rich_text, 1 html
		`INSERT INTO pages (id, title, slug, content_type, html_content, page_order, status, comment, created_at, updated_at) VALUES
		  (1, 'about', 'about', 'rich_text', NULL, 1, 1, 0, '` + ts1 + `', '` + ts1 + `'),
		  (2, 'sup', 'supporters', 'html', '<div>sponsor</div>', 2, 1, 0, '` + ts1 + `', '` + ts1 + `')`,
		// comments: root + child + external
		`INSERT INTO comments (id, commentable_type, commentable_id, article_id, parent_id, author_name,
		  content, status, platform, external_id, published_at, created_at, updated_at) VALUES
		  (1, 'Article', 1, 1, NULL, 'alice', 'root', 1, NULL, NULL, '` + ts1 + `', '` + ts1 + `', '` + ts1 + `'),
		  (2, 'Article', 1, 1, 1, 'bob', 'child', 0, NULL, NULL, NULL, '` + ts2 + `', '` + ts2 + `'),
		  (3, 'Article', 1, 1, NULL, 'bot', 'ext', 1, 'mastodon', 'm123', '` + ts2 + `', '` + ts2 + `', '` + ts2 + `')`,
		`INSERT INTO article_tags VALUES (1, 1, 1, '` + ts1 + `', '` + ts1 + `'), (2, 1, 2, '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO subscribers VALUES
		  (1, 'a@x.test', 'ctoken1', 'utoken1', '` + ts1 + `', NULL, '` + ts1 + `', '` + ts1 + `'),
		  (2, 'b@x.test', 'ctoken2', 'utoken2', '` + ts1 + `', '` + ts2 + `', '` + ts1 + `', '` + ts2 + `')`,
		`INSERT INTO subscriber_tags VALUES (1, 1, 1, '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO social_media_posts VALUES (1, 1, 'twitter', 'https://x.test/1', '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO redirects VALUES (1, '^/old', '/new', 1, 0, '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO static_files VALUES (1, 'doc.pdf', 'a doc', '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO settings (id, title, description, author, url, time_zone, head_code, custom_css,
		  tool_code, giscus, social_links, setup_completed, github_token, static_files, created_at, updated_at) VALUES
		  (1, 'My Blog', 'desc', 'versun', 'https://blog.test', 'Asia/Shanghai', '<meta>', 'css', 'tool',
		   'giscus', '[{"name":"x"}]', 1, 'SECRET-not-migrated', '{}', '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO newsletter_settings VALUES (1, 1, 'native', 'from@x.test', 'smtp.x.test', 2525, 'u', 'p',
		  'x.test', 'login', 0, '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO crossposts VALUES (1, 'mastodon', 1, NULL, NULL, 'token', NULL, NULL, NULL, NULL, NULL,
		  NULL, '` + ts2 + `', 'https://m.test', 'me', 500, 1, 'daily', '{}', '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO listmonks VALUES (1, 'https://lm.test', 'api', 'key', 3, 2, 1, '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO twitter_syncs VALUES (1, 1, 'me', '123', '456', '2025-01-01', 'hourly', '` + ts2 + `',
		  NULL, '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO twitter_archive_tweets VALUES (1, 't100', 'me', 'hello', 'tweet', '` + ts1 + `', '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO twitter_archive_connections VALUES
		  (1, 'acc1', 'foo', NULL, 'follower', '` + ts1 + `', '` + ts1 + `'),
		  (2, 'acc2', 'bar', 'https://x.test/bar', 'following', '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO twitter_archive_likes VALUES (1, 't200', 'liked', 'https://x.test/t200', '` + ts1 + `', '` + ts1 + `')`,
		`INSERT INTO twitter_archive_imports VALUES (1, 'completed', 100, 5, 1, 1, 1, 1, 'archive.zip', '/tmp/a.zip',
		  'done', NULL, '` + ts1 + `', '` + ts1 + `', '` + ts2 + `', NULL, '` + ts1 + `', '` + ts1 + `')`,
	}
	for _, s := range stmts {
		if _, err := old.Exec(s); err != nil {
			t.Fatalf("fixture: %v\n%s", err, s)
		}
	}
	return path
}

// openRO opens the fixture read-only, mirroring the tool's DSN.
func openRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func tableReport(rep *Report, name string) *TableReport {
	for _, tr := range rep.Tables {
		if tr.Table == name {
			return tr
		}
	}
	return nil
}

func TestRunMigration(t *testing.T) {
	oldPath := buildFixture(t)
	oldDB := openRO(t, oldPath)
	newDB, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer newDB.Close()
	ctx := context.Background()

	rep, err := Run(ctx, oldDB, newDB, Options{Out: io.Discard})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Mismatch() {
		t.Fatalf("unexpected mismatch: %+v", rep.Tables)
	}

	// per-table counts: old / inserted / skipped / transformed
	want := map[string][4]int64{
		"users": {1, 1, 0, 0}, "tags": {2, 2, 0, 0}, "articles": {3, 3, 0, 0},
		"pages": {2, 2, 0, 0}, "comments": {3, 3, 0, 0}, "article_tags": {2, 2, 0, 0},
		"subscribers": {2, 2, 0, 0}, "subscriber_tags": {1, 1, 0, 0},
		"social_media_posts": {1, 1, 0, 0}, "redirects": {1, 1, 0, 0},
		"files": {3, 3, 0, 0}, "attachments": {5, 3, 0, 2}, "static_files": {1, 1, 0, 0},
		"settings": {1, 1, 0, 0}, "newsletter_settings": {1, 1, 0, 0},
		"crossposts": {1, 1, 0, 0}, "listmonks": {1, 1, 0, 0}, "twitter_syncs": {1, 1, 0, 0},
		"twitter_archive_tweets": {1, 1, 0, 0}, "twitter_archive_connections": {2, 2, 0, 0},
		"twitter_archive_likes": {1, 1, 0, 0}, "twitter_archive_imports": {1, 1, 0, 0},
	}
	for name, w := range want {
		tr := tableReport(rep, name)
		if tr == nil {
			t.Fatalf("no report for %s", name)
		}
		if got := [4]int64{tr.Old, tr.Inserted, tr.Skipped, tr.Transformed}; got != w {
			t.Errorf("%s counts = %v, want %v", name, got, w)
		}
		if tr.NewTotal != tr.Expected {
			t.Errorf("%s total %d != expected %d", name, tr.NewTotal, tr.Expected)
		}
	}

	// attachment rewrite: 3 resolved (img + link + old-style img), 1 kept
	if rep.Rewrite.Rewritten != 3 || len(rep.Rewrite.Kept) != 1 {
		t.Errorf("rewrite stats: %+v", rep.Rewrite)
	}
	if rep.Rewrite.Kept[0].Record != "Article/1" {
		t.Errorf("kept record: %+v", rep.Rewrite.Kept[0])
	}

	q := func(query string, args ...any) *sql.Row {
		return newDB.QueryRowContext(ctx, query, args...)
	}
	var s string
	var n int64
	var nn sql.NullInt64

	// article 1: rewritten body
	if err := q("SELECT content_html FROM articles WHERE id = 1").Scan(&s); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<img src="/files/aaaa1111bbbb" alt="pic.png" loading="lazy"/>`,
		`<a href="/files/cccc2222dddd">doc.pdf</a>`,
		`<img src="/files/aaaa1111bbbb"/>`,       // old-style img rewritten
		`<action-text-attachment sgid="broken">`, // unresolvable kept
	} {
		if !strings.Contains(s, want) {
			t.Errorf("article 1 content_html missing %q:\n%s", want, s)
		}
	}
	// article 2: html body byte-identical
	if err := q("SELECT content_html FROM articles WHERE id = 2").Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != "<p>raw <i>html</i></p>" {
		t.Errorf("article 2 content_html = %q", s)
	}
	// article 3: scheduled_at and snapshots carried, times converted
	if err := q("SELECT scheduled_at, scheduled_crosspost_platforms, scheduled_send_newsletter, created_at, updated_at FROM articles WHERE id = 3").
		Scan(&nn, &s, &n, new(int64), new(int64)); err != nil {
		t.Fatal(err)
	}
	wantSched := time.Date(2025, 4, 9, 0, 26, 0, 0, time.UTC).Unix()
	if !nn.Valid || nn.Int64 != wantSched || s != `["twitter"]` || n != 1 {
		t.Errorf("article 3 schedule fields: %v %q %d", nn, s, n)
	}
	if err := q("SELECT created_at FROM articles WHERE id = 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != unix1() {
		t.Errorf("article 1 created_at = %d, want %d", n, unix1())
	}

	// comments: parent backfilled in pass 2
	if err := q("SELECT parent_id FROM comments WHERE id = 2").Scan(&nn); err != nil {
		t.Fatal(err)
	}
	if !nn.Valid || nn.Int64 != 1 {
		t.Errorf("comment 2 parent_id = %v", nn)
	}

	// files: variant linked
	if err := q("SELECT variant_of FROM files WHERE id = 3").Scan(&nn); err != nil {
		t.Fatal(err)
	}
	if !nn.Valid || nn.Int64 != 1 {
		t.Errorf("file 3 variant_of = %v", nn)
	}

	// attachments: richtext rows remapped to Article/1, twitter archive carried
	var rt string
	var rid int64
	if err := q("SELECT record_type, record_id FROM attachments WHERE id = 1").Scan(&rt, &rid); err != nil {
		t.Fatal(err)
	}
	if rt != "Article" || rid != 1 {
		t.Errorf("attachment 1 = %s/%d", rt, rid)
	}
	if err := q("SELECT record_type FROM attachments WHERE id = 5").Scan(&rt); err != nil {
		t.Fatal(err)
	}
	if rt != "TwitterArchiveTweet" {
		t.Errorf("attachment 5 record_type = %s", rt)
	}

	// static_files via attachment join
	if err := q("SELECT file_id FROM static_files WHERE id = 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("static_file 1 file_id = %d", n)
	}

	// subscribers keep old tokens
	if err := q("SELECT confirmation_token, unsubscribe_token FROM subscribers WHERE id = 1").Scan(&rt, &s); err != nil {
		t.Fatal(err)
	}
	if rt != "ctoken1" || s != "utoken1" {
		t.Errorf("subscriber tokens = %q %q", rt, s)
	}

	// settings: kept fields carried, legacy columns gone (column absent)
	if err := q("SELECT title, time_zone, setup_completed FROM settings WHERE id = 1").Scan(&s, &rt, &n); err != nil {
		t.Fatal(err)
	}
	if s != "My Blog" || rt != "Asia/Shanghai" || n != 1 {
		t.Errorf("settings = %q %q %d", s, rt, n)
	}

	// crossposts: token_expires_at converted
	if err := q("SELECT token_expires_at FROM crossposts WHERE id = 1").Scan(&nn); err != nil {
		t.Fatal(err)
	}
	if !nn.Valid || nn.Int64 != unix2() {
		t.Errorf("crosspost token_expires_at = %v, want %d", nn, unix2())
	}

	// twitter_syncs: start_date stays TEXT, last_synced_at converted
	if err := q("SELECT start_date, last_synced_at FROM twitter_syncs WHERE id = 1").Scan(&s, &nn); err != nil {
		t.Fatal(err)
	}
	if s != "2025-01-01" || !nn.Valid || nn.Int64 != unix2() {
		t.Errorf("twitter_syncs = %q %v", s, nn)
	}

	// sessions must not be migrated (decision log)
	if err := q("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("sessions migrated: %d rows", n)
	}

	// second run: fully idempotent
	rep2, err := Run(ctx, oldDB, newDB, Options{Out: io.Discard})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rep2.Mismatch() {
		t.Fatalf("second run mismatch")
	}
	for _, tr := range rep2.Tables {
		if tr.Inserted != 0 || tr.Skipped != tr.Expected {
			t.Errorf("second run %s: inserted=%d skipped=%d expected=%d", tr.Table, tr.Inserted, tr.Skipped, tr.Expected)
		}
	}
	// singleton rows skipped on re-run must be called out in the notes
	var foundNote bool
	for _, note := range rep2.Notes {
		if strings.HasPrefix(note, "settings: row already exists") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Errorf("second run missing singleton skip note: %v", rep2.Notes)
	}
	// parent backfill must not clobber on re-run
	if err := q("SELECT parent_id FROM comments WHERE id = 2").Scan(&nn); err != nil {
		t.Fatal(err)
	}
	if !nn.Valid || nn.Int64 != 1 {
		t.Errorf("comment 2 parent_id after re-run = %v", nn)
	}
}

// A wrong -old path (e.g. a 0-byte file sqlite accepts as an empty database)
// must fail fast with a clear message instead of "no such table" midway.
func TestRunMissingTables(t *testing.T) {
	oldDB, err := sql.Open("sqlite", t.TempDir()+"/empty.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer oldDB.Close()
	newDB, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer newDB.Close()

	_, err = Run(context.Background(), oldDB, newDB, Options{Out: io.Discard})
	if err == nil {
		t.Fatal("expected an error for an empty old database")
	}
	if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "users") {
		t.Errorf("error should name the missing tables, got: %v", err)
	}
}

func TestVerifyFiles(t *testing.T) {
	oldPath := buildFixture(t)
	oldDB := openRO(t, oldPath)
	dir := t.TempDir()
	newDB, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer newDB.Close()

	rep, err := Run(context.Background(), oldDB, newDB, Options{Out: io.Discard, DataDir: dir, VerifyFiles: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verified == nil || rep.Verified.Total != 3 {
		t.Fatalf("verify report: %+v", rep.Verified)
	}
	if len(rep.Verified.Missing) != 3 {
		t.Errorf("expected 3 missing files, got %v", rep.Verified.Missing)
	}

	// a blob present on disk must drop out of the missing list
	p := filepath.Join(dir, "files", "aa", "aa")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "aaaa1111bbbb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err = Run(context.Background(), oldDB, newDB, Options{Out: io.Discard, DataDir: dir, VerifyFiles: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Verified.Missing) != 2 {
		t.Errorf("expected 2 missing files with one on disk, got %v", rep.Verified.Missing)
	}
}
