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
	"regexp"
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
	// KeepOnFailure leaves Path on disk when the import fails, so a
	// server-side file (copied into imports/ by the admin) can be retried.
	// Web uploads are always removed, success or failure.
	KeepOnFailure bool `json:"keep_on_failure,omitempty"`
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

// Import runs the import and returns the tallies. A failure before or during
// the row copy rolls the database transaction back, leaving the live database
// untouched. A blob-restore failure happens after the rows committed and is
// returned as *BlobRestoreError so the caller can keep the source file for a
// healing retry.
func (z *DBImporter) Import(ctx context.Context, path string) (*DBImportResult, error) {
	dbPath := path
	var stage string
	isZip, err := isZipBundle(path)
	if err != nil {
		return nil, err
	}
	if isZip {
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
	if err := z.copyTables(ctx, dbPath, res); err != nil {
		return nil, err
	}
	// Blobs restore after the row copy has committed: they live on disk
	// outside the transaction, so a bundle rejected by the schema check (or
	// a failed copy) must not leave orphan blobs no files row points at.
	if stage != "" {
		if err := z.restoreBlobs(filepath.Join(stage, "files"), res); err != nil {
			return nil, &BlobRestoreError{Err: err}
		}
	}
	return res, nil
}

// isZipBundle sniffs the file magic instead of trusting the name: the admin
// server-file import renames the file to <name>.queued before enqueueing, so
// a .zip suffix check would route a queued ZIP into ATTACH as a bare SQLite
// file and fail with 'file is not a database'. Anything that is not a ZIP
// takes the bare-database path and lets ATTACH report a bad file.
func isZipBundle(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("import db: open source: %w", err)
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("import db: read source: %w", err)
	}
	return magic == [4]byte{'P', 'K', 0x03, 0x04}, nil
}

// BlobRestoreError wraps a blob-restore failure: the row copy had already
// committed when it happened, so the database rows are in while some blobs
// may be missing. Retrying the same bundle heals it (the row upserts are
// idempotent and blobs already on disk are kept), so the caller must keep
// the source file even when it would normally remove it after a failure.
type BlobRestoreError struct {
	Err error
}

