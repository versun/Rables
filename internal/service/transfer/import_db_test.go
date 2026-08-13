package transfer

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rables/internal/jobs"
	"rables/internal/settings"
	"rables/internal/testutil/zipfake"
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

// TestDBImporterNormalizesActiveTwitterArchiveImport: a backup taken while an
// archive import was queued/running carries that row with active_slot=1.
// Imported as-is it would block every new archive upload
// (HasActiveTwitterArchiveImport) until the next restart's recovery; the
// import must fail the row and release the slot while keeping the history
// rows' display value.
func TestDBImporterNormalizesActiveTwitterArchiveImport(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	stmts := []string{
		`INSERT INTO twitter_archive_imports (id, status, progress, source_filename, source_path, status_message, queued_at, active_slot, created_at, updated_at)
		 VALUES (1, 'running', 40, 'twitter.zip', '/old/server/imports/twitter.zip', 'Importing likes', 1700000000, 1, 1700000000, 1700000001)`,
		`INSERT INTO twitter_archive_imports (id, status, progress, source_filename, status_message, queued_at, created_at, updated_at)
		 VALUES (2, 'completed', 100, 'older.zip', 'Import completed', 1690000000, 1690000000, 1690000100)`,
	}
	for _, s := range stmts {
		if _, err := srcDB.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	zipPath := exportZip(t, srcDB, srcDir)

	dstDB, dstDir := newTestDB(t)
	if _, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, zipPath); err != nil {
		t.Fatalf("import: %v", err)
	}

	// No active import left, so a new archive upload is accepted (the
	// active_slot unique index admits the re-claim too).
	var active int
	if err := dstDB.QueryRow(`SELECT COUNT(*) FROM twitter_archive_imports WHERE status IN ('queued', 'running')`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Errorf("active imports = %d, want 0", active)
	}
	if _, err := dstDB.Exec(`INSERT INTO twitter_archive_imports (status, source_filename, queued_at, active_slot, created_at, updated_at)
		VALUES ('queued', 'new.zip', 1700001000, 1, 1700001000, 1700001000)`); err != nil {
		t.Fatalf("new archive import blocked: %v", err)
	}

	// The imported active row was failed and its slot released (the upsert
	// neutralizes active_slot on the SELECT side), but it keeps the history
	// display value (filename, progress) and its source_path: on a
	// same-server restore the file is still on disk and a still-queued job
	// self-heals from it.
	var status, filename, statusMessage string
	var progress int
	var activeSlot, finishedAt sql.NullInt64
	var sourcePath, errorMessage sql.NullString
	if err := dstDB.QueryRow(`SELECT status, progress, source_filename, status_message, active_slot, source_path, error_message, finished_at
		FROM twitter_archive_imports WHERE id = 1`).Scan(&status, &progress, &filename, &statusMessage, &activeSlot, &sourcePath, &errorMessage, &finishedAt); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("import 1 status = %q, want failed", status)
	}
	if activeSlot.Valid {
		t.Errorf("import 1 active_slot = %v, want NULL (slot released)", activeSlot.Int64)
	}
	if !sourcePath.Valid || sourcePath.String != "/old/server/imports/twitter.zip" {
		t.Errorf("import 1 source_path = %v, want kept (a same-server restore self-heals from it)", sourcePath)
	}
	if !errorMessage.Valid || errorMessage.String == "" {
		t.Errorf("import 1 error_message = %v, want an explanation", errorMessage)
	}
	if !finishedAt.Valid {
		t.Error("import 1 finished_at = NULL, want set")
	}
	if filename != "twitter.zip" || progress != 40 || statusMessage != "Import failed" {
		t.Errorf("import 1 keeps history: filename=%q progress=%d status_message=%q", filename, progress, statusMessage)
	}

	// The completed history row is untouched.
	var status2, filename2 string
	if err := dstDB.QueryRow(`SELECT status, source_filename FROM twitter_archive_imports WHERE id = 2`).Scan(&status2, &filename2); err != nil {
		t.Fatal(err)
	}
	if status2 != "completed" || filename2 != "older.zip" {
		t.Errorf("import 2 = %q %q, want completed older.zip", status2, filename2)
	}
}

