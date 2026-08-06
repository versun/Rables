package transfer

import (
	"archive/zip"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedSource fills a fresh database with a bit of every content kind plus
// rows in runtime tables that must NOT be imported.
func seedSource(t *testing.T, database *sql.DB, dataDir string) {
	t.Helper()
	stmts := []string{
		`INSERT INTO users (id, user_name, password_digest, created_at, updated_at) VALUES (1, 'versun', 'digest', 1700000000, 1700000000)`,
		`INSERT INTO tags (id, name, slug, created_at, updated_at) VALUES (1, 'go', 'go', 1700000000, 1700000000)`,
		`INSERT INTO articles (id, title, slug, content_html, status, created_at, updated_at) VALUES
		  (1, 'from-src', 'a1', '<p>one</p>', 1, 1700000000, 1700000000),
		  (2, 'second', 'a2', '<p>two</p>', 1, 1700000000, 1700000000)`,
		`INSERT INTO article_tags (id, article_id, tag_id, created_at, updated_at) VALUES (1, 1, 1, 1700000000, 1700000000)`,
		`INSERT INTO comments (id, commentable_type, commentable_id, article_id, parent_id, author_name, content, status, created_at, updated_at) VALUES
		  (1, 'Article', 1, 1, NULL, 'alice', 'root', 1, 1700000000, 1700000000),
		  (2, 'Article', 1, 1, 1, 'bob', 'child', 0, 1700000000, 1700000000)`,
		`INSERT INTO subscribers (id, email, confirmation_token, unsubscribe_token, created_at, updated_at) VALUES
		  (1, 'a@x.test', 'ctoken', 'utoken', 1700000000, 1700000000)`,
		`INSERT INTO files (id, key, filename, content_type, byte_size, created_at) VALUES
		  (1, 'aaaa1111bbbb', 'pic.png', 'image/png', 11, 1700000000)`,
		`INSERT INTO settings (id, title, time_zone, setup_completed, created_at, updated_at) VALUES
		  (1, 'Src Title', 'Asia/Shanghai', 1, 1700000000, 1700000000)`,
		// runtime tables: must not be imported
		`INSERT INTO activity_logs (id, level, action, created_at, updated_at) VALUES (1, 0, 'x', 1700000000, 1700000000)`,
		`INSERT INTO job_runs (id, kind, run_at, created_at, updated_at) VALUES (1, 'export', 1700000000, 1700000000, 1700000000)`,
		`INSERT INTO sessions (id, token, user_id, created_at, updated_at) VALUES (1, 'tok', 1, 1700000000, 1700000000)`,
		`INSERT INTO kv (key, value, updated_at) VALUES ('k', 'v', 1700000000)`,
	}
	for _, s := range stmts {
		if _, err := database.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	blobPath := filepath.Join(dataDir, "files", "aa", "aa", "aaaa1111bbbb")
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, []byte("png-content"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// exportZip runs the bundle exporter on the source and returns the zip path.
func exportZip(t *testing.T, database *sql.DB, dataDir string) string {
	t.Helper()
	zipPath, err := (&BundleExporter{DB: database, DataDir: dataDir}).Generate(context.Background())
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return zipPath
}

func TestDBImporterRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	seedSource(t, srcDB, srcDir)
	zipPath := exportZip(t, srcDB, srcDir)

	dstDB, dstDir := newTestDB(t)
	// Overlapping row (same id, stale content) and a row the source lacks.
	if _, err := dstDB.Exec(`INSERT INTO articles (id, title, slug, content_html, status, created_at, updated_at) VALUES
		(1, 'stale', 'a1', '<p>old</p>', 0, 1700000001, 1700000001),
		(99, 'keep-me', 'keep', '<p>k</p>', 1, 1700000001, 1700000001)`); err != nil {
		t.Fatal(err)
	}
	if _, err := dstDB.Exec(`INSERT INTO settings (id, title, created_at, updated_at) VALUES (1, 'Dst Title', 1700000001, 1700000001)`); err != nil {
		t.Fatal(err)
	}

	res, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, zipPath)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Rows["articles"] != 2 || res.Rows["tags"] != 1 || res.Rows["users"] != 1 {
		t.Errorf("rows = %v, want articles:2 tags:1 users:1", res.Rows)
	}
	if res.BlobsCopied != 1 || res.BlobsKept != 0 {
		t.Errorf("blobs copied=%d kept=%d, want 1/0", res.BlobsCopied, res.BlobsKept)
	}

	// Same-id row was overwritten, the extra row survived.
	var title string
	if err := dstDB.QueryRow(`SELECT title FROM articles WHERE id = 1`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "from-src" {
		t.Errorf("article 1 title = %q, want from-src (overwritten)", title)
	}
	var kept string
	if err := dstDB.QueryRow(`SELECT title FROM articles WHERE id = 99`).Scan(&kept); err != nil {
		t.Fatalf("article 99 missing: %v", err)
	}
	if kept != "keep-me" {
		t.Errorf("article 99 title = %q, want keep-me", kept)
	}

	// Associations and the self-referencing parent link made it.
	var parent sql.NullInt64
	if err := dstDB.QueryRow(`SELECT parent_id FROM comments WHERE id = 2`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if !parent.Valid || parent.Int64 != 1 {
		t.Errorf("comment 2 parent_id = %v, want 1", parent)
	}
	var atCount int
	if err := dstDB.QueryRow(`SELECT COUNT(*) FROM article_tags WHERE article_id = 1 AND tag_id = 1`).Scan(&atCount); err != nil {
		t.Fatal(err)
	}
	if atCount != 1 {
		t.Errorf("article_tags rows = %d, want 1", atCount)
	}

	// The singleton settings row was overwritten by the import.
	var siteTitle string
	if err := dstDB.QueryRow(`SELECT title FROM settings WHERE id = 1`).Scan(&siteTitle); err != nil {
		t.Fatal(err)
	}
	if siteTitle != "Src Title" {
		t.Errorf("settings title = %q, want Src Title", siteTitle)
	}

	// Media blob restored on disk.
	blob, err := os.ReadFile(filepath.Join(dstDir, "files", "aa", "aa", "aaaa1111bbbb"))
	if err != nil {
		t.Fatalf("restored blob: %v", err)
	}
	if string(blob) != "png-content" {
		t.Errorf("blob content = %q, want png-content", blob)
	}

	// Runtime tables were not imported.
	for _, table := range []string{"activity_logs", "job_runs", "sessions", "kv"} {
		var n int
		if err := dstDB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s rows = %d, want 0 (runtime table must not be imported)", table, n)
		}
	}
}

func TestDBImporterBareDatabase(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	seedSource(t, srcDB, srcDir)

	// A bare rables.db upload: VACUUM INTO produces a consistent file copy.
	copyPath := filepath.Join(t.TempDir(), "rables.db")
	if err := vacuumInto(ctx, srcDB, copyPath); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	dstDB, dstDir := newTestDB(t)
	res, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, copyPath)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.BlobsCopied != 0 {
		t.Errorf("blobs copied = %d, want 0 (bare database carries no media)", res.BlobsCopied)
	}
	var n int
	if err := dstDB.QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("articles = %d, want 2", n)
	}
}

func TestDBImporterRejectsRailsDatabase(t *testing.T) {
	ctx := context.Background()
	railsDB, _ := newTestDB(t)
	// Make the otherwise-Go database smell like a Rails one.
	if _, err := railsDB.Exec(`CREATE TABLE action_text_rich_texts (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "production.sqlite3")
	if err := vacuumInto(ctx, railsDB, copyPath); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	dstDB, dstDir := newTestDB(t)
	_, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, copyPath)
	if err == nil || !strings.Contains(err.Error(), "Rails") {
		t.Fatalf("err = %v, want a Rails-database rejection", err)
	}
}

func TestDBImporterToleratesLitterInBundle(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	seedSource(t, srcDB, srcDir)
	zipPath := exportZip(t, srcDB, srcDir)

	// Re-pack the export with litter added, simulating a hand-made bundle.
	littered := filepath.Join(t.TempDir(), "littered.zip")
	if err := addZipEntries(t, zipPath, littered, map[string]string{
		"files/.DS_Store":          "junk",
		"files/aa/.DS_Store":       "junk",
		"files/notes/readme.txt":   "not a blob path",
		"files/aa/aa/not!a!blob":   "bad key chars",
		"files/aa/aa/aaaa1111bbbb": "png-content", // same blob, still fine
	}); err != nil {
		t.Fatalf("repack: %v", err)
	}

	dstDB, dstDir := newTestDB(t)
	res, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, littered)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.BlobsCopied != 1 {
		t.Errorf("blobs copied = %d, want 1 (litter skipped)", res.BlobsCopied)
	}
	for _, gone := range []string{".DS_Store", "aa/.DS_Store", "notes/readme.txt", "aa/aa/not!a!blob"} {
		if _, err := os.Stat(filepath.Join(dstDir, "files", filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("litter %s must not be restored, stat err = %v", gone, err)
		}
	}
	if blob, err := os.ReadFile(filepath.Join(dstDir, "files", "aa", "aa", "aaaa1111bbbb")); err != nil || string(blob) != "png-content" {
		t.Errorf("real blob not restored: %q, %v", blob, err)
	}
}

// addZipEntries copies the entries of srcZip plus the extra name/content
// pairs into dstZip.
func addZipEntries(t *testing.T, srcZip, dstZip string, extra map[string]string) error {
	t.Helper()
	zr, err := zip.OpenReader(srcZip)
	if err != nil {
		return err
	}
	defer zr.Close()
	out, err := os.Create(dstZip)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := zw.Create(f.Name)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			return err
		}
		rc.Close()
	}
	for name, content := range extra {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Close()
}

func TestDBImporterRejectsBadZip(t *testing.T) {
	ctx := context.Background()
	dstDB, dstDir := newTestDB(t)

	// A zip without any database inside.
	zipPath := filepath.Join(t.TempDir(), "notes.zip")
	out, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	w, err := zw.Create("readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, zipPath); err == nil {
		t.Fatal("expected an error for a database-less zip")
	}
}
