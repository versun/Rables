// Go sqlite import: consumes the bundle produced by BundleExporter (zip with
// rables.db + files/) or a bare rables.db upload. Rows are merged into the
// live database with an upsert on id (INSERT ... ON CONFLICT(id) DO UPDATE),
// so imported rows overwrite rows with the same id without deleting anything
// the source does not have (plain REPLACE would cascade-delete FK children).
// Runtime tables (sessions, activity_logs, job_runs, kv) are never imported.
// The whole copy runs in one transaction with foreign keys deferred to
// commit, so table order and self-references cannot trip FK checks midway.
package transfer

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/media"
)

// ImportDBPayload is the job_runs payload for kind "import_db".
type ImportDBPayload struct {
	// Path is the uploaded bundle or database file, usually
	// <DataDir>/imports/import_*.
	Path string `json:"path"`
}

// DBImporter imports a BundleExporter zip or a bare sqlite database.
type DBImporter struct {
	DB      *sql.DB
	DataDir string
}

// DBImportResult summarizes one import run for the activity log.
type DBImportResult struct {
	Rows        map[string]int64 // table -> rows written (inserted or updated)
	BlobsCopied int
	BlobsKept   int // already on disk, left untouched
}

// dbImportTables are the content tables copied from the source database, in
// dependency order. Runtime/state tables (sessions, activity_logs, job_runs,
// kv, goose_db_version) are deliberately excluded.
var dbImportTables = []string{
	"users", "articles", "pages", "tags", "article_tags", "comments",
	"subscribers", "subscriber_tags", "social_media_posts", "redirects",
	"files", "attachments", "static_files",
	"settings", "newsletter_settings", "crossposts", "listmonks",
	"twitter_syncs", "twitter_archive_tweets", "twitter_archive_connections",
	"twitter_archive_likes", "twitter_archive_imports",
}

// Import runs the import and returns the tallies. Any error rolls the
// database transaction back, leaving the live database untouched.
func (z *DBImporter) Import(ctx context.Context, path string) (*DBImportResult, error) {
	dbPath := path
	var stage string
	if strings.HasSuffix(strings.ToLower(path), ".zip") {
		var err error
		stage, err = importStagingDir(z.DataDir)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(stage)
		if err := extractImportZip(path, stage); err != nil {
			return nil, err
		}
		dbPath, err = findBundleDB(stage)
		if err != nil {
			return nil, err
		}
	}

	res := &DBImportResult{Rows: map[string]int64{}}
	if stage != "" {
		if err := z.restoreBlobs(filepath.Join(stage, "files"), res); err != nil {
			return nil, err
		}
	}
	if err := z.copyTables(ctx, dbPath, res); err != nil {
		return nil, err
	}
	return res, nil
}

// findBundleDB locates the database inside an extracted bundle: rables.db at
// the root wins, otherwise the first *.db/*.sqlite/*.sqlite3 file.
func findBundleDB(stage string) (string, error) {
	root := filepath.Join(stage, "rables.db")
	if info, err := os.Stat(root); err == nil && info.Mode().IsRegular() {
		return root, nil
	}
	var found string
	_ = filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" || !d.Type().IsRegular() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(d.Name())) {
		case ".db", ".sqlite", ".sqlite3":
			found = p
		}
		return nil
	})
	if found == "" {
		return "", errors.New("import db: no database file found in the bundle (not a Rables export ZIP)")
	}
	return found, nil
}

// restoreBlobs copies the staged files/ tree into <DataDir>/files, never
// overwriting a blob that already exists on disk. Entries that do not match
// the blob layout (xx/yy/<key>) are skipped, so litter like .DS_Store in a
// hand-made bundle cannot abort the import (same rule as restoreStorageZip).
func (z *DBImporter) restoreBlobs(tree string, res *DBImportResult) error {
	if info, err := os.Stat(tree); err != nil || !info.IsDir() {
		return nil // bundles without media are fine
	}
	return filepath.WalkDir(tree, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(tree, p)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 2 || !media.ValidKey(parts[2]) {
			return nil // not a blob path (e.g. .DS_Store): skip
		}
		dest := filepath.Join(z.DataDir, "files", parts[0], parts[1], parts[2])
		if _, err := os.Stat(dest); err == nil {
			res.BlobsKept++
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := copyFile(p, dest); err != nil {
			return fmt.Errorf("import db: restore blob %s: %w", rel, err)
		}
		res.BlobsCopied++
		return nil
	})
}