// TestDBImporterKeepsLiveQueuedTwitterArchiveImport: an archive import queued
// in the live database is not part of the bundle, so the normalization must
// leave it alone — its row and its still-queued job belong to this server,
// not to the backup.
func TestDBImporterKeepsLiveQueuedTwitterArchiveImport(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	// The bundle carries only an inactive history row.
	if _, err := srcDB.Exec(`INSERT INTO twitter_archive_imports (id, status, progress, source_filename, status_message, queued_at, created_at, updated_at)
		VALUES (2, 'completed', 100, 'older.zip', 'Import completed', 1690000000, 1690000000, 1690000100)`); err != nil {
		t.Fatal(err)
	}
	zipPath := exportZip(t, srcDB, srcDir)

	dstDB, dstDir := newTestDB(t)
	if _, err := dstDB.Exec(`INSERT INTO twitter_archive_imports (id, status, source_filename, source_path, queued_at, active_slot, created_at, updated_at)
		VALUES (9, 'queued', 'live.zip', '/data/imports/live.zip', 1700000002, 1, 1700000002, 1700000002)`); err != nil {
		t.Fatal(err)
	}
	if _, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, zipPath); err != nil {
		t.Fatalf("import: %v", err)
	}

	var status, sourcePath string
	var activeSlot int
	if err := dstDB.QueryRow(`SELECT status, source_path, active_slot FROM twitter_archive_imports WHERE id = 9`).Scan(&status, &sourcePath, &activeSlot); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || sourcePath != "/data/imports/live.zip" || activeSlot != 1 {
		t.Errorf("live import 9 = %q %q slot %d, want queued with its path and slot", status, sourcePath, activeSlot)
	}
	var imported int
	if err := dstDB.QueryRow(`SELECT COUNT(*) FROM twitter_archive_imports WHERE id = 2`).Scan(&imported); err != nil {
		t.Fatal(err)
	}
	if imported != 1 {
		t.Errorf("bundle history row imported = %d, want 1", imported)
	}
}

