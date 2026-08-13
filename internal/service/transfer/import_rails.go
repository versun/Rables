// Rails sqlite import: runs railsmigrate against an uploaded Rails rables
// database (production.sqlite3) and optionally restores an uploaded zip of
// the Rails storage/ directory into data/files (ActiveStorage disk layout,
// xx/yy/<key>, which matches the Go media layout; existing blobs are kept).
package transfer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/media"
	"rables/internal/service/railsmigrate"
)

// ImportRailsPayload is the job_runs payload for kind "import_rails".
type ImportRailsPayload struct {
	// DBPath is the uploaded Rails sqlite database.
	DBPath string `json:"db_path"`
	// StoragePath is an optional uploaded zip of the Rails storage/ tree.
	StoragePath string `json:"storage_path,omitempty"`
}

// restoreStorageZip extracts a Rails storage zip into <DataDir>/files. Only
// entries in the ActiveStorage disk layout (xx/yy/<key>, optionally wrapped
// in a storage/ directory) are copied; blobs already on disk are kept.
// It returns the number of copied and kept blobs.
func restoreStorageZip(dataDir, zipPath string) (copied, kept int, err error) {
	stage, err := importStagingDir(dataDir)
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(stage)
	if err := extractImportZip(zipPath, stage); err != nil {
		return 0, 0, err
	}

	err = filepath.WalkDir(stage, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) > 0 && parts[0] == "storage" {
			parts = parts[1:]
		}
		if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 2 || !media.ValidKey(parts[2]) ||
			parts[0] != parts[2][0:2] || parts[1] != parts[2][2:4] {
			return nil // not a blob path (e.g. stray metadata files) or dirs don't match the key: ignore
		}
		dest := filepath.Join(dataDir, "files", parts[0], parts[1], parts[2])
		if _, err := os.Stat(dest); err == nil {
			kept++
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := copyFile(p, dest); err != nil {
			return fmt.Errorf("import rails: restore blob %s: %w", strings.Join(parts, "/"), err)
		}
		copied++
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return copied, kept, nil
}

// runRailsImport performs the whole job; extracted for testing.
func runRailsImport(ctx context.Context, db *sql.DB, dataDir string, p ImportRailsPayload) (string, error) {
	fi, err := os.Stat(p.DBPath)
	if err != nil {
		return "", fmt.Errorf("import rails: open database: %w", err)
	}
	if fi.Size() == 0 {
		return "", fmt.Errorf("import rails: %s is empty (0 bytes)", p.DBPath)
	}
	absDB, err := filepath.Abs(p.DBPath)
	if err != nil {
		return "", fmt.Errorf("import rails: open database: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: absDB, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	oldDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", fmt.Errorf("import rails: open database: %w", err)
	}
	defer oldDB.Close()

	var storageNote string
	var report strings.Builder
	rep, err := railsmigrate.Run(ctx, oldDB, db, railsmigrate.Options{
		Out:         &report,
		DataDir:     dataDir,
		VerifyFiles: true,
		// Blobs restore after the migration committed and before the files
		// check (same rule as the db import): a failed migration must not
		// leave orphan blobs no files row points at, and the check must run
		// after the restore or it reports every blob as missing.
		BeforeVerify: func() error {
			if p.StoragePath == "" {
				return nil
			}
			copied, kept, err := restoreStorageZip(dataDir, p.StoragePath)
			if err != nil {
				// The migration already committed; surface this as a
				// BlobRestoreError so the handler logs that re-uploading
				// both files heals the import (the migration is idempotent,
				// INSERT OR IGNORE, so a re-run restores the missing blobs).
				return &BlobRestoreError{Err: err}
			}
			storageNote = fmt.Sprintf(" storage_blobs_copied=%d storage_blobs_kept=%d", copied, kept)
			return nil
		},
	})
	if err != nil {
		return strings.TrimSpace(report.String()), fmt.Errorf("import rails: %w", err)
	}
	summary := strings.TrimSpace(report.String())
	if rep.Mismatch() {
		return summary + storageNote, fmt.Errorf("import rails: row count mismatch, see the report in the activity log")
	}
	return summary + storageNote, nil
}

// RegisterImportHandlers installs the import job handlers: kind "import_db"
// (Rables sqlite bundle), kind "import_rails" (Rails sqlite + optional
// storage zip) and kind "import_rss". Failures are logged to activity_logs
// and swallowed, like the other import jobs (no retry). invalidate is passed
// to the import_db handler, which calls it after a committed import so the
// settings cache drops the row the import overwrote; it may be nil.
func RegisterImportHandlers(w *jobs.Worker, db *sql.DB, dataDir string, invalidate func()) {
	RegisterImportDBHandler(w, db, dataDir, invalidate)

	w.Register(jobs.KindImportRails, func(ctx context.Context, payload json.RawMessage) error {
		var p ImportRailsPayload
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &p); err != nil {
				return fmt.Errorf("import_rails: decode payload: %w", err)
			}
		}
		if p.DBPath == "" {
			return fmt.Errorf("import_rails: db_path required")
		}
		activity.Log(ctx, db, "info", "started", "import", fmt.Sprintf("source=\"rails\" file=%s", activity.Quote(filepath.Base(p.DBPath))))
		report, err := runRailsImport(ctx, db, dataDir, p)
		var blobErr *BlobRestoreError
		rowsCommitted := errors.As(err, &blobErr)
		cleanupImportUpload(dataDir, p.DBPath)
		// Unlike the db import there is no server-file retry channel for
		// rails imports, so a kept storage zip could never be replayed: both
		// uploads are removed even when a blob-restore failure left
		// committed rows behind. The migration is idempotent (INSERT OR
		// IGNORE), so re-uploading both files re-runs cleanly and restores
		// the missing blobs.
		if p.StoragePath != "" {
			cleanupImportUpload(dataDir, p.StoragePath)
		}
		if report != "" {
			activity.Log(ctx, db, "info", "report", "import", activity.Quote(report))
		}
		if err != nil {
			detail := fmt.Sprintf("source=\"rails\" file=%s error=%s", activity.Quote(filepath.Base(p.DBPath)), activity.Quote(err.Error()))
			if rowsCommitted {
				detail += " rows_committed=true blobs_partially_restored=true (re-upload both files to finish the restore)"
			}
			activity.Log(ctx, db, "error", "failed", "import", detail)
			return nil
		}
		activity.Log(ctx, db, "info", "completed", "import", fmt.Sprintf("source=\"rails\" file=%s", activity.Quote(filepath.Base(p.DBPath))))
		return nil
	})

	registerImportRSSHandler(w, db, dataDir)
}
