// Rails sqlite import: runs railsmigrate against an uploaded Rails rables
// database (production.sqlite3) and optionally restores an uploaded zip of
// the Rails storage/ directory into data/files (ActiveStorage disk layout,
// xx/yy/<key>, which matches the Go media layout; existing blobs are kept).
package transfer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
		if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 2 || !media.ValidKey(parts[2]) {
			return nil // not a blob path (e.g. stray metadata files): ignore
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
	var storageNote string
	if p.StoragePath != "" {
		copied, kept, err := restoreStorageZip(dataDir, p.StoragePath)
		if err != nil {
			return "", err
		}
		storageNote = fmt.Sprintf(" storage_blobs_copied=%d storage_blobs_kept=%d", copied, kept)
	}

	fi, err := os.Stat(p.DBPath)
	if err != nil {
		return "", fmt.Errorf("import rails: open database: %w", err)
	}
	if fi.Size() == 0 {
		return "", fmt.Errorf("import rails: %s is empty (0 bytes)", p.DBPath)
	}
	oldDB, err := sql.Open("sqlite", "file:"+p.DBPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", fmt.Errorf("import rails: open database: %w", err)
	}
	defer oldDB.Close()

	var report strings.Builder
	rep, err := railsmigrate.Run(ctx, oldDB, db, railsmigrate.Options{
		Out:         &report,
		DataDir:     dataDir,
		VerifyFiles: true,
	})
	if err != nil {
		return "", fmt.Errorf("import rails: %w", err)
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
// and swallowed, like the other import jobs (no retry).
func RegisterImportHandlers(w *jobs.Worker, db *sql.DB, dataDir string) {
	RegisterImportDBHandler(w, db, dataDir)

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
		cleanupImportUpload(dataDir, p.DBPath)
		if p.StoragePath != "" {
			cleanupImportUpload(dataDir, p.StoragePath)
		}
		if report != "" {
			activity.Log(ctx, db, "info", "report", "import", activity.Quote(report))
		}
		if err != nil {
			activity.Log(ctx, db, "error", "failed", "import", fmt.Sprintf("source=\"rails\" file=%s error=%s", activity.Quote(filepath.Base(p.DBPath)), activity.Quote(err.Error())))
			return nil
		}
		activity.Log(ctx, db, "info", "completed", "import", fmt.Sprintf("source=\"rails\" file=%s", activity.Quote(filepath.Base(p.DBPath))))
		return nil
	})

	registerImportRSSHandler(w, db, dataDir)
}
