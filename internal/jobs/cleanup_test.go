package jobs

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rables/internal/db/query"
)

func writeImportFile(t *testing.T, dataDir, name string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(dataDir, "imports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir imports: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("upload"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// makeImportStagingDir creates a staging dir like transfer.importStagingDir
// (with one extracted file inside) and back-dates its mtime.
func makeImportStagingDir(t *testing.T, dataDir, name string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(dataDir, "imports", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "media.bin"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write inside %s: %v", name, err)
	}
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return dir
}

// insertTwitterArchiveImport inserts a twitter_archive_imports row with the
// given status and source_path and returns its id.
func insertTwitterArchiveImport(t *testing.T, d *sql.DB, status, sourcePath string) int64 {
	t.Helper()
	var sp sql.NullString
	if sourcePath != "" {
		sp = sql.NullString{String: sourcePath, Valid: true}
	}
	res, err := d.Exec(
		`INSERT INTO twitter_archive_imports (status, source_filename, source_path, queued_at, created_at, updated_at)
		 VALUES (?, 'archive.zip', ?, 0, 0, 0)`,
		status, sp,
	)
	if err != nil {
		t.Fatalf("insert twitter archive import: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("import id: %v", err)
	}
	return id
}

func TestCleanupOrphanImportFiles(t *testing.T) {
	startedAt := time.Now()
	old := startedAt.Add(-time.Hour)

	t.Run("unreferenced uploads are removed", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		orphanDB := writeImportFile(t, dataDir, "import_100_deadbeef.zip", old)
		orphanTwitter := writeImportFile(t, dataDir, "twitter_archive_100_deadbeef.zip", old)
		orphanQueued := writeImportFile(t, dataDir, "import_101_deadbeef.zip.queued", old)
		// Files without the owned-upload prefixes are never candidates.
		serverFile := writeImportFile(t, dataDir, "backup.zip", old)
		serverQueued := writeImportFile(t, dataDir, "other.zip.queued", old)

		n, err := CleanupOrphanImportFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 3 {
			t.Errorf("removed = %d, want 3", n)
		}
		for _, path := range []string{orphanDB, orphanTwitter, orphanQueued} {
			if fileExists(path) {
				t.Errorf("%s still exists, want removed", filepath.Base(path))
			}
		}
		for _, path := range []string{serverFile, serverQueued} {
			if !fileExists(path) {
				t.Errorf("%s was removed, want kept", filepath.Base(path))
			}
		}
	})

	t.Run("uploads referenced by active jobs are kept", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		ctx := t.Context()
		enq := NewEnqueuer(d)

		dbUpload := writeImportFile(t, dataDir, "import_200_aaaaaaaa.zip", old)
		if _, err := enq.Enqueue(ctx, KindImportDB, map[string]any{"path": dbUpload}, time.Now()); err != nil {
			t.Fatalf("enqueue import_db: %v", err)
		}
		railsDB := writeImportFile(t, dataDir, "import_201_bbbbbbbb.db", old)
		railsStorage := writeImportFile(t, dataDir, "import_202_cccccccc.zip", old)
		if _, err := enq.Enqueue(ctx, KindImportRails, map[string]any{"db_path": railsDB, "storage_path": railsStorage}, time.Now()); err != nil {
			t.Fatalf("enqueue import_rails: %v", err)
		}

		n, err := CleanupOrphanImportFiles(ctx, query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		for _, path := range []string{dbUpload, railsDB, railsStorage} {
			if !fileExists(path) {
				t.Errorf("%s was removed, want kept", filepath.Base(path))
			}
		}
	})

	t.Run("upload referenced only by a finished job is removed", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		ctx := t.Context()

		upload := writeImportFile(t, dataDir, "import_300_dddddddd.zip", old)
		id, err := NewEnqueuer(d).Enqueue(ctx, KindImportDB, map[string]any{"path": upload}, time.Now())
		if err != nil {
			t.Fatalf("enqueue import_db: %v", err)
		}
		if err := query.New(d).FailJobRun(ctx, query.FailJobRunParams{
			Attempts: 1, LastError: sql.NullString{String: "boom", Valid: true}, UpdatedAt: 1, ID: id,
		}); err != nil {
			t.Fatalf("fail job run: %v", err)
		}

		n, err := CleanupOrphanImportFiles(ctx, query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 1 || fileExists(upload) {
			t.Errorf("removed = %d, exists = %v; want 1, false", n, fileExists(upload))
		}
	})

	t.Run("twitter archive sources of active imports are kept", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		ctx := t.Context()

		// Queued import row without a job (crash between the INSERT and the
		// enqueue): the row still owns the file until startup recovery fails it.
		queued := writeImportFile(t, dataDir, "twitter_archive_400_eeeeeeee.zip", old)
		insertTwitterArchiveImport(t, d, "queued", queued)

		// Failed row whose job is still queued: the job re-marks the row
		// running and re-reads the file (self-heal), so the file stays.
		selfHeal := writeImportFile(t, dataDir, "twitter_archive_401_ffffffff.zip", old)
		importID := insertTwitterArchiveImport(t, d, "failed", selfHeal)
		if _, err := NewEnqueuer(d).Enqueue(ctx, KindTwitterArchiveImport, map[string]any{"import_id": importID}, time.Now()); err != nil {
			t.Fatalf("enqueue twitter_archive_import: %v", err)
		}

		n, err := CleanupOrphanImportFiles(ctx, query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		for _, path := range []string{queued, selfHeal} {
			if !fileExists(path) {
				t.Errorf("%s was removed, want kept", filepath.Base(path))
			}
		}
	})

	t.Run("fresh upload is kept even when unreferenced", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		// An upload written after this process started (another process still
		// streaming it during a rolling deploy) is never swept.
		fresh := writeImportFile(t, dataDir, "import_500_99999999.zip", startedAt.Add(time.Minute))

		n, err := CleanupOrphanImportFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		if !fileExists(fresh) {
			t.Errorf("fresh upload was removed, want kept")
		}
	})

	t.Run("stale temp files are removed without a reference check", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		// A .part file is the temp name of an in-progress upload; a job never
		// references it (the handler renames to the final name before
		// enqueueing), so a stale one is a crash leftover and goes without a
		// lookup.
		staleImport := writeImportFile(t, dataDir, "import_600_aabbccdd.zip.part", old)
		staleTwitter := writeImportFile(t, dataDir, "twitter_archive_600_eeff0011.zip.part", old)
		// A fresh .part file is still being written by the other process of a
		// rolling deploy; sweeping it would make the eventual rename fail.
		fresh := writeImportFile(t, dataDir, "import_601_22334455.zip.part", startedAt.Add(time.Minute))

		n, err := CleanupOrphanImportFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 2 {
			t.Errorf("removed = %d, want 2", n)
		}
		for _, path := range []string{staleImport, staleTwitter} {
			if fileExists(path) {
				t.Errorf("%s still exists, want removed", filepath.Base(path))
			}
		}
		if !fileExists(fresh) {
			t.Errorf("fresh temp file was removed, want kept")
		}
	})

	t.Run("stale extract dir is removed when no import job is active", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		// Crash leftover of transfer.importStagingDir (SIGKILL/OOM before the
		// deferred RemoveAll ran).
		stale := makeImportStagingDir(t, dataDir, "extract_20260101_120000_1234_abcd1234", old)
		// Directories without the extract_ prefix are never candidates.
		other := makeImportStagingDir(t, dataDir, "other_dir", old)

		n, err := CleanupOrphanImportFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 1 {
			t.Errorf("removed = %d, want 1", n)
		}
		if fileExists(stale) {
			t.Errorf("stale extract dir still exists, want removed")
		}
		if !fileExists(other) {
			t.Errorf("non-extract dir was removed, want kept")
		}
	})

	t.Run("extract dir is kept while an import job is active", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		ctx := t.Context()
		// A queued import_db job may belong to the other process of a rolling
		// deploy, still extracting into its own staging dir; no extract_* dir
		// is swept while any import job is active.
		if _, err := NewEnqueuer(d).Enqueue(ctx, KindImportDB, map[string]any{"path": filepath.Join(dataDir, "imports", "import_700_abcdef01.zip")}, time.Now()); err != nil {
			t.Fatalf("enqueue import_db: %v", err)
		}
		stale := makeImportStagingDir(t, dataDir, "extract_20260101_120000_1234_abcd1234", old)

		n, err := CleanupOrphanImportFiles(ctx, query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		if !fileExists(stale) {
			t.Errorf("extract dir was removed while an import job is active, want kept")
		}
	})

	t.Run("fresh extract dir is kept even with no active import job", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		fresh := makeImportStagingDir(t, dataDir, "extract_20260101_120000_1234_abcd1234", startedAt.Add(time.Minute))

		n, err := CleanupOrphanImportFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		if !fileExists(fresh) {
			t.Errorf("fresh extract dir was removed, want kept")
		}
	})

	t.Run("missing imports dir is a no-op", func(t *testing.T) {
		d := openDB(t)
		n, err := CleanupOrphanImportFiles(t.Context(), query.New(d), t.TempDir(), startedAt)
		if err != nil {
			t.Fatalf("CleanupOrphanImportFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
	})
}

// insertReapFile inserts a files row with the given key and created_at and
// returns its id. variantOf != 0 links the row as a variant of that file.
func insertReapFile(t *testing.T, d *sql.DB, key string, createdAt, variantOf int64) int64 {
	t.Helper()
	var vo sql.NullInt64
	if variantOf != 0 {
		vo = sql.NullInt64{Int64: variantOf, Valid: true}
	}
	res, err := d.Exec(
		`INSERT INTO files (key, filename, byte_size, variant_of, created_at) VALUES (?, 'x.png', 1, ?, ?)`,
		key, vo, createdAt,
	)
	if err != nil {
		t.Fatalf("insert file %s: %v", key, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("file id: %v", err)
	}
	return id
}

// writeReapBlob writes a disk blob under the media layout for key.
func writeReapBlob(t *testing.T, dataDir, key string) string {
	t.Helper()
	path := filepath.Join(dataDir, "files", key[0:2], key[2:4], key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir blob dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("blob"), 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return path
}

func fileRowExists(t *testing.T, d *sql.DB, id int64) bool {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM files WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("count files: %v", err)
	}
	return n > 0
}

func TestReapOrphanFiles(t *testing.T) {
	startedAt := time.Now()
	old := startedAt.Add(-25 * time.Hour).Unix()

	t.Run("orphan rows and blobs are removed", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		// Crash leftovers of twitterarchive storeMediaEntry / twittersync
		// downloadMedia: the files row and blob committed, the referencing
		// attachment never did.
		orphanA := insertReapFile(t, d, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", old, 0)
		orphanB := insertReapFile(t, d, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", old, 0)
		blobA := writeReapBlob(t, dataDir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		blobB := writeReapBlob(t, dataDir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

		n, err := ReapOrphanFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("ReapOrphanFiles: %v", err)
		}
		if n != 2 {
			t.Errorf("removed = %d, want 2", n)
		}
		for _, id := range []int64{orphanA, orphanB} {
			if fileRowExists(t, d, id) {
				t.Errorf("file row %d still exists, want removed", id)
			}
		}
		for _, path := range []string{blobA, blobB} {
			if fileExists(path) {
				t.Errorf("blob %s still exists, want removed", filepath.Base(path))
			}
		}
	})

	t.Run("referenced files are kept", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		ctx := t.Context()
		attached := insertReapFile(t, d, "cccccccccccccccccccccccccccccccc", old, 0)
		if _, err := d.Exec(
			`INSERT INTO attachments (file_id, record_type, record_id, name, created_at) VALUES (?, 'Article', 1, 'embeds', 0)`,
			attached,
		); err != nil {
			t.Fatalf("attach: %v", err)
		}
		static := insertReapFile(t, d, "dddddddddddddddddddddddddddddddd", old, 0)
		if _, err := d.Exec(
			`INSERT INTO static_files (filename, file_id, created_at, updated_at) VALUES ('robots.txt', ?, 0, 0)`,
			static,
		); err != nil {
			t.Fatalf("insert static file: %v", err)
		}
		// Admin uploads and RSS-imported images are embedded in content by
		// URL and never get an attachment row.
		embedded := insertReapFile(t, d, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", old, 0)
		if _, err := d.Exec(
			`INSERT INTO articles (content_html, created_at, updated_at) VALUES (?, 0, 0)`,
			`<p><img src="/files/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"></p>`,
		); err != nil {
			t.Fatalf("insert article: %v", err)
		}
		embeddedBlob := writeReapBlob(t, dataDir, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")

		n, err := ReapOrphanFiles(ctx, query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("ReapOrphanFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		for _, id := range []int64{attached, static, embedded} {
			if !fileRowExists(t, d, id) {
				t.Errorf("file row %d was removed, want kept", id)
			}
		}
		if !fileExists(embeddedBlob) {
			t.Errorf("content-referenced blob was removed, want kept")
		}
	})

	t.Run("fresh rows are kept even when unreferenced", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		// A row stored an hour before this process started (mid import batch,
		// or an editor upload whose URL sits in a draft saved hours later)
		// predates the process start but is inside the 24 hour margin.
		fresh := insertReapFile(t, d, "ffffffffffffffffffffffffffffffff", startedAt.Add(-time.Hour).Unix(), 0)

		n, err := ReapOrphanFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("ReapOrphanFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		if !fileRowExists(t, d, fresh) {
			t.Errorf("fresh file row was removed, want kept")
		}
	})

	t.Run("orphan family is removed variants first", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		orig := insertReapFile(t, d, "01010101010101010101010101010101", old, 0)
		variant := insertReapFile(t, d, "02020202020202020202020202020202", old, orig)
		origBlob := writeReapBlob(t, dataDir, "01010101010101010101010101010101")
		variantBlob := writeReapBlob(t, dataDir, "02020202020202020202020202020202")

		n, err := ReapOrphanFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("ReapOrphanFiles: %v", err)
		}
		if n != 2 {
			t.Errorf("removed = %d, want 2", n)
		}
		for _, id := range []int64{orig, variant} {
			if fileRowExists(t, d, id) {
				t.Errorf("file row %d still exists, want removed", id)
			}
		}
		for _, path := range []string{origBlob, variantBlob} {
			if fileExists(path) {
				t.Errorf("blob %s still exists, want removed", filepath.Base(path))
			}
		}
	})

	t.Run("a referenced variant keeps the family", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		orig := insertReapFile(t, d, "03030303030303030303030303030303", old, 0)
		variant := insertReapFile(t, d, "04040404040404040404040404040404", old, orig)
		if _, err := d.Exec(
			`INSERT INTO attachments (file_id, record_type, record_id, name, created_at) VALUES (?, 'Article', 1, 'embeds', 0)`,
			variant,
		); err != nil {
			t.Fatalf("attach variant: %v", err)
		}

		n, err := ReapOrphanFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("ReapOrphanFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		for _, id := range []int64{orig, variant} {
			if !fileRowExists(t, d, id) {
				t.Errorf("file row %d was removed, want kept", id)
			}
		}
	})

	t.Run("content referencing a variant key keeps the family", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		// No write path emits variant URLs, but a database import can merge
		// bundle content that carries one.
		orig := insertReapFile(t, d, "05050505050505050505050505050505", old, 0)
		variant := insertReapFile(t, d, "06060606060606060606060606060606", old, orig)
		origBlob := writeReapBlob(t, dataDir, "05050505050505050505050505050505")
		variantBlob := writeReapBlob(t, dataDir, "06060606060606060606060606060606")
		if _, err := d.Exec(
			`INSERT INTO articles (content_html, created_at, updated_at) VALUES (?, 0, 0)`,
			`<p><img src="/files/06060606060606060606060606060606"></p>`,
		); err != nil {
			t.Fatalf("insert article: %v", err)
		}

		n, err := ReapOrphanFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("ReapOrphanFiles: %v", err)
		}
		if n != 0 {
			t.Errorf("removed = %d, want 0", n)
		}
		for _, id := range []int64{orig, variant} {
			if !fileRowExists(t, d, id) {
				t.Errorf("file row %d was removed, want kept", id)
			}
		}
		for _, path := range []string{origBlob, variantBlob} {
			if !fileExists(path) {
				t.Errorf("blob %s was removed, want kept", filepath.Base(path))
			}
		}
	})

	t.Run("row with an unsafe key is removed without touching disk", func(t *testing.T) {
		d := openDB(t)
		dataDir := t.TempDir()
		// Migrated rows can carry arbitrary legacy keys; only the row goes.
		bad := insertReapFile(t, d, "../evil", old, 0)

		n, err := ReapOrphanFiles(t.Context(), query.New(d), dataDir, startedAt)
		if err != nil {
			t.Fatalf("ReapOrphanFiles: %v", err)
		}
		if n != 1 {
			t.Errorf("removed = %d, want 1", n)
		}
		if fileRowExists(t, d, bad) {
			t.Errorf("unsafe-key file row still exists, want removed")
		}
	})
}