func (e *BlobRestoreError) Error() string { return e.Err.Error() }
func (e *BlobRestoreError) Unwrap() error { return e.Err }

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
		if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 2 || !media.ValidKey(parts[2]) ||
			parts[0] != parts[2][0:2] || parts[1] != parts[2][2:4] {
			return nil // not a blob path (e.g. .DS_Store) or dirs don't match the key: skip
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
		if table == "twitter_archive_imports" && n > 0 {
			if err := normalizeTwitterArchiveImports(ctx, tx); err != nil {
				return fmt.Errorf("import db: %s: %w", table, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("import db: commit: %w", err)
	}
	return nil
}

// checkSourceSchema verifies the attached source looks like a Go rables
// database and not a Rails one (which must go through the Rails import).
// Every table the import copies must be a real table: a VIEW passes PRAGMA
// table_info too, and a hostile bundle could define one reading excluded
// runtime tables (main.sessions tokens, users.password_digest) into public
// content, so a source object that exists but is not a table aborts the
// import (absent tables are skipped, as before).
func checkSourceSchema(ctx context.Context, conn *sql.Conn) error {
	objects := map[string]string{}
	rows, err := conn.QueryContext(ctx, "SELECT name, type FROM src.sqlite_master")
	if err != nil {
		return fmt.Errorf("import db: inspect source schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return err
		}
		objects[name] = typ
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if objects["action_text_rich_texts"] == "table" || objects["active_storage_blobs"] == "table" {
		return errors.New("import db: the uploaded database is a Rails rables database; use the Rails import instead")
	}
	var missing []string
	for _, req := range []string{"articles", "pages", "settings", "files"} {
		if objects[req] != "table" {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("import db: source database is missing tables (%s); not a Rables database", strings.Join(missing, ", "))
	}
	for _, table := range dbImportTables {
		if typ, ok := objects[table]; ok && typ != "table" {
			return fmt.Errorf("import db: source %s is a %s, not a table; refusing to import", table, typ)
		}
	}
	// Source column names are interpolated into the upsert statements (quoted
	// via quoteIdent, but defense in depth): a name outside the plain
	// identifier pattern means the source was crafted, not exported.
	for _, table := range dbImportTables {
		if objects[table] != "table" {
			continue
		}
		cols, err := tableColumns(ctx, conn, "src", table)
		if err != nil {
			return fmt.Errorf("import db: inspect source %s: %w", table, err)
		}
		for _, col := range cols {
			if !validColumnName.MatchString(col) {
				return fmt.Errorf("import db: source %s has invalid column name %q; refusing to import", table, col)
			}
		}
	}
	return nil
}

// validColumnName matches the plain identifiers every real schema carries
// (Rails and Go column names are all alphanumeric/underscore); anything else
// in a source database means a crafted bundle.
var validColumnName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

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
	selected := make([]string, len(cols))
	hasStatus := false
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
		selected[i] = quoted[i]
		if c == "status" {
			hasStatus = true
		}
	}
	// A backup taken while an archive import was queued/running carries that
	// row with active_slot=1; upserted as-is it would collide with the live
	// active row in idx_tai_active_slot before normalizeTwitterArchiveImports
	// gets to run, rolling the whole import back. Terminal rows always have a
	// NULL slot (Complete/FailTwitterArchiveImport release it), so only active
	// rows are neutralized here; the normalization then fails them.
	if table == "twitter_archive_imports" && hasStatus {
		for i, c := range cols {
			if c == "active_slot" {
				selected[i] = `CASE WHEN "status" IN ('queued', 'running') THEN NULL ELSE "active_slot" END`
			}
		}
	}
	stmt := fmt.Sprintf(`INSERT INTO main.%s (%s) SELECT %s FROM src.%s WHERE true`,
		quoteIdent(table), strings.Join(quoted, ", "), strings.Join(selected, ", "), quoteIdent(table))
	var updates []string
	for _, c := range cols {
		if c == "id" {
			continue
		}
		updates = append(updates, fmt.Sprintf(`%s = excluded.%s`, quoteIdent(c), quoteIdent(c)))
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

// normalizeTwitterArchiveImports strips the runtime status from imported
// twitter_archive_imports rows. A backup taken while an archive import was
// queued/running carries that row; imported as-is it would block new archive
// uploads (HasActiveTwitterArchiveImport) until startup recovery fails it.
// The imported active rows are marked failed instead, while the rest of the
// row is kept for the history list. active_slot was already neutralized on
// the upsert's SELECT side (upsertTable), and source_path is kept on
// purpose: on a same-server restore the source zip is still on disk and the
// still-queued job (job_runs is never imported, so the live one survives)
// self-heals by re-running it, while a row restored elsewhere points at a
// missing file and the import handler treats it as terminal
// (sourceFilePresent). Rows absent from the source are left alone, so an
// archive import queued in this database survives a bundle restore.
func normalizeTwitterArchiveImports(ctx context.Context, tx *sql.Tx) error {
	now := time.Now().Unix()
	_, err := tx.ExecContext(ctx, `UPDATE main.twitter_archive_imports
		SET status = 'failed', status_message = 'Import failed',
		    error_message = 'The server was restored from a backup taken while this import was still active',
		    finished_at = ?, updated_at = ?
		WHERE status IN ('queued', 'running')
		  AND id IN (SELECT id FROM src.twitter_archive_imports)`, now, now)
	return err
}

// queryer is satisfied by *sql.Conn (checkSourceSchema) and *sql.Tx
// (upsertTable).
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// tableColumns returns the column names of schema.table, or nil when the
// table does not exist. Names come from the fixed dbImportTables whitelist.
func tableColumns(ctx context.Context, q queryer, schema, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA %s.table_info(%s)", schema, quoteIdent(table)))
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

// copyFile streams src to dst atomically (see writeFileAtomic).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFileAtomic(dst, in)
}

// writeFileAtomic lands the content in a temporary sibling of target first
// and renames it into place (same directory, so the rename is atomic): the
// blob restores treat an existing destination as "kept" and never re-copy,
// and serveFile caches blob responses for a year, so target must never exist
// with partial content — neither mid-copy (concurrent readers) nor after a
// crash between create and close. A crash can still orphan the .part-*
// sibling, which is harmless litter no key points at; a failed copy or
// rename removes it.
func writeFileAtomic(target string, rc io.Reader) error {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := target + ".part-" + hex.EncodeToString(rnd[:])
	if err := writeFileFrom(tmp, rc); err != nil {
		return err // writeFileFrom already removed the partial temp file
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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

// MaxImportExtractBytes caps the total declared uncompressed size of an
// import ZIP (10GB). Upload limits only cover the compressed size; without
// this cap a zip bomb could fill the disk during extraction.
const MaxImportExtractBytes = 10 << 30

// MaxImportExtractEntries caps the number of file entries in an import ZIP:
// millions of tiny entries stay under the byte limit but would exhaust
// inodes during extraction.
const MaxImportExtractEntries = 100000

// extractImportZip unpacks every regular file entry into stage, rejecting
// entries whose path would escape it. Any unsafe entry aborts the import.
// The declared uncompressed sizes are summed up front and the archive is
// rejected once the total passes MaxImportExtractBytes, so a bomb is refused
// before anything is written (checking mid-extraction would come too late:
// Go's zip reader already fails a lying entry with unexpected EOF). The entry
// count is likewise capped at MaxImportExtractEntries up front, so a flood of
// tiny entries cannot exhaust inodes.
func extractImportZip(zipPath, stage string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("import: open zip: %w", err)
	}
	defer zr.Close()

	var declared uint64
	var entries int
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		entries++
		if entries > MaxImportExtractEntries {
			return fmt.Errorf("import: ZIP has more than %d file entries", MaxImportExtractEntries)
		}
		// Compare before adding: the invariant declared <= MaxImportExtractBytes
		// keeps the subtraction from underflowing and the running total can
		// never wrap around on a forged zip64 declared size.
		if f.UncompressedSize64 > MaxImportExtractBytes-declared {
			return fmt.Errorf("import: ZIP entry %q pushes the declared uncompressed total over the %d byte limit", f.Name, MaxImportExtractBytes)
		}
		declared += f.UncompressedSize64
	}

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
	if copyErr != nil {
		// os.Create already truncated/created target, so a failure midway
		// (e.g. disk full) leaves a partial file behind. Blob restore treats
		// an existing destination as "kept" and never re-copies, so drop the
		// fragment to let a retry copy the blob again. Harmless for zip
		// extraction too: a failed extract removes the whole staging dir.
		os.Remove(target)
	}
	return copyErr
}

// safeZipEntryName cleans a zip entry name and rejects anything that could
// escape the staging directory: absolute paths, drive letters, NUL bytes,
// backslashes (a separator on Windows, where the "/"-based ".." check would
// miss "..\.." traversal) and ".." segments (path traversal entries are
// refused, not sanitized).
func safeZipEntryName(name string) (string, error) {
	unsafe := fmt.Errorf("import: unsafe path in ZIP entry: %s", name)
	if name == "" || strings.ContainsRune(name, 0) || strings.ContainsRune(name, '\\') || strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
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

// keepImportUploadForRetry renames a kept import_* upload to the same name
// without the prefix, turning it into an ordinary server-side file: listed
// by the import tab, accepted by import_server, and ignored by
// jobs.CleanupOrphanImportFiles, which only reaps import_* /
// twitter_archive_* names owned by an enqueued job. Under the import_* name
// the kept file would be hidden from every retry channel and swept at the
// next startup. When the rename fails the original path is returned:
// keeping the file under its old name still beats dropping the only retry
// copy (the sweep simply reaps it next startup, as before).
func keepImportUploadForRetry(path string) string {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "import_") {
		return path
	}
	kept := filepath.Join(filepath.Dir(path), strings.TrimPrefix(base, "import_"))
	if err := os.Rename(path, kept); err != nil {
		return path
	}
	return kept
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
// other import jobs. invalidate is called whenever the import committed rows:
// the row copy upserts the settings table (dbImportTables) behind the
// settings.Cache's back, and settings.Cache's contract requires writers that
// bypass Update to invalidate, or public pages keep serving the old site
// title/URL/CSS until the TTL expires. It may be nil.
func RegisterImportDBHandler(w *jobs.Worker, db *sql.DB, dataDir string, invalidate func()) {
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
		var blobErr *BlobRestoreError
		rowsCommitted := errors.As(err, &blobErr)
		if invalidate != nil && (err == nil || rowsCommitted) {
			invalidate()
		}
		// A blob-restore failure left committed rows behind; keep the source
		// file for a healing retry even when it is a web upload.
		if err == nil || (!p.KeepOnFailure && !rowsCommitted) {
			cleanupImportUpload(dataDir, p.Path)
		} else if rowsCommitted {
			p.Path = keepImportUploadForRetry(p.Path)
		}
		if err != nil {
			detail := fmt.Sprintf("source=\"db\" file=%s error=%s", activity.Quote(filepath.Base(p.Path)), activity.Quote(err.Error()))
			if rowsCommitted {
				detail += " rows_committed=true source_kept=true"
			}
			activity.Log(ctx, db, "error", "failed", "import", detail)
			return nil
		}
		activity.Log(ctx, db, "info", "completed", "import", fmt.Sprintf("source=\"db\" file=%s %s", activity.Quote(filepath.Base(p.Path)), formatDBImportResult(res)))
		return nil
	})
}
