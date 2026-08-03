package db

import (
	"database/sql"
	"math"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"rables/internal/db/query"
	"rables/migrations"
)

// wantTables lists every table from plan §3 (26 tables; the §7 DoD says 25,
// §3 is authoritative).
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
	"twitter_archive_tweets",
	"twitter_archive_connections",
	"twitter_archive_likes",
	"twitter_archive_imports",
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
		"idx_tai_active_slot",
		"idx_job_runs_due",
		"idx_files_filename",
	} {
		if _, ok := indexes[idx]; !ok {
			t.Errorf("missing index %q", idx)
		}
	}
	for _, idx := range []string{"idx_comments_ext_article", "idx_comments_ext_commentable", "idx_tai_active_slot"} {
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
	if got := len(migs); got != 1 {
		t.Fatalf("collected %d migrations, want 1", got)
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
