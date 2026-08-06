// Database export (replacing the old CSV bundle): a zip containing a
// consistent copy of the SQLite database (VACUUM INTO) plus every media blob
// under data/files. The layout is what ImportDB consumes:
//
//	rables.db:           full SQLite database copy
//	files/xx/yy/<key>:   raw content of each media blob
package transfer

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/media"
)

// BundleExporter writes the database+media export zip into <DataDir>/exports.
type BundleExporter struct {
	DB      *sql.DB
	DataDir string
}

// Generate produces the export zip and returns its path.
func (e *BundleExporter) Generate(ctx context.Context) (string, error) {
	stage, err := stagingDir(e.DataDir, "export")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)

	dbCopy := filepath.Join(stage, "rables.db")
	if err := vacuumInto(ctx, e.DB, dbCopy); err != nil {
		return "", err
	}
	return e.zipBundle(stage, dbCopy)
}

// stagingDir creates <dataDir>/exports/<prefix>_<ts>_<pid>_<rand> for assembly.
func stagingDir(dataDir, prefix string) (string, error) {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s_%d_%s", prefix, time.Now().UTC().Format("20060102_150405"), os.Getpid(), hex.EncodeToString(rnd[:]))
	dir := filepath.Join(dataDir, "exports", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// vacuumInto writes a consistent, compacted copy of the live database to
// dest. It runs on a dedicated connection because VACUUM cannot run inside a
// transaction.
func vacuumInto(ctx context.Context, db *sql.DB, dest string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("export: acquire connection: %w", err)
	}
	defer conn.Close()
	// VACUUM INTO takes a string literal; dest is a generated path, quote it
	// defensively anyway.
	literal := "'" + strings.ReplaceAll(dest, "'", "''") + "'"
	if _, err := conn.ExecContext(ctx, "VACUUM INTO "+literal); err != nil {
		return fmt.Errorf("export: vacuum into: %w", err)
	}
	return nil
}

// zipBundle packs the staged database copy plus the media blobs under
// data/files into <stage>.zip (entries sorted, relative slash paths).
// Files that do not match the blob layout (xx/yy/<key>) are skipped, so
// litter like .DS_Store never makes it into a backup.
func (e *BundleExporter) zipBundle(stage, dbCopy string) (string, error) {
	// entries: zip entry name -> disk path
	entries := map[string]string{"rables.db": dbCopy}
	filesRoot := filepath.Join(e.DataDir, "files")
	walkErr := filepath.WalkDir(filesRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == filesRoot && os.IsNotExist(err) {
				return nil // missing files/ tree is fine (no uploads yet)
			}
			return err // a backup tool must not silently skip unreadable files
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(filesRoot, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 2 || !media.ValidKey(parts[2]) {
			return nil // not a media blob (e.g. a stray .DS_Store): skip
		}
		entries["files/"+filepath.ToSlash(rel)] = path
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("export: scan media files: %w", walkErr)
	}

	zipPath := stage + ".zip"
	out, err := os.Create(zipPath)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(out)

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := addZipEntry(zw, name, entries[name]); err != nil {
			zw.Close()
			out.Close()
			os.Remove(zipPath)
			return "", fmt.Errorf("export: pack %s: %w", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		out.Close()
		os.Remove(zipPath)
		return "", fmt.Errorf("export: finalize zip: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(zipPath)
		return "", fmt.Errorf("export: finalize zip: %w", err)
	}
	return zipPath, nil
}

// addZipEntry streams one disk file into the zip as name.
func addZipEntry(zw *zip.Writer, name, path string) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// RegisterExportHandlers installs the kind "export" job handler: it runs the
// bundle exporter into <dataDir>/exports and logs the outcome.
func RegisterExportHandlers(w *jobs.Worker, db *sql.DB, dataDir string) {
	w.Register(jobs.KindExport, func(ctx context.Context, payload json.RawMessage) error {
		path, err := (&BundleExporter{DB: db, DataDir: dataDir}).Generate(ctx)
		if err != nil {
			activity.Log(ctx, db, "error", "failed", "export", fmt.Sprintf("error=%s", activity.Quote(err.Error())))
			return err
		}
		activity.Log(ctx, db, "info", "completed", "export", fmt.Sprintf("file=%s", activity.Quote(path)))
		return nil
	})
}