// TestDBImporterNeutralizesImportedActiveSlot: the live database has its own
// queued archive import holding active_slot=1 and the bundle carries a
// different active row with the same slot value. The upsert must neutralize
// the imported slot on the SELECT side — otherwise idx_tai_active_slot
// aborts the upsert before the normalization can run and rolls the whole
// import back. The live row is not part of the bundle and stays queued.
func TestDBImporterNeutralizesImportedActiveSlot(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	if _, err := srcDB.Exec(`INSERT INTO twitter_archive_imports (id, status, progress, source_filename, source_path, queued_at, active_slot, created_at, updated_at)
		VALUES (1, 'running', 40, 'twitter.zip', '/old/server/imports/twitter.zip', 1700000000, 1, 1700000000, 1700000001)`); err != nil {
		t.Fatal(err)
	}
	zipPath := exportZip(t, srcDB, srcDir)

	dstDB, dstDir := newTestDB(t)
	if _, err := dstDB.Exec(`INSERT INTO twitter_archive_imports (id, status, source_filename, source_path, queued_at, active_slot, created_at, updated_at)
		VALUES (9, 'queued', 'live.zip', '/data/imports/twitter_archive_live.zip', 1700000002, 1, 1700000002, 1700000002)`); err != nil {
		t.Fatal(err)
	}
	if _, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, zipPath); err != nil {
		t.Fatalf("import: %v", err)
	}

	// The imported row was failed with its slot released.
	var status string
	var activeSlot sql.NullInt64
	if err := dstDB.QueryRow(`SELECT status, active_slot FROM twitter_archive_imports WHERE id = 1`).Scan(&status, &activeSlot); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || activeSlot.Valid {
		t.Errorf("imported import 1 = %q slot %v, want failed with a NULL slot", status, activeSlot)
	}

	// The live active row kept its status, path and slot.
	var liveStatus, livePath string
	var liveSlot int
	if err := dstDB.QueryRow(`SELECT status, source_path, active_slot FROM twitter_archive_imports WHERE id = 9`).Scan(&liveStatus, &livePath, &liveSlot); err != nil {
		t.Fatal(err)
	}
	if liveStatus != "queued" || livePath != "/data/imports/twitter_archive_live.zip" || liveSlot != 1 {
		t.Errorf("live import 9 = %q %q slot %d, want queued with its path and slot", liveStatus, livePath, liveSlot)
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

// TestDBImporterRejectsRailsBundleWithoutOrphanBlobs: a bundle rejected by
// the schema check must not leave its blobs behind on disk (blob restore
// runs only after the row copy has committed).
func TestDBImporterRejectsRailsBundleWithoutOrphanBlobs(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	seedSource(t, srcDB, srcDir) // includes the files/aa/aa/aaaa1111bbbb blob
	// Make the otherwise-Go database smell like a Rails one.
	if _, err := srcDB.Exec(`CREATE TABLE action_text_rich_texts (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	zipPath := exportZip(t, srcDB, srcDir)

	dstDB, dstDir := newTestDB(t)
	_, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, zipPath)
	if err == nil || !strings.Contains(err.Error(), "Rails") {
		t.Fatalf("err = %v, want a Rails-database rejection", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "files", "aa", "aa", "aaaa1111bbbb")); !os.IsNotExist(err) {
		t.Errorf("rejected bundle left a blob on disk, stat err = %v", err)
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
		"files/bb/bb/aaaa1111bbbb": "wrong layout (dirs don't match the key)",
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
	for _, gone := range []string{".DS_Store", "aa/.DS_Store", "notes/readme.txt", "aa/aa/not!a!blob", "bb/bb/aaaa1111bbbb"} {
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

// TestExtractImportZipSizeLimit aborts extraction once the declared
// uncompressed sizes add up past MaxImportExtractBytes (zip-bomb guard);
// honest small archives extract fine.
func TestExtractImportZipSizeLimit(t *testing.T) {
	bomb := filepath.Join(t.TempDir(), "bomb.zip")
	zipfake.Write(t, bomb, []zipfake.Entry{
		{Name: "e0.bin", Declared: 3 << 30},
		{Name: "e1.bin", Declared: 3 << 30},
		{Name: "e2.bin", Declared: 3 << 30},
		{Name: "e3.bin", Declared: 3 << 30},
	})
	err := extractImportZip(bomb, t.TempDir())
	if err == nil {
		t.Fatal("expected the extraction limit to trip")
	}
	limit := fmt.Sprint(MaxImportExtractBytes)
	if !strings.Contains(err.Error(), limit) || !strings.Contains(err.Error(), "e3.bin") {
		t.Errorf("error = %v, want the offending entry and the %s limit", err, limit)
	}

	honest := filepath.Join(t.TempDir(), "honest.zip")
	zipfake.Write(t, honest, []zipfake.Entry{
		{Name: "e0.bin", Payload: bytes.Repeat([]byte("a"), 100), Declared: 100},
		{Name: "e1.bin", Payload: []byte("12345"), Declared: 5},
	})
	stage := t.TempDir()
	if err := extractImportZip(honest, stage); err != nil {
		t.Fatalf("honest zip: %v", err)
	}
	for _, name := range []string{"e0.bin", "e1.bin"} {
		if _, err := os.Stat(filepath.Join(stage, name)); err != nil {
			t.Errorf("entry %s not extracted: %v", name, err)
		}
	}
}

// TestExtractImportZipDeclaredOverflow forges zip64 declared sizes whose sum
// wraps uint64 (10GB then 2^64-1); the bomb guard must compare before adding
// so the total cannot wrap back under the limit.
func TestExtractImportZipDeclaredOverflow(t *testing.T) {
	bomb := filepath.Join(t.TempDir(), "bomb64.zip")
	zipfake.Write(t, bomb, []zipfake.Entry{
		{Name: "e0.bin", Payload: []byte("x"), Declared: MaxImportExtractBytes, Zip64: true},
		{Name: "e1.bin", Payload: []byte("x"), Declared: math.MaxUint64, Zip64: true},
	})
	if err := extractImportZip(bomb, t.TempDir()); err == nil {
		t.Fatal("expected the forged zip64 declared sizes to be rejected")
	}
}

// TestExtractImportZipRejectsTraversal: an entry whose name would escape the
// staging directory aborts the extraction (refused, not sanitized), and
// nothing is written outside it.
func TestExtractImportZipRejectsTraversal(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "evil.zip")
	out, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	w, err := zw.Create("../evil.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	stage := filepath.Join(parent, "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	err = extractImportZip(zipPath, stage)
	if err == nil || !strings.Contains(err.Error(), "../evil.txt") {
		t.Fatalf("extractImportZip err = %v, want a rejection naming the traversal entry", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("traversal entry escaped the staging dir (stat err = %v)", err)
	}
}

// TestSafeZipEntryNameRejectsBackslash: a backslash is a path separator on
// Windows, where the "/"-based ".." check would miss "..\..\evil.txt" and let
// the entry escape the staging directory — any name containing one is
// refused. Ordinary forward-slash names still pass.
func TestSafeZipEntryNameRejectsBackslash(t *testing.T) {
	for _, name := range []string{`..\..\evil.txt`, `dir\file.txt`} {
		if _, err := safeZipEntryName(name); err == nil {
			t.Errorf("safeZipEntryName(%q) = nil error, want a rejection", name)
		}
	}
	if _, err := safeZipEntryName("dir/file.txt"); err != nil {
		t.Errorf("safeZipEntryName(%q) = %v, want ok", "dir/file.txt", err)
	}
}

// TestDBImporterQueuedServerZip: the admin server-file import renames
// export.zip to export.zip.queued before enqueueing; the importer must sniff
// the magic bytes instead of the name, or the queued ZIP is ATTACHed as a
// bare database and fails with 'file is not a database'.
func TestDBImporterQueuedServerZip(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	seedSource(t, srcDB, srcDir)
	zipPath := exportZip(t, srcDB, srcDir)

	queued := filepath.Join(t.TempDir(), "export.zip.queued")
	if err := copyFile(zipPath, queued); err != nil {
		t.Fatal(err)
	}

	dstDB, dstDir := newTestDB(t)
	res, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, queued)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Rows["articles"] != 2 {
		t.Errorf("articles rows = %d, want 2", res.Rows["articles"])
	}
	if res.BlobsCopied != 1 {
		t.Errorf("blobs copied = %d, want 1", res.BlobsCopied)
	}
}

// TestDBImporterRejectsViewOverRuntimeTables: a hostile bundle replaces the
// redirects table with a VIEW reading main.sessions (live tokens) once
// attached; the schema check must refuse it before any row is copied.
func TestDBImporterRejectsViewOverRuntimeTables(t *testing.T) {
	ctx := context.Background()
	srcDB, _ := newTestDB(t)
	// The required tables stay real tables; redirects becomes a VIEW pumping
	// session tokens into the publicly rendered replacement column.
	if _, err := srcDB.Exec(`DROP TABLE redirects`); err != nil {
		t.Fatal(err)
	}
	if _, err := srcDB.Exec(`CREATE VIEW redirects AS SELECT id, token AS replacement FROM main.sessions`); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "evil.db")
	if err := vacuumInto(ctx, srcDB, copyPath); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	dstDB, dstDir := newTestDB(t)
	if _, err := dstDB.Exec(`INSERT INTO users (id, user_name, password_digest, created_at, updated_at) VALUES (1, 'victim', 'digest', 1700000000, 1700000000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := dstDB.Exec(`INSERT INTO sessions (id, token, user_id, created_at, updated_at) VALUES (1, 'live-token', 1, 1700000000, 1700000000)`); err != nil {
		t.Fatal(err)
	}
	_, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, copyPath)
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("err = %v, want a rejection naming redirects", err)
	}
	var n int
	if err := dstDB.QueryRow(`SELECT COUNT(*) FROM redirects`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("redirects rows = %d, want 0 (nothing imported from the hostile source)", n)
	}
}

// TestDBImporterRejectsCraftedColumnName: a hostile bundle gives a real
// table a column whose name embeds a quote and a second statement; the
// schema check must refuse the import before any row is copied, and the
// injected statement must never run.
func TestDBImporterRejectsCraftedColumnName(t *testing.T) {
	ctx := context.Background()
	srcDB, _ := newTestDB(t)
	if _, err := srcDB.Exec(`ALTER TABLE redirects ADD COLUMN "x""; DELETE FROM users; --" TEXT`); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "evil.db")
	if err := vacuumInto(ctx, srcDB, copyPath); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	dstDB, dstDir := newTestDB(t)
	if _, err := dstDB.Exec(`INSERT INTO users (id, user_name, password_digest, created_at, updated_at) VALUES (1, 'victim', 'digest', 1700000000, 1700000000)`); err != nil {
		t.Fatal(err)
	}
	_, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, copyPath)
	if err == nil || !strings.Contains(err.Error(), "invalid column name") || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("err = %v, want a rejection naming the crafted redirects column", err)
	}
	var n int
	if err := dstDB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("users rows = %d, want 1 (the injected statement must not run)", n)
	}
}

