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

	_ "modernc.org/sqlite"

	"rables/internal/db"
)

// newTestDB opens a migrated database in a fresh temp data dir.
func newTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database, dataDir
}

// tableCount returns the row count of table.
func tableCount(t *testing.T, database *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestBundleExporterGenerate(t *testing.T) {
	database, dataDir := newTestDB(t)
	ctx := context.Background()

	if _, err := database.Exec(`INSERT INTO articles (title, slug, content_html, status, created_at, updated_at)
		VALUES ('hello', 'hello', '<p>hi</p>', 1, 1700000000, 1700000000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO files (key, filename, content_type, byte_size, created_at)
		VALUES ('aaaa1111bbbb', 'pic.png', 'image/png', 11, 1700000000)`); err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(dataDir, "files", "aa", "aa", "aaaa1111bbbb")
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, []byte("png-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Litter in the media tree must not end up in the bundle.
	if err := os.WriteFile(filepath.Join(dataDir, "files", ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "files", "aa", ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	zipPath, err := (&BundleExporter{DB: database, DataDir: dataDir}).Generate(ctx)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	base := filepath.Base(zipPath)
	if filepath.Dir(zipPath) != filepath.Join(dataDir, "exports") ||
		!strings.HasPrefix(base, "export_") || !strings.HasSuffix(base, ".zip") {
		t.Errorf("zip path = %q, want data/exports/export_*.zip", zipPath)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		entries[f.Name] = f
	}
	if entries["rables.db"] == nil {
		t.Fatalf("zip missing rables.db, entries: %v", entries)
	}
	blob := entries["files/aa/aa/aaaa1111bbbb"]
	if blob == nil {
		t.Fatalf("zip missing the media blob, entries: %v", entries)
	}
	for name := range entries {
		if strings.Contains(name, ".DS_Store") {
			t.Errorf("zip contains litter %q; non-blob files must be skipped", name)
		}
	}
	rc, err := blob.Open()
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "png-content" {
		t.Errorf("blob content = %q, want png-content", content)
	}

	// The exported database copy opens standalone and carries the data.
	rc, err = entries["rables.db"].Open()
	if err != nil {
		t.Fatal(err)
	}
	dbBytes, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(extracted, dbBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	copy, err := sql.Open("sqlite", "file:"+extracted+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var title string
	if err := copy.QueryRow("SELECT title FROM articles WHERE slug = 'hello'").Scan(&title); err != nil {
		t.Fatalf("query exported db: %v", err)
	}
	if title != "hello" {
		t.Errorf("title = %q, want hello", title)
	}
}

func TestBundleExporterWithoutMedia(t *testing.T) {
	database, dataDir := newTestDB(t)
	zipPath, err := (&BundleExporter{DB: database, DataDir: dataDir}).Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	if len(zr.File) != 1 || zr.File[0].Name != "rables.db" {
		names := make([]string, 0, len(zr.File))
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		t.Errorf("entries = %v, want only rables.db", names)
	}
}

// A files/ root symlinked elsewhere (e.g. media moved to another disk) must
// be followed: filepath.WalkDir does not descend into a symlinked root, so
// without resolution the export would silently contain no blobs.
func TestBundleExporterSymlinkedMediaRoot(t *testing.T) {
	database, dataDir := newTestDB(t)

	realRoot := t.TempDir()
	blobPath := filepath.Join(realRoot, "aa", "aa", "aaaa1111bbbb")
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, []byte("png-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, filepath.Join(dataDir, "files")); err != nil {
		t.Fatal(err)
	}

	zipPath, err := (&BundleExporter{DB: database, DataDir: dataDir}).Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	var found bool
	for _, f := range zr.File {
		if f.Name == "files/aa/aa/aaaa1111bbbb" {
			found = true
		}
	}
	if !found {
		t.Error("zip missing the media blob behind the symlinked files/ root")
	}
}

// A dangling symlink as files/ root (e.g. the media disk is not mounted)
// must fail the export: the blob-less bundle would otherwise look like a
// valid backup of an install with no media.
func TestBundleExporterDanglingMediaRoot(t *testing.T) {
	database, dataDir := newTestDB(t)

	if err := os.Symlink(filepath.Join(dataDir, "unmounted"), filepath.Join(dataDir, "files")); err != nil {
		t.Fatal(err)
	}
	if _, err := (&BundleExporter{DB: database, DataDir: dataDir}).Generate(context.Background()); err == nil {
		t.Fatal("generate succeeded with a dangling files/ symlink")
	}
}

// A files/ root that exists but is not a directory is a misconfiguration,
// not "no media": the export must fail instead of shipping a blob-less zip.
func TestBundleExporterFilesRootNotDir(t *testing.T) {
	database, dataDir := newTestDB(t)

	if err := os.WriteFile(filepath.Join(dataDir, "files"), []byte("oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&BundleExporter{DB: database, DataDir: dataDir}).Generate(context.Background()); err == nil {
		t.Fatal("generate succeeded with files/ as a regular file")
	}
}

// A failed export must leave exports/ empty: the bundle is assembled inside
// the staging dir and only renamed to export_*.zip once complete, so neither
// a partial zip nor the staging dir reaches the admin download list.
func TestBundleExporterFailureLeavesNoZip(t *testing.T) {
	database, dataDir := newTestDB(t)

	if _, err := database.Exec(`INSERT INTO files (key, filename, content_type, byte_size, created_at)
		VALUES ('aaaa1111bbbb', 'pic.png', 'image/png', 11, 1700000000)`); err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(dataDir, "files", "aa", "aa", "aaaa1111bbbb")
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, []byte("png-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unreadable blob fails the export mid-pack, after the zip is opened.
	if err := os.Chmod(blobPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blobPath, 0o644) }) // let t.TempDir cleanup succeed

	if _, err := (&BundleExporter{DB: database, DataDir: dataDir}).Generate(context.Background()); err == nil {
		t.Fatal("generate succeeded with an unreadable blob")
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "exports"))
	if err != nil {
		t.Fatalf("read exports: %v", err)
	}
	for _, e := range entries {
		t.Errorf("leftover %q in exports; a failed export must leave it empty", e.Name())
	}
}
