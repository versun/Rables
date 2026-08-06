package transfer

import (
	"archive/zip"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// buildTestZip writes a zip with the given entries (name -> content).
func buildTestZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "upload.zip")
	out, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return zipPath
}

func TestRestoreStorageZip(t *testing.T) {
	dataDir := t.TempDir()
	// One blob already on disk: it must be kept, not overwritten.
	existing := filepath.Join(dataDir, "files", "ab", "ab", "abab0000abab")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("old-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	zipPath := buildTestZip(t, map[string]string{
		"storage/ab/cd/abcdef123456": "new-blob",        // storage/ wrapper stripped
		"ef/gh/efgh654321ef":         "plain-blob",      // bare xx/yy/key layout
		"storage/ab/ab/abab0000abab": "other-content",   // exists already: kept
		"storage/notes/readme.txt":   "not a blob path", // ignored
	})
	copied, kept, err := restoreStorageZip(dataDir, zipPath)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if copied != 2 || kept != 1 {
		t.Errorf("copied=%d kept=%d, want 2/1", copied, kept)
	}
	for rel, want := range map[string]string{
		"ab/cd/abcdef123456": "new-blob",
		"ef/gh/efgh654321ef": "plain-blob",
		"ab/ab/abab0000abab": "old-content",
	} {
		got, err := os.ReadFile(filepath.Join(dataDir, "files", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("blob %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("blob %s = %q, want %q", rel, got, want)
		}
	}
}

func TestRestoreStorageZipRejectsTraversal(t *testing.T) {
	dataDir := t.TempDir()
	zipPath := buildTestZip(t, map[string]string{"../evil.txt": "boom"})
	if _, _, err := restoreStorageZip(dataDir, zipPath); err == nil {
		t.Fatal("expected a traversal error")
	}
}

// railsFixtureSchema mirrors the railsmigrate test fixture schema (the Rails
// tables the migrator reads), trimmed to the columns its SELECTs use.
const railsFixtureSchema = `
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

// buildRailsFixture creates a minimal Rails rables database with one user,
// one html article and one blob.
func buildRailsFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "production.sqlite3")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := old.Exec(railsFixtureSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	stmts := []string{
		`INSERT INTO users VALUES (1, 'versun', 'digest', '2025-02-18 07:32:15', '2025-02-18 07:32:15')`,
		`INSERT INTO articles (id, title, slug, content_type, html_content, status, comment,
		  scheduled_crosspost_platforms, scheduled_send_newsletter, created_at, updated_at) VALUES
		  (1, 'first', 'first', 'html', '<p>raw</p>', 1, 0, '[]', 0, '2025-02-18 07:32:15', '2025-02-18 07:32:15')`,
		`INSERT INTO active_storage_blobs (id, key, filename, content_type, byte_size, checksum, created_at) VALUES
		  (1, 'aaaa1111bbbb', 'pic.png', 'image/png', 11, 'sum', '2025-02-18 07:32:15')`,
	}
	for _, s := range stmts {
		if _, err := old.Exec(s); err != nil {
			t.Fatalf("fixture: %v\n%s", err, s)
		}
	}
	return path
}

func TestRunRailsImport(t *testing.T) {
	ctx := context.Background()
	dstDB, dstDir := newTestDB(t)

	dbPath := buildRailsFixture(t)
	storageZip := buildTestZip(t, map[string]string{"aa/aa/aaaa1111bbbb": "png-content"})

	report, err := runRailsImport(ctx, dstDB, dstDir, ImportRailsPayload{DBPath: dbPath, StoragePath: storageZip})
	if err != nil {
		t.Fatalf("runRailsImport: %v", err)
	}
	if !strings.Contains(report, "RESULT: OK") {
		t.Errorf("report missing RESULT: OK:\n%s", report)
	}
	if !strings.Contains(report, "storage_blobs_copied=1") {
		t.Errorf("report missing storage tally:\n%s", report)
	}

	var name string
	if err := dstDB.QueryRow(`SELECT user_name FROM users WHERE id = 1`).Scan(&name); err != nil {
		t.Fatalf("user not migrated: %v", err)
	}
	if name != "versun" {
		t.Errorf("user_name = %q, want versun", name)
	}
	var html string
	if err := dstDB.QueryRow(`SELECT content_html FROM articles WHERE id = 1`).Scan(&html); err != nil {
		t.Fatalf("article not migrated: %v", err)
	}
	if html != "<p>raw</p>" {
		t.Errorf("content_html = %q, want <p>raw</p>", html)
	}
	blob, err := os.ReadFile(filepath.Join(dstDir, "files", "aa", "aa", "aaaa1111bbbb"))
	if err != nil {
		t.Fatalf("blob not restored: %v", err)
	}
	if string(blob) != "png-content" {
		t.Errorf("blob content = %q, want png-content", blob)
	}
}

func TestRunRailsImportWithoutStorage(t *testing.T) {
	ctx := context.Background()
	dstDB, dstDir := newTestDB(t)
	dbPath := buildRailsFixture(t)

	if _, err := runRailsImport(ctx, dstDB, dstDir, ImportRailsPayload{DBPath: dbPath}); err != nil {
		t.Fatalf("runRailsImport: %v", err)
	}
	var n int
	if err := dstDB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
}

func TestRunRailsImportRejectsGoDatabase(t *testing.T) {
	ctx := context.Background()
	dstDB, dstDir := newTestDB(t)

	// A Go rables database lacks the Rails tables and must be refused.
	goDB, goDir := newTestDB(t)
	copyPath := filepath.Join(t.TempDir(), "rables.db")
	if err := vacuumInto(ctx, goDB, copyPath); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	_ = goDir

	if _, err := runRailsImport(ctx, dstDB, dstDir, ImportRailsPayload{DBPath: copyPath}); err == nil {
		t.Fatal("expected an error for a Go database")
	}
}
