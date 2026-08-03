// Package railsmigrate copies a Rails (ActiveRecord/SQLite) database into the
// Go rables schema, following plan section 8. The old database is read-only;
// rows are carried over with their original ids and skipped when the target
// table already holds them (INSERT OR IGNORE), so the tool is idempotent and
// can be re-run to catch up before cutover. Disk files are never copied - the
// old storage/ tree is mounted as DATA_DIR/files at deploy time.
package railsmigrate

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options tunes a migration run.
type Options struct {
	Out         io.Writer // report destination (required)
	DataDir     string    // Go DATA_DIR, used by --verify-files
	VerifyFiles bool      // check every files row has a disk file
}

// TableReport holds the per-table counts from the report (spec section 8.7).
type TableReport struct {
	Table       string
	Old         int64 // rows in the old database
	Inserted    int64 // rows written this run
	Skipped     int64 // rows already present (idempotent re-run)
	Transformed int64 // old rows folded into something else (not copied 1:1)
	NewTotal    int64 // rows in the new database after the run
	Expected    int64 // Old - Transformed
}

func (t *TableReport) ok() bool { return t.NewTotal == t.Expected }

// Report is the full migration report.
type Report struct {
	Tables  []*TableReport
	Rewrite RewriteStats
	// Notes carries human-readable caveats (unmigratable rows, spec gaps).
	Notes    []string
	Verified *VerifyReport
}

// VerifyReport summarizes --verify-files.
type VerifyReport struct {
	Total   int64
	Missing []string
}

// Mismatch reports whether any table row count diverges from the expected
// value; the process exit code is non-zero exactly in this case (plus fatal
// errors).
func (r *Report) Mismatch() bool {
	for _, t := range r.Tables {
		if !t.ok() {
			return true
		}
	}
	return false
}

