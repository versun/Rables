package db

import (
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"rables/internal/db/query"
	"rables/migrations"
)

// wantTables lists every table from plan §3 minus the twitter archive tables
// dropped by migration 0006.
var wantTables = []string{
	"users",
	"sessions",
	"articles",
	"pages",
	"tags",
	"article_tags",
	"comments",
	"subscribers",
	"subscriber_tags",
	"social_media_posts",
	"redirects",
	"settings",
	"newsletter_settings",
	"crossposts",
	"listmonks",
	"twitter_syncs",
	"activity_logs",
	"files",
	"attachments",
	"static_files",
	"job_runs",
	"kv",
}

func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func userObjects(t *testing.T, db *sql.DB, objType string) map[string]string {
	t.Helper()
	rows, err := db.Query(
		`SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type = ? AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'goose_%'`,
		objType,
	)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	objects := map[string]string{}
	for rows.Next() {
		var name, sqlText string
		if err := rows.Scan(&name, &sqlText); err != nil {
			t.Fatalf("scan: %v", err)
		}
		objects[name] = sqlText
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return objects
}

func TestOpenCreatesAllTables(t *testing.T) {
	db := open(t)
	tables := userObjects(t, db, "table")
	for _, table := range wantTables {
		if _, ok := tables[table]; !ok {
			t.Errorf("missing table %q", table)
		}
	}
	if got, want := len(tables), len(wantTables); got != want {
		t.Errorf("table count = %d, want %d", got, want)
	}
}

// TestOpenCreatesIndexes spot-checks the important indexes, including the two
// partial unique ones.
func TestOpenCreatesIndexes(t *testing.T) {
	db := open(t)
	indexes := userObjects(t, db, "index")
	for _, idx := range []string{
		"idx_articles_status",
		"idx_articles_status_created",
		"idx_comments_commentable",
		"idx_comments_ext_article",
		"idx_comments_ext_commentable",
		"idx_job_runs_due",
		"idx_files_filename",
	} {
		if _, ok := indexes[idx]; !ok {
			t.Errorf("missing index %q", idx)
		}
	}
	for _, idx := range []string{"idx_comments_ext_article", "idx_comments_ext_commentable"} {
		if sql := indexes[idx]; !strings.Contains(sql, "UNIQUE") || !strings.Contains(sql, "WHERE") {
			t.Errorf("index %q should be a partial UNIQUE index, got: %s", idx, sql)
		}
	}
}

func TestForeignKeysEnabled(t *testing.T) {
	db := open(t)
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
	_, err := db.Exec(
		"INSERT INTO sessions (token, user_id, created_at, updated_at) VALUES ('tok', 999, 1, 1)",
	)
	if err == nil {
		t.Fatal("expected foreign key violation inserting session with missing user")
	}
}

func TestJournalModeWAL(t *testing.T) {
	db := open(t)
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestOpenIdempotent(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db, err = Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db.Close()
	if got := len(userObjects(t, db, "table")); got != len(wantTables) {
		t.Errorf("table count after reopen = %d, want %d", got, len(wantTables))
	}
}

// TestOpenSpecialCharPath guards the net/url DSN construction: a data
// directory whose name contains characters that are special in a file: URI
// ("?", "%", space, non-ASCII) must open and land at the intended path.
func TestOpenSpecialCharPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "weird ?% dir 数据")
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := os.Stat(filepath.Join(dir, "rables.db")); err != nil {
		t.Fatalf("database file not at the expected path: %v", err)
	}
	var one int
	if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
}

// TestMigrationsValidate is the library-level equivalent of `goose validate`:
// it collects/parses every migration from the embedded FS, then runs the full
// Up -> Down -> Up cycle against a scratch database.
func TestMigrationsValidate(t *testing.T) {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	migs, err := goose.CollectMigrations(".", 0, math.MaxInt64)
	if err != nil {
		t.Fatalf("CollectMigrations: %v", err)
	}
	if got := len(migs); got != 7 {
		t.Fatalf("collected %d migrations, want 7", got)
	}

	db := open(t)
	if err := goose.DownTo(db, ".", 0); err != nil {
		t.Fatalf("DownTo 0: %v", err)
	}
	if got := len(userObjects(t, db, "table")); got != 0 {
		t.Errorf("tables left after DownTo 0: %d, want 0", got)
	}
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("re-Up: %v", err)
	}
	if got := len(userObjects(t, db, "table")); got != len(wantTables) {
		t.Errorf("tables after re-Up: %d, want %d", got, len(wantTables))
	}
}

// TestOpenRestrictsPermissions checks that Open creates the data directory
// 0700 and pins the database file (session tokens, API keys) to 0600.
func TestOpenRestrictsPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("data dir mode = %o, want 700", got)
	}
	info, err = os.Stat(filepath.Join(dir, "rables.db"))
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("database mode = %o, want 600", got)
	}
}

// TestOpenRestrictsExistingDatabase covers upgrades: a database file left
// over at 0644 by an older release is tightened to 0600 on open.
func TestOpenRestrictsExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	path := filepath.Join(dir, "rables.db")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod 644: %v", err)
	}

	db, err = Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("database mode after reopen = %o, want 600", got)
	}
}

// TestOpenRestrictsWALPermissions checks that the WAL sidecar files, which
// hold all new writes (session pages included) while the DB runs, are pinned
// to 0600 alongside the main database file.
func TestOpenRestrictsWALPermissions(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Force a write so the -wal file exists while the connection is open.
	_, err = db.Exec(
		"INSERT INTO articles (title, slug, status, created_at, updated_at) VALUES ('t', 't', 1, 10, 20)",
	)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Stat(filepath.Join(dir, "rables.db"+suffix))
		if err != nil {
			t.Fatalf("stat rables.db%s: %v", suffix, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("rables.db%s mode = %o, want 600", suffix, got)
		}
	}
}

// TestOpenRestrictsExistingDataDir covers upgrades: a data directory left at
// 0755 is tightened to 0700 on open (MkdirAll alone is a no-op there).
func TestOpenRestrictsExistingDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod 755: %v", err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("data dir mode = %o, want 700", got)
	}
}

// TestGeneratedQuerySmoke exercises the sqlc-generated code against the real
// schema.
func TestGeneratedQuerySmoke(t *testing.T) {
	db := open(t)
	_, err := db.Exec(
		"INSERT INTO articles (title, slug, status, created_at, updated_at) VALUES ('hello', 'hello', 1, 10, 20)",
	)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	article, err := query.New(db).GetArticleByID(t.Context(), 1)
	if err != nil {
		t.Fatalf("GetArticleByID: %v", err)
	}
	if !article.Title.Valid || article.Title.String != "hello" || article.Status != 1 {
		t.Errorf("unexpected article: %+v", article)
	}
}