// TestUpsertTableQuotesWeirdColumnNames: a column name embedding a quote
// (present in both schemas, so the intersection keeps it) must be quoted
// with quoteIdent like the table name — a naive "col" wrapping breaks the
// statement instead of copying the row.
func TestUpsertTableQuotesWeirdColumnNames(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	conn, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stmts := []string{
		`ATTACH DATABASE ':memory:' AS src`,
		`CREATE TABLE src.redirects (id INTEGER PRIMARY KEY, regex TEXT NOT NULL, replacement TEXT NOT NULL,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, "weird""col" TEXT)`,
		`ALTER TABLE main.redirects ADD COLUMN "weird""col" TEXT`,
		`INSERT INTO src.redirects (id, regex, replacement, created_at, updated_at, "weird""col")
			VALUES (1, '^/old$', '/new', 1700000000, 1700000000, 'copied')`,
	}
	for _, s := range stmts {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := upsertTable(ctx, tx, "redirects")
	if err != nil {
		tx.Rollback()
		t.Fatalf("upsert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	var v sql.NullString
	if err := database.QueryRow(`SELECT "weird""col" FROM redirects WHERE id = 1`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if !v.Valid || v.String != "copied" {
		t.Errorf("weird column = %v, want copied", v)
	}
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

// runImportDBJob enqueues one import_db job and executes it synchronously.
func runImportDBJob(t *testing.T, database *sql.DB, dataDir string, payload ImportDBPayload) {
	t.Helper()
	ctx := context.Background()
	w := jobs.NewWorker(database)
	RegisterImportDBHandler(w, database, dataDir, nil)
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindImportDB, payload, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run job: %v", err)
	}
}

func TestImportDBJobFileCleanup(t *testing.T) {
	// A source bundle that imports cleanly, and a file that is not a
	// database at all.
	srcDB, srcDir := newTestDB(t)
	seedSource(t, srcDB, srcDir)
	goodZip := exportZip(t, srcDB, srcDir)

	setup := func(t *testing.T, name, src string) (database *sql.DB, dataDir, path string) {
		t.Helper()
		database, dataDir = newTestDB(t)
		importsDir := filepath.Join(dataDir, "imports")
		if err := os.MkdirAll(importsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(importsDir, name)
		if src == "" {
			if err := os.WriteFile(path, []byte("not a sqlite database"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := copyFile(src, path); err != nil {
			t.Fatal(err)
		}
		return database, dataDir, path
	}

	exists := func(t *testing.T, path string) bool {
		t.Helper()
		_, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
		return err == nil
	}

	t.Run("failed import keeps a server file for retry", func(t *testing.T) {
		database, dataDir, path := setup(t, "from-server.db", "")
		runImportDBJob(t, database, dataDir, ImportDBPayload{Path: path, KeepOnFailure: true})
		if !exists(t, path) {
			t.Error("server file removed after a failed import, want it kept for retry")
		}
	})

	t.Run("failed import still removes an upload", func(t *testing.T) {
		database, dataDir, path := setup(t, "import_123_broken.db", "")
		runImportDBJob(t, database, dataDir, ImportDBPayload{Path: path})
		if exists(t, path) {
			t.Error("upload left behind after a failed import")
		}
	})

	t.Run("successful import removes a server file", func(t *testing.T) {
		database, dataDir, path := setup(t, "backup.zip", goodZip)
		runImportDBJob(t, database, dataDir, ImportDBPayload{Path: path, KeepOnFailure: true})
		if exists(t, path) {
			t.Error("server file left behind after a successful import")
		}
	})
}

// TestImportDBJobBlobRestoreFailureKeepsUpload: when the blob restore fails
// after the row copy committed, the source file is kept even for a web
// upload (KeepOnFailure=false) so a retry can heal the missing blobs. The
// kept upload is renamed off the import_* prefix — under it the startup
// orphan sweep would reap the file while the import tab and import_server
// hide/reject it — so the retry goes through the ordinary server-file
// channel and removes the file on success.
func TestImportDBJobBlobRestoreFailureKeepsUpload(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	seedSource(t, srcDB, srcDir)
	goodZip := exportZip(t, srcDB, srcDir)

	dstDB, dstDir := newTestDB(t)
	importsDir := filepath.Join(dstDir, "imports")
	if err := os.MkdirAll(importsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(importsDir, "import_123_bundle.zip")
	if err := copyFile(goodZip, path); err != nil {
		t.Fatal(err)
	}

	// Break the blob restore: <DataDir>/files as a regular file makes the
	// MkdirAll of every blob destination fail.
	filesPath := filepath.Join(dstDir, "files")
	if err := os.WriteFile(filesPath, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The direct Import surfaces the post-commit failure as BlobRestoreError.
	_, err := (&DBImporter{DB: dstDB, DataDir: dstDir}).Import(ctx, path)
	var blobErr *BlobRestoreError
	if !errors.As(err, &blobErr) {
		t.Fatalf("Import err = %v, want a *BlobRestoreError", err)
	}
	// The rows committed before the blob restore ran.
	var n int
	if err := dstDB.QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("articles = %d, want 2 (row copy committed before the blob failure)", n)
	}

	// Through the job the upload is kept for retry despite KeepOnFailure=false,
	// renamed to an ordinary server-side file name.
	runImportDBJob(t, dstDB, dstDir, ImportDBPayload{Path: path})
	kept := filepath.Join(importsDir, "123_bundle.zip")
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("kept upload missing under its unprefixed name: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("import_* name left behind after the keep-rename, stat err = %v", err)
	}

	// Heal the disk and retry the renamed file (as import_server would): the
	// import succeeds (row upserts are idempotent, the blob is copied now)
	// and the job removes the file.
	if err := os.Remove(filesPath); err != nil {
		t.Fatal(err)
	}
	runImportDBJob(t, dstDB, dstDir, ImportDBPayload{Path: kept})
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Errorf("upload left behind after the successful retry, stat err = %v", err)
	}
	if blob, err := os.ReadFile(filepath.Join(dstDir, "files", "aa", "aa", "aaaa1111bbbb")); err != nil || string(blob) != "png-content" {
		t.Errorf("blob not restored by the retry: %q, %v", blob, err)
	}
}

// TestImportDBJobInvalidatesSettingsCache pins the settings.Cache contract
// (writers bypassing Update must Invalidate): the row copy upserts the
// settings table, so the job handler must run the invalidate hook the
// integrator wires to s.Settings().Invalidate() — otherwise public pages
// serve the old site title/URL/CSS until the 5-minute TTL expires. An import
// that committed nothing must not invalidate.
func TestImportDBJobInvalidatesSettingsCache(t *testing.T) {
	ctx := context.Background()
	srcDB, srcDir := newTestDB(t)
	seedSource(t, srcDB, srcDir) // settings row: title "Src Title"
	zipPath := exportZip(t, srcDB, srcDir)

	runJob := func(t *testing.T, database *sql.DB, dataDir string, payload ImportDBPayload, invalidate func()) {
		t.Helper()
		w := jobs.NewWorker(database)
		RegisterImportDBHandler(w, database, dataDir, invalidate)
		if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindImportDB, payload, time.Now()); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("run job: %v", err)
		}
	}

	t.Run("committed import invalidates the cached settings row", func(t *testing.T) {
		dstDB, dstDir := newTestDB(t)
		if _, err := dstDB.Exec(`INSERT INTO settings (id, title, created_at, updated_at) VALUES (1, 'Dst Title', 1700000001, 1700000001)`); err != nil {
			t.Fatal(err)
		}
		cache := settings.NewCache(dstDB, nil)
		if row, err := cache.Get(ctx); err != nil || row.Title.String != "Dst Title" {
			t.Fatalf("cached title = %q, %v, want Dst Title", row.Title.String, err)
		}
		runJob(t, dstDB, dstDir, ImportDBPayload{Path: zipPath}, cache.Invalidate)
		row, err := cache.Get(ctx)
		if err != nil {
			t.Fatalf("Get after import: %v", err)
		}
		if row.Title.String != "Src Title" {
			t.Errorf("title after import = %q, want Src Title (the cached row was not invalidated)", row.Title.String)
		}
	})

	t.Run("failed import does not invalidate", func(t *testing.T) {
		dstDB, dstDir := newTestDB(t)
		bad := filepath.Join(t.TempDir(), "broken.db")
		if err := os.WriteFile(bad, []byte("not a sqlite database"), 0o644); err != nil {
			t.Fatal(err)
		}
		fired := false
		runJob(t, dstDB, dstDir, ImportDBPayload{Path: bad}, func() { fired = true })
		if fired {
			t.Error("invalidate ran for an import that committed nothing")
		}
	})
}

// TestExtractImportZipEntryLimit: a zip with more file entries than
// MaxImportExtractEntries is refused up front, even though every entry is a
// one-byte file far under the byte limit (inode-exhaustion guard).
func TestExtractImportZipEntryLimit(t *testing.T) {
	entries := make([]zipfake.Entry, 0, MaxImportExtractEntries+1)
	for i := 0; i <= MaxImportExtractEntries; i++ {
		entries = append(entries, zipfake.Entry{
			Name:     fmt.Sprintf("e%06d.bin", i),
			Payload:  []byte("x"),
			Declared: 1,
		})
	}
	bomb := filepath.Join(t.TempDir(), "many.zip")
	zipfake.Write(t, bomb, entries)
	err := extractImportZip(bomb, t.TempDir())
	if err == nil {
		t.Fatal("expected the entry-count limit to trip")
	}
	limit := fmt.Sprint(MaxImportExtractEntries)
	if !strings.Contains(err.Error(), limit) {
		t.Errorf("error = %v, want the %s entry limit", err, limit)
	}
}

// errReader yields some bytes then fails, standing in for a copy that dies
// midway (the classic case is a full disk surfacing at write time).
type errReader struct{}

func (errReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'x'
	return 1, errors.New("injected copy failure")
}

func TestWriteFileFromRemovesPartialOnError(t *testing.T) {
	target := filepath.Join(t.TempDir(), "blob")
	if err := writeFileFrom(target, errReader{}); err == nil {
		t.Fatal("expected the injected copy failure")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file must be removed after a failed copy, stat err = %v", err)
	}

	// The retry that follows a healed failure (blob gone from disk) copies
	// the file fresh instead of keeping the truncated fragment.
	if err := writeFileFrom(target, strings.NewReader("full-content")); err != nil {
		t.Fatalf("retry after cleanup: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "full-content" {
		t.Errorf("retried copy content = %q, want %q", got, "full-content")
	}
}

// TestWriteFileAtomicLeavesNoPartialOnError: the blob restores treat an
// existing destination as "kept" and skip it, so a copy that dies midway
// (crash, disk full) must never leave anything at the final path — the
// fragment lands in a .part-* sibling and only a complete file is renamed
// into place.
func TestWriteFileAtomicLeavesNoPartialOnError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "blob")
	if err := writeFileAtomic(target, errReader{}); err == nil {
		t.Fatal("expected the injected copy failure")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final path must not exist after a failed copy, stat err = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("no .part-* sibling may be left behind, found %s", entries[0].Name())
	}

	// The retry that follows a healed failure copies the blob fresh, and the
	// rename lands the full content at the final path in one step.
	if err := writeFileAtomic(target, strings.NewReader("full-content")); err != nil {
		t.Fatalf("retry after cleanup: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "full-content" {
		t.Errorf("retried copy content = %q, want %q", got, "full-content")
	}
}