// copyTables attaches the source database and upserts every content table.
func (z *DBImporter) copyTables(ctx context.Context, srcPath string, res *DBImportResult) error {
	conn, err := z.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("import db: acquire connection: %w", err)
	}
	defer conn.Close()

	literal := "'" + strings.ReplaceAll(srcPath, "'", "''") + "'"
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE "+literal+" AS src"); err != nil {
		return fmt.Errorf("import db: attach source: %w", err)
	}
	defer conn.ExecContext(context.Background(), "DETACH DATABASE src")

	if err := checkSourceSchema(ctx, conn); err != nil {
		return err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("import db: begin transaction: %w", err)
	}
	defer tx.Rollback()
	// FK checks (including comments.parent_id self-references) are enforced at
	// commit, not per statement.
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
		return fmt.Errorf("import db: defer foreign keys: %w", err)
	}
	for _, table := range dbImportTables {
		n, err := upsertTable(ctx, tx, table)
		if err != nil {
			return fmt.Errorf("import db: %s: %w", table, err)
		}
		res.Rows[table] = n
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("import db: commit: %w", err)
	}
	return nil
}

// checkSourceSchema verifies the attached source looks like a Go rables
// database and not a Rails one (which must go through the Rails import).
func checkSourceSchema(ctx context.Context, conn *sql.Conn) error {
	tables := map[string]bool{}
	rows, err := conn.QueryContext(ctx, "SELECT name FROM src.sqlite_master WHERE type = 'table'")
	if err != nil {
		return fmt.Errorf("import db: inspect source schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		tables[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if tables["action_text_rich_texts"] || tables["active_storage_blobs"] {
		return errors.New("import db: the uploaded database is a Rails rables database; use the Rails import instead")
	}
	var missing []string
	for _, req := range []string{"articles", "pages", "settings", "files"} {
		if !tables[req] {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("import db: source database is missing tables (%s); not a Rables database", strings.Join(missing, ", "))
	}
	return nil
}

// upsertTable copies one table from src into main, overwriting rows whose id
// already exists. Columns are the intersection of both schemas (source column
// order), so exports from slightly different schema versions still load.
// Tables absent from the source are skipped. It returns the rows written.
// (WHERE true is required so SQLite does not read ON CONFLICT as a join.)
func upsertTable(ctx context.Context, tx *sql.Tx, table string) (int64, error) {
	srcCols, err := tableColumns(ctx, tx, "src", table)
	if err != nil {
		return 0, err
	}
	if srcCols == nil {
		return 0, nil // table not present in the source database
	}
	mainCols, err := tableColumns(ctx, tx, "main", table)
	if err != nil {
		return 0, err
	}
	inMain := map[string]bool{}
	for _, c := range mainCols {
		inMain[c] = true
	}
	var cols []string
	for _, c := range srcCols {
		if inMain[c] {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return 0, nil
	}

	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + c + `"`
	}
	stmt := fmt.Sprintf(`INSERT INTO main.%s (%s) SELECT %s FROM src.%s WHERE true`,
		quoteIdent(table), strings.Join(quoted, ", "), strings.Join(quoted, ", "), quoteIdent(table))
	var updates []string
	for _, c := range cols {
		if c == "id" {
			continue
		}
		updates = append(updates, fmt.Sprintf(`"%s" = excluded."%s"`, c, c))
	}
	if len(updates) > 0 {
		stmt += ` ON CONFLICT(id) DO UPDATE SET ` + strings.Join(updates, ", ")
	} else {
		stmt += ` ON CONFLICT(id) DO NOTHING`
	}
	res, err := tx.ExecContext(ctx, stmt)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// tableColumns returns the column names of schema.table, or nil when the
// table does not exist. Names come from the fixed dbImportTables whitelist.
func tableColumns(ctx context.Context, tx *sql.Tx, schema, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA %s.table_info(%s)", schema, quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// copyFile streams src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFileFrom(dst, in)
}

// importStagingDir creates <dataDir>/imports/extract_<ts>_<pid>_<rand>.
func importStagingDir(dataDir string) (string, error) {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	name := fmt.Sprintf("extract_%s_%d_%s", time.Now().UTC().Format("20060102_150405"), os.Getpid(), hex.EncodeToString(rnd[:]))
	dir := filepath.Join(dataDir, "imports", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("import db: create staging dir: %w", err)
	}
	return dir, nil
}

// extractImportZip unpacks every regular file entry into stage, rejecting
// entries whose path would escape it. Any unsafe entry aborts the import.
func extractImportZip(zipPath, stage string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("import: open zip: %w", err)
	}
	defer zr.Close()

	stageClean := filepath.Clean(stage)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel, err := safeZipEntryName(f.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(stageClean, filepath.FromSlash(rel))
		if target != stageClean && !strings.HasPrefix(target, stageClean+string(os.PathSeparator)) {
			return fmt.Errorf("import: unsafe path in ZIP entry: %s", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("import: extract %s: %w", f.Name, err)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("import: extract %s: %w", f.Name, err)
		}
		err = writeFileFrom(target, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("import: extract %s: %w", f.Name, err)
		}
	}
	return nil
}

func writeFileFrom(target string, rc io.Reader) error {
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, rc)
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	return copyErr
}

// safeZipEntryName cleans a zip entry name and rejects anything that could
// escape the staging directory: absolute paths, drive letters, NUL bytes and
// ".." segments (path traversal entries are refused, not sanitized).
func safeZipEntryName(name string) (string, error) {
	unsafe := fmt.Errorf("import: unsafe path in ZIP entry: %s", name)
	if name == "" || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", unsafe
	}
	if len(name) >= 2 && name[1] == ':' { // Windows drive letter
		return "", unsafe
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", unsafe
		}
	}
	clean := path.Clean(name)
	if clean == "." || clean == "" {
		return "", unsafe
	}
	return clean, nil
}