// Print writes the report in the spec section 8.7 shape.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "%-28s %8s %8s %8s %11s %8s %8s  %s\n",
		"table", "old", "inserted", "skipped", "transformed", "total", "expected", "status")
	for _, t := range r.Tables {
		status := "OK"
		if !t.ok() {
			status = "MISMATCH"
		}
		fmt.Fprintf(w, "%-28s %8d %8d %8d %11d %8d %8d  %s\n",
			t.Table, t.Old, t.Inserted, t.Skipped, t.Transformed, t.NewTotal, t.Expected, status)
	}
	fmt.Fprintf(w, "\nattachment references: rewritten=%d kept=%d\n", r.Rewrite.Rewritten, len(r.Rewrite.Kept))
	if len(r.Rewrite.Kept) > 0 {
		fmt.Fprintln(w, "kept references (manual fixup list):")
		for _, k := range r.Rewrite.Kept {
			fmt.Fprintf(w, "  - %s: %s\n      %s\n", k.Record, k.Reason, k.Snippet)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	if r.Verified != nil {
		fmt.Fprintf(w, "\nfile verification: %d files rows, %d missing on disk\n", r.Verified.Total, len(r.Verified.Missing))
		for _, m := range r.Verified.Missing {
			fmt.Fprintf(w, "  - missing: %s\n", m)
		}
	}
	if r.Mismatch() {
		fmt.Fprintln(w, "\nRESULT: MISMATCH (exit code 1)")
	} else {
		fmt.Fprintln(w, "\nRESULT: OK")
	}
}

// migrator carries the run state.
type migrator struct {
	old *sql.DB
	tx  *sql.Tx // all writes go through one transaction
	rep *Report

	blobs     map[int64]blobRef
	richTexts map[int64]struct {
		typ string
		id  int64
	} // old rt id -> owner
}

// oldTables lists every Rails table the migrator reads. Run refuses to start
// when any are missing, so a wrong -old path (or an empty file, which sqlite
// accepts as a valid empty database) fails fast with a clear message instead
// of a "no such table" error midway through the run.
var oldTables = []string{
	"users", "tags", "articles", "pages", "comments", "article_tags",
	"subscribers", "subscriber_tags", "social_media_posts", "redirects",
	"static_files", "settings", "newsletter_settings", "crossposts", "listmonks",
	"twitter_syncs", "twitter_archive_tweets", "twitter_archive_connections",
	"twitter_archive_likes", "twitter_archive_imports",
	"action_text_rich_texts", "active_storage_blobs", "active_storage_attachments",
	"active_storage_variant_records",
}

// checkOldSchema verifies the old database looks like a Rails rables database.
func checkOldSchema(ctx context.Context, oldDB *sql.DB) error {
	var missing []string
	for _, name := range oldTables {
		var got string
		err := oldDB.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&got)
		if err == sql.ErrNoRows {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			return fmt.Errorf("check old schema: %w", err)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("old database is missing %d Rails tables (%s); "+
			"check that -old points at the Rails database (db/development.sqlite3)",
			len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// Run executes the full migration. oldDB must be opened read-only by the
// caller; newDB is the target (already goose-migrated by db.Open).
func Run(ctx context.Context, oldDB, newDB *sql.DB, opts Options) (*Report, error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if err := checkOldSchema(ctx, oldDB); err != nil {
		return nil, err
	}
	tx, err := newDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	m := &migrator{old: oldDB, tx: tx, rep: &Report{}}
	// rollback is a no-op after a successful commit
	defer func() { _ = tx.Rollback() }()

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"users", m.users},
		{"tags", m.tags},
		{"articles", m.articles},
		{"pages", m.pages},
		{"comments", m.comments},
		{"article_tags", m.articleTags},
		{"subscribers", m.subscribers},
		{"subscriber_tags", m.subscriberTags},
		{"social_media_posts", m.socialMediaPosts},
		{"redirects", m.redirects},
		{"files", m.files},
		{"attachments", m.attachments},
		{"static_files", m.staticFiles},
		{"settings", m.settings},
		{"newsletter_settings", m.newsletterSettings},
		{"crossposts", m.crossposts},
		{"listmonks", m.listmonks},
		{"twitter_syncs", m.twitterSyncs},
		{"twitter_archive_tweets", m.twitterArchiveTweets},
		{"twitter_archive_connections", m.twitterArchiveConnections},
		{"twitter_archive_likes", m.twitterArchiveLikes},
		{"twitter_archive_imports", m.twitterArchiveImports},
	}
	for _, s := range steps {
		if err := s.fn(ctx); err != nil {
			return nil, fmt.Errorf("migrate %s: %w", s.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	if err := m.fillTotals(ctx, newDB); err != nil {
		return nil, err
	}
	if opts.VerifyFiles {
		if err := m.verifyFiles(ctx, newDB, opts.DataDir); err != nil {
			return nil, err
		}
	}
	m.rep.Print(opts.Out)
	return m.rep, nil
}

// table starts a TableReport with the old row count.
func (m *migrator) table(ctx context.Context, name, oldTable string, transformed int64) (*TableReport, error) {
	var n int64
	if err := m.old.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+oldTable).Scan(&n); err != nil {
		return nil, fmt.Errorf("count old %s: %w", oldTable, err)
	}
	t := &TableReport{Table: name, Old: n, Transformed: transformed, Expected: n - transformed}
	m.rep.Tables = append(m.rep.Tables, t)
	return t, nil
}

// insert runs one INSERT OR IGNORE inside the transaction and updates counts.
func (m *migrator) insert(ctx context.Context, t *TableReport, query string, args ...any) error {
	res, err := m.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if aff == 0 {
		t.Skipped++
	} else {
		t.Inserted += aff
	}
	return nil
}

// insertSingleton wraps insert for the CHECK (id = 1) config tables: a skip
// there means the Go app already created the row (e.g. settings via
// EnsureSettings) and the old values were NOT carried over - worth a note.
func (m *migrator) insertSingleton(ctx context.Context, t *TableReport, query string, args ...any) error {
	if err := m.insert(ctx, t, query, args...); err != nil {
		return err
	}
	if t.Skipped > 0 {
		m.rep.Notes = append(m.rep.Notes,
			fmt.Sprintf("%s: row already exists in the new database, old values NOT carried over", t.Table))
	}
	return nil
}

// fillTotals loads the post-run new-table counts.
func (m *migrator) fillTotals(ctx context.Context, db *sql.DB) error {
	for _, t := range m.rep.Tables {
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.Table).Scan(&t.NewTotal); err != nil {
			return fmt.Errorf("count new %s: %w", t.Table, err)
		}
	}
	return nil
}

// verifyFiles checks that every files row has its blob on disk under
// DataDir/files/xx/yy/<key> (ActiveStorage disk layout, unchanged).
func (m *migrator) verifyFiles(ctx context.Context, db *sql.DB, dataDir string) error {
	rows, err := db.QueryContext(ctx, "SELECT key FROM files ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	v := &VerifyReport{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return err
		}
		v.Total++
		if len(key) < 4 {
			v.Missing = append(v.Missing, key+" (invalid key)")
			continue
		}
		p := filepath.Join(dataDir, "files", key[0:2], key[2:4], key)
		if _, err := os.Stat(p); err != nil {
			v.Missing = append(v.Missing, key)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m.rep.Verified = v
	return nil
}

// --- time conversion (spec section 8.6: Rails datetime text -> unix seconds, UTC)

var timeLayouts = []string{
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999Z07:00",
	"2006-01-02T15:04:05.999999",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// unix converts a nullable Rails datetime string to nullable unix seconds.
func unix(s sql.NullString) (sql.NullInt64, error) {
	if !s.Valid || strings.TrimSpace(s.String) == "" {
		return sql.NullInt64{}, nil
	}
	v := strings.TrimSpace(s.String)
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			return sql.NullInt64{Int64: t.Unix(), Valid: true}, nil
		}
	}
	return sql.NullInt64{}, fmt.Errorf("unparseable datetime %q", s.String)
}

// mustUnix is unix for NOT NULL columns.
func mustUnix(s sql.NullString) (int64, error) {
	v, err := unix(s)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, fmt.Errorf("unexpected NULL timestamp")
	}
	return v.Int64, nil
}

// --- simple 1:1 table copies

func (m *migrator) users(ctx context.Context) error {
	t, err := m.table(ctx, "users", "users", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx,
		"SELECT id, user_name, password_digest, CAST(created_at AS TEXT), CAST(updated_at AS TEXT) FROM users ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name, digest, created, updated sql.NullString
		if err := rows.Scan(&id, &name, &digest, &created, &updated); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t,
			"INSERT OR IGNORE INTO users (id, user_name, password_digest, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			id, name.String, digest.String, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) tags(ctx context.Context) error {
	t, err := m.table(ctx, "tags", "tags", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx,
		"SELECT id, name, slug, CAST(created_at AS TEXT), CAST(updated_at AS TEXT) FROM tags ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name, slug, created, updated sql.NullString
		if err := rows.Scan(&id, &name, &slug, &created, &updated); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t,
			"INSERT OR IGNORE INTO tags (id, name, slug, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			id, name.String, slug.String, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

// contentRecord is one articles/pages row joined with its rich text body.
type contentRecord struct {
	id          int64
	title, slug sql.NullString
	contentType string
	description sql.NullString
	excerpt     sql.NullString
	htmlContent sql.NullString
	body        sql.NullString // action_text_rich_texts.body

	metaDescription, metaTitle, metaImage  sql.NullString
	sourceAuthor, sourceURL, sourceContent sql.NullString
	status, comment                        int64
	scheduledAt                            sql.NullString
	scheduledCrosspostPlatforms            sql.NullString
	scheduledSendNewsletter                int64
	createdAt, updatedAt                   sql.NullString

	redirectURL sql.NullString // pages only
	pageOrder   int64          // pages only
}

// contentHTML merges html_content / rich text body per spec section 8.3 and
// runs the attachment rewrite of section 8.4.
func (m *migrator) contentHTML(c contentRecord, record string) sql.NullString {
	src := c.body
	if c.contentType == "html" {
		src = c.htmlContent
	}
	if !src.Valid {
		return sql.NullString{}
	}
	return sql.NullString{String: rewriteContent(src.String, m.blobs, record, &m.rep.Rewrite), Valid: true}
}

func (m *migrator) articles(ctx context.Context) error {
	t, err := m.table(ctx, "articles", "articles", 0)
	if err != nil {
		return err
	}
	if err := m.loadBlobs(ctx); err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx, `SELECT a.id, a.title, a.slug, a.content_type, a.description, a.excerpt,
		a.html_content, rt.body, a.meta_description, a.meta_title, a.meta_image,
		a.source_author, a.source_url, a.source_content, a.status, a.comment,
		CAST(a.scheduled_at AS TEXT), a.scheduled_crosspost_platforms, a.scheduled_send_newsletter,
		CAST(a.created_at AS TEXT), CAST(a.updated_at AS TEXT)
		FROM articles a
		LEFT JOIN action_text_rich_texts rt ON rt.record_type = 'Article' AND rt.record_id = a.id AND rt.name = 'content'
		ORDER BY a.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c contentRecord
		if err := rows.Scan(&c.id, &c.title, &c.slug, &c.contentType, &c.description, &c.excerpt,
			&c.htmlContent, &c.body, &c.metaDescription, &c.metaTitle, &c.metaImage,
			&c.sourceAuthor, &c.sourceURL, &c.sourceContent, &c.status, &c.comment,
			&c.scheduledAt, &c.scheduledCrosspostPlatforms, &c.scheduledSendNewsletter,
			&c.createdAt, &c.updatedAt); err != nil {
			return err
		}
		scheduled, err := unix(c.scheduledAt)
		if err != nil {
			return fmt.Errorf("article %d scheduled_at: %w", c.id, err)
		}
		created, err := mustUnix(c.createdAt)
		if err != nil {
			return fmt.Errorf("article %d created_at: %w", c.id, err)
		}
		updated, err := mustUnix(c.updatedAt)
		if err != nil {
			return fmt.Errorf("article %d updated_at: %w", c.id, err)
		}
		platforms := c.scheduledCrosspostPlatforms
		if !platforms.Valid || platforms.String == "" {
			platforms = sql.NullString{String: "[]", Valid: true}
		}
		if err := m.insert(ctx, t, `INSERT OR IGNORE INTO articles
			(id, title, slug, content_html, content_type, description, excerpt,
			 meta_description, meta_title, meta_image, source_author, source_url, source_content,
			 status, comment, scheduled_at, scheduled_crosspost_platforms, scheduled_send_newsletter,
			 created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.id, c.title, c.slug, m.contentHTML(c, fmt.Sprintf("Article/%d", c.id)), c.contentType,
			c.description, c.excerpt, c.metaDescription, c.metaTitle, c.metaImage,
			c.sourceAuthor, c.sourceURL, c.sourceContent,
			c.status, c.comment, scheduled, platforms.String, c.scheduledSendNewsletter,
			created, updated); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) pages(ctx context.Context) error {
	t, err := m.table(ctx, "pages", "pages", 0)
	if err != nil {
		return err
	}
	if err := m.loadBlobs(ctx); err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx, `SELECT p.id, p.title, p.slug, p.content_type, p.html_content, rt.body,
		p.redirect_url, p.page_order, p.status, p.comment,
		CAST(p.scheduled_at AS TEXT), CAST(p.created_at AS TEXT), CAST(p.updated_at AS TEXT)
		FROM pages p
		LEFT JOIN action_text_rich_texts rt ON rt.record_type = 'Page' AND rt.record_id = p.id AND rt.name = 'content'
		ORDER BY p.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c contentRecord
		if err := rows.Scan(&c.id, &c.title, &c.slug, &c.contentType, &c.htmlContent, &c.body,
			&c.redirectURL, &c.pageOrder, &c.status, &c.comment,
			&c.scheduledAt, &c.createdAt, &c.updatedAt); err != nil {
			return err
		}
		scheduled, err := unix(c.scheduledAt)
		if err != nil {
			return fmt.Errorf("page %d scheduled_at: %w", c.id, err)
		}
		created, err := mustUnix(c.createdAt)
		if err != nil {
			return fmt.Errorf("page %d created_at: %w", c.id, err)
		}
		updated, err := mustUnix(c.updatedAt)
		if err != nil {
			return fmt.Errorf("page %d updated_at: %w", c.id, err)
		}
		if err := m.insert(ctx, t, `INSERT OR IGNORE INTO pages
			(id, title, slug, content_html, content_type, redirect_url, page_order,
			 status, comment, scheduled_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.id, c.title, c.slug, m.contentHTML(c, fmt.Sprintf("Page/%d", c.id)), c.contentType,
			c.redirectURL, c.pageOrder, c.status, c.comment, scheduled, created, updated); err != nil {
			return err
		}
	}
	return rows.Err()
}

// comments migrates in two passes (spec section 8.2): insert with
// parent_id NULL, then backfill parents that exist in the new table.
func (m *migrator) comments(ctx context.Context) error {
	t, err := m.table(ctx, "comments", "comments", 0)
	if err != nil {
		return err
	}
	type parentRef struct {
		id, parent int64
	}
	var parents []parentRef
	rows, err := m.old.QueryContext(ctx, `SELECT id, commentable_type, commentable_id, article_id, parent_id,
		author_name, author_email, author_url, author_username, author_avatar_url,
		content, status, platform, external_id, url,
		CAST(published_at AS TEXT), CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM comments ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, status                                        int64
			commentableType, authorName, platform, externalID sql.NullString
			authorEmail, authorURL, authorUsername, avatar    sql.NullString
			content, urlCol                                   sql.NullString
			commentableID, articleID, parentID                sql.NullInt64
			publishedAt, createdAt, updatedAt                 sql.NullString
		)
		if err := rows.Scan(&id, &commentableType, &commentableID, &articleID, &parentID,
			&authorName, &authorEmail, &authorURL, &authorUsername, &avatar,
			&content, &status, &platform, &externalID, &urlCol,
			&publishedAt, &createdAt, &updatedAt); err != nil {
			return err
		}
		published, err := unix(publishedAt)
		if err != nil {
			return fmt.Errorf("comment %d published_at: %w", id, err)
		}
		created, err := mustUnix(createdAt)
		if err != nil {
			return fmt.Errorf("comment %d created_at: %w", id, err)
		}
		updated, err := mustUnix(updatedAt)
		if err != nil {
			return fmt.Errorf("comment %d updated_at: %w", id, err)
		}
		before := t.Inserted
		if err := m.insert(ctx, t, `INSERT OR IGNORE INTO comments
			(id, commentable_type, commentable_id, article_id, parent_id,
			 author_name, author_email, author_url, author_username, author_avatar_url,
			 content, status, platform, external_id, url, published_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, commentableType, commentableID, articleID,
			authorName, authorEmail, authorURL, authorUsername, avatar,
			content, status, platform, externalID, urlCol, published, created, updated); err != nil {
			return err
		}
		if t.Inserted > before && parentID.Valid {
			parents = append(parents, parentRef{id: id, parent: parentID.Int64})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// pass 2: backfill parent_id where the parent made it into the new table
	for _, p := range parents {
		if _, err := m.tx.ExecContext(ctx,
			`UPDATE comments SET parent_id = ?
			 WHERE id = ? AND parent_id IS NULL
			   AND EXISTS (SELECT 1 FROM comments WHERE id = ?)`,
			p.parent, p.id, p.parent); err != nil {
			return err
		}
	}
	return nil
}

func (m *migrator) articleTags(ctx context.Context) error {
	t, err := m.table(ctx, "article_tags", "article_tags", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx,
		"SELECT id, article_id, tag_id, CAST(created_at AS TEXT), CAST(updated_at AS TEXT) FROM article_tags ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, articleID, tagID int64
		var created, updated sql.NullString
		if err := rows.Scan(&id, &articleID, &tagID, &created, &updated); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t,
			"INSERT OR IGNORE INTO article_tags (id, article_id, tag_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			id, articleID, tagID, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

// subscribers keeps the old confirmation/unsubscribe tokens (spec section 8.6).
func (m *migrator) subscribers(ctx context.Context) error {
	t, err := m.table(ctx, "subscribers", "subscribers", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx, `SELECT id, email, confirmation_token, unsubscribe_token,
		CAST(confirmed_at AS TEXT), CAST(unsubscribed_at AS TEXT),
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM subscribers ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var email, confirmToken, unsubToken sql.NullString
		var confirmedAt, unsubAt, created, updated sql.NullString
		if err := rows.Scan(&id, &email, &confirmToken, &unsubToken, &confirmedAt, &unsubAt, &created, &updated); err != nil {
			return err
		}
		confirmed, err := unix(confirmedAt)
		if err != nil {
			return err
		}
		unsub, err := unix(unsubAt)
		if err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t, `INSERT OR IGNORE INTO subscribers
			(id, email, confirmation_token, unsubscribe_token, confirmed_at, unsubscribed_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, email.String, confirmToken, unsubToken, confirmed, unsub, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) subscriberTags(ctx context.Context) error {
	t, err := m.table(ctx, "subscriber_tags", "subscriber_tags", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx,
		"SELECT id, subscriber_id, tag_id, CAST(created_at AS TEXT), CAST(updated_at AS TEXT) FROM subscriber_tags ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, subID, tagID int64
		var created, updated sql.NullString
		if err := rows.Scan(&id, &subID, &tagID, &created, &updated); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t,
			"INSERT OR IGNORE INTO subscriber_tags (id, subscriber_id, tag_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			id, subID, tagID, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) socialMediaPosts(ctx context.Context) error {
	t, err := m.table(ctx, "social_media_posts", "social_media_posts", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx,
		"SELECT id, article_id, platform, url, CAST(created_at AS TEXT), CAST(updated_at AS TEXT) FROM social_media_posts ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, articleID int64
		var platform, urlCol, created, updated sql.NullString
		if err := rows.Scan(&id, &articleID, &platform, &urlCol, &created, &updated); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t,
			"INSERT OR IGNORE INTO social_media_posts (id, article_id, platform, url, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			id, articleID, platform.String, urlCol.String, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) redirects(ctx context.Context) error {
	t, err := m.table(ctx, "redirects", "redirects", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx,
		"SELECT id, regex, replacement, enabled, permanent, CAST(created_at AS TEXT), CAST(updated_at AS TEXT) FROM redirects ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var regex, replacement, created, updated sql.NullString
		var enabled, permanent sql.NullInt64
		if err := rows.Scan(&id, &regex, &replacement, &enabled, &permanent, &created, &updated); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t,
			"INSERT OR IGNORE INTO redirects (id, regex, replacement, enabled, permanent, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			id, regex.String, replacement.String, nullInt(enabled, 1), nullInt(permanent, 0), c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

func nullInt(v sql.NullInt64, def int64) int64 {
	if v.Valid {
		return v.Int64
	}
	return def
}

// --- files / attachments (spec section 8.5): disk bytes are never copied.

// loadBlobs reads all active_storage_blobs rows once for content rewriting.
func (m *migrator) loadBlobs(ctx context.Context) error {
	if m.blobs != nil {
		return nil
	}
	m.blobs = map[int64]blobRef{}
	rows, err := m.old.QueryContext(ctx, "SELECT id, key, filename, content_type FROM active_storage_blobs")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var key, filename, contentType sql.NullString
		if err := rows.Scan(&id, &key, &filename, &contentType); err != nil {
			return err
		}
		m.blobs[id] = blobRef{key: key.String, filename: filename.String, contentType: contentType.String}
	}
	return rows.Err()
}

func (m *migrator) files(ctx context.Context) error {
	t, err := m.table(ctx, "files", "active_storage_blobs", 0)
	if err != nil {
		return err
	}
	if err := m.loadBlobs(ctx); err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx,
		"SELECT id, key, filename, content_type, byte_size, checksum, CAST(created_at AS TEXT) FROM active_storage_blobs ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, byteSize int64
		var key, filename, contentType, checksum, created sql.NullString
		if err := rows.Scan(&id, &key, &filename, &contentType, &byteSize, &checksum, &created); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t,
			"INSERT OR IGNORE INTO files (id, key, filename, content_type, byte_size, checksum, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			id, key.String, filename.String, contentType, byteSize, checksum, c); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// variants: the variant record's image attachment points at the variant
	// blob; files.variant_of links it to the original blob.
	var linked int64
	vrows, err := m.old.QueryContext(ctx, `SELECT vr.blob_id, att.blob_id
		FROM active_storage_variant_records vr
		JOIN active_storage_attachments att
		  ON att.record_type = 'ActiveStorage::VariantRecord' AND att.record_id = vr.id AND att.name = 'image'
		ORDER BY vr.id`)
	if err != nil {
		return err
	}
	defer vrows.Close()
	for vrows.Next() {
		var origID, variantID int64
		if err := vrows.Scan(&origID, &variantID); err != nil {
			return err
		}
		res, err := m.tx.ExecContext(ctx,
			"UPDATE files SET variant_of = ? WHERE id = ? AND variant_of IS NULL", origID, variantID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			linked += n
		}
	}
	if err := vrows.Err(); err != nil {
		return err
	}
	var total, dangling int64
	if err := m.old.QueryRowContext(ctx, "SELECT COUNT(*) FROM active_storage_variant_records").Scan(&total); err != nil {
		return err
	}
	if err := m.old.QueryRowContext(ctx, `SELECT COUNT(*) FROM active_storage_variant_records vr
		LEFT JOIN active_storage_attachments att
		  ON att.record_type = 'ActiveStorage::VariantRecord' AND att.record_id = vr.id AND att.name = 'image'
		WHERE att.id IS NULL`).Scan(&dangling); err != nil {
		return err
	}
	m.rep.Notes = append(m.rep.Notes,
		fmt.Sprintf("variants: %d linked via files.variant_of (%d old variant records, %d without image attachment)",
			linked, total, dangling))
	return nil
}

// attachments remaps ActionText::RichText rows onto their owner record
// (Article/Page), folds ActiveStorage::VariantRecord rows into
// files.variant_of and StaticFile rows into static_files.file_id (both
// counted as transformed), and carries everything else over unchanged.
func (m *migrator) attachments(ctx context.Context) error {
	var variantCount, staticFileCount int64
	if err := m.old.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM active_storage_attachments WHERE record_type = 'ActiveStorage::VariantRecord'").Scan(&variantCount); err != nil {
		return err
	}
	if err := m.old.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM active_storage_attachments WHERE record_type = 'StaticFile'").Scan(&staticFileCount); err != nil {
		return err
	}
	if err := m.loadRichTexts(ctx); err != nil {
		return err
	}
	// rich text attachments whose owner row vanished cannot be remapped
	var unmapped int64
	if err := m.old.QueryRowContext(ctx, `SELECT COUNT(*) FROM active_storage_attachments a
		LEFT JOIN action_text_rich_texts rt ON rt.id = a.record_id
		WHERE a.record_type = 'ActionText::RichText' AND rt.id IS NULL`).Scan(&unmapped); err != nil {
		return err
	}
	t, err := m.table(ctx, "attachments", "active_storage_attachments", variantCount+staticFileCount+unmapped)
	if err != nil {
		return err
	}
	if unmapped > 0 {
		m.rep.Notes = append(m.rep.Notes,
			fmt.Sprintf("%d ActionText::RichText attachments had no rich text row and were dropped", unmapped))
	}
	rows, err := m.old.QueryContext(ctx, `SELECT id, blob_id, record_type, record_id, name, CAST(created_at AS TEXT)
		FROM active_storage_attachments ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, blobID, recordID int64
		var recordType, name, created sql.NullString
		if err := rows.Scan(&id, &blobID, &recordType, &recordID, &name, &created); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		rt, rn := recordType.String, recordID
		switch recordType.String {
		case "ActiveStorage::VariantRecord", "StaticFile":
			continue // folded into files.variant_of / static_files.file_id
		case "ActionText::RichText":
			owner, ok := m.richTexts[recordID]
			if !ok {
				continue
			}
			rt, rn = owner.typ, owner.id
		}
		if err := m.insert(ctx, t,
			"INSERT OR IGNORE INTO attachments (id, file_id, record_type, record_id, name, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			id, blobID, rt, rn, name.String, c); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) loadRichTexts(ctx context.Context) error {
	if m.richTexts != nil {
		return nil
	}
	m.richTexts = map[int64]struct {
		typ string
		id  int64
	}{}
	rows, err := m.old.QueryContext(ctx, "SELECT id, record_type, record_id FROM action_text_rich_texts")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, recordID int64
		var recordType string
		if err := rows.Scan(&id, &recordType, &recordID); err != nil {
			return err
		}
		m.richTexts[id] = struct {
			typ string
			id  int64
		}{recordType, recordID}
	}
	return rows.Err()
}

// staticFiles is not in the spec section 8.2 table list, but the Go schema
// keeps the feature (static_files.file_id); carried over via the old
// StaticFile attachment join. Spec gap recorded in the task report.
func (m *migrator) staticFiles(ctx context.Context) error {
	var noBlob int64
	if err := m.old.QueryRowContext(ctx, `SELECT COUNT(*) FROM static_files sf
		LEFT JOIN active_storage_attachments a
		  ON a.record_type = 'StaticFile' AND a.record_id = sf.id AND a.name = 'file'
		WHERE a.id IS NULL`).Scan(&noBlob); err != nil {
		return err
	}
	t, err := m.table(ctx, "static_files", "static_files", noBlob)
	if err != nil {
		return err
	}
	if noBlob > 0 {
		m.rep.Notes = append(m.rep.Notes,
			fmt.Sprintf("%d static_files rows had no file attachment and were dropped", noBlob))
	}
	rows, err := m.old.QueryContext(ctx, `SELECT sf.id, sf.filename, sf.description, a.blob_id,
		CAST(sf.created_at AS TEXT), CAST(sf.updated_at AS TEXT)
		FROM static_files sf
		JOIN active_storage_attachments a
		  ON a.record_type = 'StaticFile' AND a.record_id = sf.id AND a.name = 'file'
		ORDER BY sf.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, blobID int64
		var filename, description, created, updated sql.NullString
		if err := rows.Scan(&id, &filename, &description, &blobID, &created, &updated); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t,
			"INSERT OR IGNORE INTO static_files (id, filename, description, file_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			id, filename.String, description, blobID, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

// --- singleton config tables: INSERT OR IGNORE (skip if the Go app already
// created the row), so a re-run stays idempotent.

func (m *migrator) settings(ctx context.Context) error {
	t, err := m.table(ctx, "settings", "settings", 0)
	if err != nil {
		return err
	}
	// only the fields kept in the new schema (spec section 8.6); legacy
	// static-generation/GitHub-backup columns are intentionally not read.
	row := m.old.QueryRowContext(ctx, `SELECT title, description, author, url, time_zone,
		head_code, custom_css, tool_code, giscus, social_links, setup_completed,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM settings ORDER BY id LIMIT 1`)
	var title, description, author, urlCol, timeZone sql.NullString
	var headCode, customCSS, toolCode, giscus, socialLinks sql.NullString
	var setupCompleted sql.NullInt64
	var created, updated sql.NullString
	if err := row.Scan(&title, &description, &author, &urlCol, &timeZone,
		&headCode, &customCSS, &toolCode, &giscus, &socialLinks, &setupCompleted,
		&created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	c, err := mustUnix(created)
	if err != nil {
		return err
	}
	u, err := mustUnix(updated)
	if err != nil {
		return err
	}
	if !timeZone.Valid || timeZone.String == "" {
		timeZone = sql.NullString{String: "UTC", Valid: true}
	}
	return m.insertSingleton(ctx, t, `INSERT OR IGNORE INTO settings
		(id, title, description, author, url, time_zone, head_code, custom_css, tool_code, giscus,
		 social_links, setup_completed, created_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		title, description, author, urlCol, timeZone, headCode, customCSS, toolCode, giscus,
		socialLinks, nullInt(setupCompleted, 0), c, u)
}

func (m *migrator) newsletterSettings(ctx context.Context) error {
	t, err := m.table(ctx, "newsletter_settings", "newsletter_settings", 0)
	if err != nil {
		return err
	}
	row := m.old.QueryRowContext(ctx, `SELECT enabled, provider, from_email,
		smtp_address, smtp_port, smtp_user_name, smtp_password, smtp_domain,
		smtp_authentication, smtp_enable_starttls,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM newsletter_settings ORDER BY id LIMIT 1`)
	var enabled sql.NullInt64
	var provider, fromEmail, smtpAddress, smtpUser, smtpPassword, smtpDomain, smtpAuth sql.NullString
	var smtpPort, smtpStarttls sql.NullInt64
	var created, updated sql.NullString
	if err := row.Scan(&enabled, &provider, &fromEmail,
		&smtpAddress, &smtpPort, &smtpUser, &smtpPassword, &smtpDomain,
		&smtpAuth, &smtpStarttls, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	c, err := mustUnix(created)
	if err != nil {
		return err
	}
	u, err := mustUnix(updated)
	if err != nil {
		return err
	}
	if !provider.Valid || provider.String == "" {
		provider = sql.NullString{String: "native", Valid: true}
	}
	return m.insertSingleton(ctx, t, `INSERT OR IGNORE INTO newsletter_settings
		(id, enabled, provider, from_email, smtp_address, smtp_port, smtp_user_name, smtp_password,
		 smtp_domain, smtp_authentication, smtp_enable_starttls, created_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullInt(enabled, 0), provider, fromEmail, smtpAddress, smtpPort, smtpUser, smtpPassword,
		smtpDomain, smtpAuth, smtpStarttls, c, u)
}

func (m *migrator) crossposts(ctx context.Context) error {
	t, err := m.table(ctx, "crossposts", "crossposts", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx, `SELECT id, platform, enabled,
		api_key, api_key_secret, access_token, access_token_secret,
		client_id, client_key, client_secret, app_password, refresh_token,
		CAST(token_expires_at AS TEXT), server_url, username, max_characters,
		auto_fetch_comments, comment_fetch_schedule, settings,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM crossposts ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var platform, apiKey, apiKeySecret, accessToken, accessTokenSecret sql.NullString
		var clientID, clientKey, clientSecret, appPassword, refreshToken sql.NullString
		var tokenExpiresAt, serverURL, username, commentFetchSchedule, settingsJSON sql.NullString
		var enabled, autoFetch sql.NullInt64
		var maxCharacters sql.NullInt64
		var created, updated sql.NullString
		if err := rows.Scan(&id, &platform, &enabled,
			&apiKey, &apiKeySecret, &accessToken, &accessTokenSecret,
			&clientID, &clientKey, &clientSecret, &appPassword, &refreshToken,
			&tokenExpiresAt, &serverURL, &username, &maxCharacters,
			&autoFetch, &commentFetchSchedule, &settingsJSON, &created, &updated); err != nil {
			return err
		}
		expires, err := unix(tokenExpiresAt)
		if err != nil {
			return fmt.Errorf("crosspost %d token_expires_at: %w", id, err)
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t, `INSERT OR IGNORE INTO crossposts
			(id, platform, enabled, api_key, api_key_secret, access_token, access_token_secret,
			 client_id, client_key, client_secret, app_password, refresh_token, token_expires_at,
			 server_url, username, max_characters, auto_fetch_comments, comment_fetch_schedule,
			 settings, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, platform.String, nullInt(enabled, 0), apiKey, apiKeySecret, accessToken, accessTokenSecret,
			clientID, clientKey, clientSecret, appPassword, refreshToken, expires,
			serverURL, username, maxCharacters, nullInt(autoFetch, 0), commentFetchSchedule,
			settingsJSON, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) listmonks(ctx context.Context) error {
	t, err := m.table(ctx, "listmonks", "listmonks", 0)
	if err != nil {
		return err
	}
	row := m.old.QueryRowContext(ctx, `SELECT url, username, api_key, list_id, template_id, enabled,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM listmonks ORDER BY id LIMIT 1`)
	var urlCol, username, apiKey sql.NullString
	var listID, templateID, enabled sql.NullInt64
	var created, updated sql.NullString
	if err := row.Scan(&urlCol, &username, &apiKey, &listID, &templateID, &enabled, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	c, err := mustUnix(created)
	if err != nil {
		return err
	}
	u, err := mustUnix(updated)
	if err != nil {
		return err
	}
	return m.insertSingleton(ctx, t, `INSERT OR IGNORE INTO listmonks
		(id, url, username, api_key, list_id, template_id, enabled, created_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)`,
		urlCol, username, apiKey, listID, templateID, nullInt(enabled, 0), c, u)
}

func (m *migrator) twitterSyncs(ctx context.Context) error {
	t, err := m.table(ctx, "twitter_syncs", "twitter_syncs", 0)
	if err != nil {
		return err
	}
	row := m.old.QueryRowContext(ctx, `SELECT enabled, username, user_id, since_id, start_date,
		sync_schedule, CAST(last_synced_at AS TEXT), last_error,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM twitter_syncs ORDER BY id LIMIT 1`)
	var enabled sql.NullInt64
	var username, userID, sinceID, startDate, syncSchedule, lastSyncedAt, lastError sql.NullString
	var created, updated sql.NullString
	if err := row.Scan(&enabled, &username, &userID, &sinceID, &startDate,
		&syncSchedule, &lastSyncedAt, &lastError, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	lastSynced, err := unix(lastSyncedAt)
	if err != nil {
		return err
	}
	c, err := mustUnix(created)
	if err != nil {
		return err
	}
	u, err := mustUnix(updated)
	if err != nil {
		return err
	}
	if !syncSchedule.Valid || syncSchedule.String == "" {
		syncSchedule = sql.NullString{String: "every_15_minutes", Valid: true}
	}
	return m.insertSingleton(ctx, t, `INSERT OR IGNORE INTO twitter_syncs
		(id, enabled, username, user_id, since_id, start_date, sync_schedule, last_synced_at, last_error,
		 created_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullInt(enabled, 0), username, userID, sinceID, startDate, syncSchedule, lastSynced, lastError, c, u)
}

func (m *migrator) twitterArchiveTweets(ctx context.Context) error {
	t, err := m.table(ctx, "twitter_archive_tweets", "twitter_archive_tweets", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx, `SELECT id, tweet_id, screen_name, full_text, entry_type,
		CAST(tweeted_at AS TEXT), CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM twitter_archive_tweets ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var tweetID, screenName, fullText, entryType, tweetedAt, created, updated sql.NullString
		if err := rows.Scan(&id, &tweetID, &screenName, &fullText, &entryType, &tweetedAt, &created, &updated); err != nil {
			return err
		}
		tw, err := mustUnix(tweetedAt)
		if err != nil {
			return fmt.Errorf("archive tweet %d tweeted_at: %w", id, err)
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t, `INSERT OR IGNORE INTO twitter_archive_tweets
			(id, tweet_id, screen_name, full_text, entry_type, tweeted_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, tweetID.String, screenName.String, fullText.String, entryType.String, tw, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) twitterArchiveConnections(ctx context.Context) error {
	t, err := m.table(ctx, "twitter_archive_connections", "twitter_archive_connections", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx, `SELECT id, account_id, screen_name, user_link, relationship_type,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM twitter_archive_connections ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var accountID, screenName, userLink, relType, created, updated sql.NullString
		if err := rows.Scan(&id, &accountID, &screenName, &userLink, &relType, &created, &updated); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t, `INSERT OR IGNORE INTO twitter_archive_connections
			(id, account_id, screen_name, user_link, relationship_type, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, accountID.String, screenName, userLink, relType.String, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) twitterArchiveLikes(ctx context.Context) error {
	t, err := m.table(ctx, "twitter_archive_likes", "twitter_archive_likes", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx, `SELECT id, tweet_id, full_text, expanded_url,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM twitter_archive_likes ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var tweetID, fullText, expandedURL, created, updated sql.NullString
		if err := rows.Scan(&id, &tweetID, &fullText, &expandedURL, &created, &updated); err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t, `INSERT OR IGNORE INTO twitter_archive_likes
			(id, tweet_id, full_text, expanded_url, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			id, tweetID.String, fullText, expandedURL, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *migrator) twitterArchiveImports(ctx context.Context) error {
	t, err := m.table(ctx, "twitter_archive_imports", "twitter_archive_imports", 0)
	if err != nil {
		return err
	}
	rows, err := m.old.QueryContext(ctx, `SELECT id, status, progress, total_items_count,
		tweets_count, followers_count, following_count, likes_count,
		source_filename, source_path, status_message, error_message,
		CAST(queued_at AS TEXT), CAST(started_at AS TEXT), CAST(finished_at AS TEXT), active_slot,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT)
		FROM twitter_archive_imports ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, progress, totalItems, tweets, followers, following, likes int64
		var status, sourceFilename, sourcePath, statusMessage, errorMessage sql.NullString
		var queuedAt, startedAt, finishedAt sql.NullString
		var activeSlot sql.NullInt64
		var created, updated sql.NullString
		if err := rows.Scan(&id, &status, &progress, &totalItems, &tweets, &followers, &following, &likes,
			&sourceFilename, &sourcePath, &statusMessage, &errorMessage,
			&queuedAt, &startedAt, &finishedAt, &activeSlot, &created, &updated); err != nil {
			return err
		}
		queued, err := mustUnix(queuedAt)
		if err != nil {
			return fmt.Errorf("archive import %d queued_at: %w", id, err)
		}
		started, err := unix(startedAt)
		if err != nil {
			return err
		}
		finished, err := unix(finishedAt)
		if err != nil {
			return err
		}
		c, err := mustUnix(created)
		if err != nil {
			return err
		}
		u, err := mustUnix(updated)
		if err != nil {
			return err
		}
		if err := m.insert(ctx, t, `INSERT OR IGNORE INTO twitter_archive_imports
			(id, status, progress, total_items_count, tweets_count, followers_count, following_count,
			 likes_count, source_filename, source_path, status_message, error_message,
			 queued_at, started_at, finished_at, active_slot, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, status.String, progress, totalItems, tweets, followers, following, likes,
			sourceFilename.String, sourcePath, statusMessage, errorMessage,
			queued, started, finished, activeSlot, c, u); err != nil {
			return err
		}
	}
	return rows.Err()
}