// cleanupImportUpload removes an uploaded file when it lives under
// <dataDir>/imports (the job owns the file once enqueued).
func cleanupImportUpload(dataDir, path string) {
	importsDir, err := filepath.Abs(filepath.Join(dataDir, "imports"))
	if err != nil {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil || !strings.HasPrefix(abs, importsDir+string(os.PathSeparator)) {
		return
	}
	os.Remove(abs)
}

// formatDBImportResult renders the tally line for the activity log.
func formatDBImportResult(res *DBImportResult) string {
	var written int64
	for _, n := range res.Rows {
		written += n
	}
	return fmt.Sprintf("rows_written=%d blobs_copied=%d blobs_kept=%d", written, res.BlobsCopied, res.BlobsKept)
}

// RegisterImportDBHandler installs the kind "import_db" job handler. Import
// failures are logged to activity_logs and swallowed (no job retry), like the
// other import jobs.
func RegisterImportDBHandler(w *jobs.Worker, db *sql.DB, dataDir string) {
	w.Register(jobs.KindImportDB, func(ctx context.Context, payload json.RawMessage) error {
		var p ImportDBPayload
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &p); err != nil {
				return fmt.Errorf("import_db: decode payload: %w", err)
			}
		}
		if p.Path == "" {
			return fmt.Errorf("import_db: path required")
		}
		activity.Log(ctx, db, "info", "started", "import", fmt.Sprintf("source=\"db\" file=%s", activity.Quote(filepath.Base(p.Path))))
		res, err := (&DBImporter{DB: db, DataDir: dataDir}).Import(ctx, p.Path)
		cleanupImportUpload(dataDir, p.Path)
		if err != nil {
			activity.Log(ctx, db, "error", "failed", "import", fmt.Sprintf("source=\"db\" file=%s error=%s", activity.Quote(filepath.Base(p.Path)), activity.Quote(err.Error())))
			return nil
		}
		activity.Log(ctx, db, "info", "completed", "import", fmt.Sprintf("source=\"db\" file=%s %s", activity.Quote(filepath.Base(p.Path)), formatDBImportResult(res)))
		return nil
	})
}
