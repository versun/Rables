// ZIP import (plan T26, section 4.11): consumes the canonical ZIP layout
// produced by ZipExporter (see export_zip.go for the fixed CSV columns).
// Mirrors ImportZip (app/models/import_zip.rb): a single all-or-nothing
// transaction, per-row dedupe skips by slug/email/regex/(platform,external_id),
// two-pass comment import with parent_id backfill, [REDACTED] credentials
// never overwriting stored values, overdue schedule articles losing their
// crosspost/newsletter snapshots, and traversal-safe extraction.
//
// Deviations from the Rails importer (it consumes the old Rails export
// format; this importer consumes the Go format):
//   - No ActionText content rewriting (sgid/active_storage URL fixes): Go
//     content_html already references /files/<key> and files are restored by
//     key. Rails-export zips are not supported (T27 migrates old databases).
//   - Record ids are reassigned; associations resolve via the helper columns
//     (article_slug, tag_slug, subscriber_email) exactly like the Rails
//     importer, and old->new id maps rebuild comments.parent_id,
//     files.variant_of, attachments and static_files references.
//   - static_files import runs before attachments (the task order lists them
//     the other way) so StaticFile attachments can remap record_id through
//     the new ids; the foreign-key dependency order is preserved either way.
//   - Rails validation-level aborts (blank tag name, invalid redirect regex,
//     ...) become plain inserts or row-level skips; DB constraints still
//     apply. A redacted listmonk api_key imports as NULL instead of aborting
//     (Rails raises on Listmonk's presence validation).
package transfer

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/media"
	"rables/internal/service/subscribers"
)

// ImportZipPayload is the job_runs payload for kind "import_zip".
type ImportZipPayload struct {
	// Path is the uploaded zip, usually <DataDir>/imports/import_*.zip.
	Path string `json:"path"`
}

// ZipImporter imports a ZipExporter bundle (ImportZip).
type ZipImporter struct {
	DB      *sql.DB
	DataDir string
	// Now overrides the clock (overdue schedule check); nil uses time.Now.
	Now func() time.Time
}

// ImportResult counts per-table outcomes of one import run.
type ImportResult struct {
	Imported map[string]int
	Updated  map[string]int
	Skipped  map[string]int
}

func newImportResult() *ImportResult {
	return &ImportResult{Imported: map[string]int{}, Updated: map[string]int{}, Skipped: map[string]int{}}
}

// Import runs the whole import and returns per-table counts. Any hard error
// rolls the transaction back, leaving the database untouched.
func (z *ZipImporter) Import(ctx context.Context, zipPath string) (*ImportResult, error) {
	stage, err := importStagingDir(z.DataDir)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	if err := extractImportZip(zipPath, stage); err != nil {
		return nil, err
	}

	now := time.Now()
	if z.Now != nil {
		now = z.Now()
	}
	imp := &zipImport{
		dataDir: z.DataDir,
		base:    csvBaseDir(stage),
		now:     now.Unix(),
		result:  newImportResult(),

		articleSlugs:    map[string]int64{},
		tagSlugs:        map[string]int64{},
		subscriberMails: map[string]int64{},
		articleIDs:      map[int64]int64{},
		pageIDs:         map[int64]int64{},
		fileIDs:         map[int64]int64{},
		fileKeys:        map[int64]string{},
		staticFileIDs:   map[int64]int64{},
		commentIDs:      map[int64]int64{},
	}

	tx, err := z.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("import zip: begin transaction: %w", err)
	}
	imp.q = query.New(tx)

	// Fixed order, following the foreign-key dependencies.
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"tags", imp.importTags},
		{"articles", imp.importArticles},
		{"article_tags", imp.importArticleTags},
		{"pages", imp.importPages},
		{"comments", imp.importComments},
		{"subscribers", imp.importSubscribers},
		{"subscriber_tags", imp.importSubscriberTags},
		{"settings", imp.importSettings},
		{"crossposts", imp.importCrossposts},
		{"listmonks", imp.importListmonks},
		{"newsletter_settings", imp.importNewsletterSettings},
		{"social_media_posts", imp.importSocialMediaPosts},
		{"redirects", imp.importRedirects},
		{"files", imp.importFiles},
		{"static_files", imp.importStaticFiles},
		{"attachments", imp.importAttachments},
	}
	for _, step := range steps {
		if err := step.fn(ctx); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("import zip: %s: %w", step.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("import zip: commit: %w", err)
	}
	z.restoreBlobs(ctx, imp)
	return imp.result, nil
}

// zipImport carries the state shared by the per-table passes.
type zipImport struct {
	q       *query.Queries
	dataDir string
	base    string // directory holding the CSVs inside the staging dir
	now     int64
	result  *ImportResult

	articleSlugs    map[string]int64 // slug -> id, 0 = known missing
	tagSlugs        map[string]int64
	subscriberMails map[string]int64
	articleIDs      map[int64]int64 // CSV id -> new id
	pageIDs         map[int64]int64
	fileIDs         map[int64]int64
	fileKeys        map[int64]string
	staticFileIDs   map[int64]int64
	commentIDs      map[int64]int64

	restores []blobRestore
}

// blobRestore is one staged attachments/files/<id>_<filename> blob to move
// into the media layout after the transaction commits.
type blobRestore struct {
	staged        string
	key           string
	onlyIfMissing bool // pre-existing key: never overwrite a blob on disk
}

func (imp *zipImport) imported(table string) { imp.result.Imported[table]++ }
func (imp *zipImport) updated(table string)  { imp.result.Updated[table]++ }
func (imp *zipImport) skipped(table string)  { imp.result.Skipped[table]++ }

// importStagingDir creates <dataDir>/imports/extract_<ts>_<pid>_<rand>.
func importStagingDir(dataDir string) (string, error) {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	name := fmt.Sprintf("extract_%s_%d_%s", time.Now().UTC().Format("20060102_150405"), os.Getpid(), hex.EncodeToString(rnd[:]))
	dir := filepath.Join(dataDir, "imports", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("import zip: create staging dir: %w", err)
	}
	return dir, nil
}

// extractImportZip unpacks every regular file entry into stage, rejecting
// entries whose path would escape it (ImportZip#extract_zip_file +
// safe_file_path?). Any unsafe entry aborts the whole import.
func extractImportZip(zipPath, stage string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("import zip: open: %w", err)
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
			return fmt.Errorf("import zip: unsafe path in ZIP entry: %s", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("import zip: extract %s: %w", f.Name, err)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("import zip: extract %s: %w", f.Name, err)
		}
		out, err := os.Create(target)
		if err == nil {
			_, err = io.Copy(out, rc)
		}
		rc.Close()
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("import zip: extract %s: %w", f.Name, err)
		}
	}
	return nil
}

// safeZipEntryName cleans a zip entry name and rejects anything that could
// escape the staging directory: absolute paths, drive letters, NUL bytes and
// ".." segments (path traversal entries are refused, not sanitized).
func safeZipEntryName(name string) (string, error) {
	unsafe := fmt.Errorf("import zip: unsafe path in ZIP entry: %s", name)
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

// csvBaseDir mirrors ImportZip#find_csv_base_dir: CSVs at the staging root
// win, otherwise the first subdirectory containing CSVs is used (zips with a
// wrapper directory).
func csvBaseDir(stage string) string {
	if matches, _ := filepath.Glob(filepath.Join(stage, "*.csv")); len(matches) > 0 {
		return stage
	}
	if matches, _ := filepath.Glob(filepath.Join(stage, "*", "*.csv")); len(matches) > 0 {
		return filepath.Dir(matches[0])
	}
	return stage
}

// csvTable is one parsed CSV file: column index plus rows.
type csvTable struct {
	idx  map[string]int
	rows [][]string
}

// readCSVTable loads <base>/<name>; a missing file yields (nil, nil), which
// the per-table passes treat as "nothing to import".
func readCSVTable(base, name string) (*csvTable, error) {
	f, err := os.Open(filepath.Join(base, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // tolerate short/long rows; missing fields read as ""
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	t := &csvTable{idx: map[string]int{}, rows: records[1:]}
	for i, h := range records[0] {
		t.idx[h] = i
	}
	return t, nil
}

// col returns the row's value for name ("" when the column or field is
// missing, matching Ruby CSV's nil).
func (t *csvTable) col(row []string, name string) string {
	i, ok := t.idx[name]
	if !ok || i >= len(row) {
		return ""
	}
	return row[i]
}

// CSV value parsers. Empty cells map to NULL/false; unparseable integers map
// to NULL (ActiveRecord typecasts garbage to nil).

func csvNullInt(s string) sql.NullInt64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

func csvIntOr(s string, def int64) int64 {
	if v := csvNullInt(s); v.Valid {
		return v.Int64
	}
	return def
}

// csvBool mirrors ActiveModel::Type::Boolean with default false (TRUE_VALUES).
func csvBool(s string) int64 {
	switch s {
	case "1", "t", "T", "true", "TRUE", "on", "ON":
		return 1
	}
	return 0
}

func csvNullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// csvTimeNow parses a NOT NULL timestamp column, falling back to the import
// time (Rails record_timestamps fill nil created_at/updated_at).
func csvTimeNow(s string, now int64) int64 { return csvIntOr(s, now) }

// resolveArticleID finds an article by slug, consulting rows already imported
// first; the result (including "missing") is cached.
func (imp *zipImport) resolveArticleID(ctx context.Context, slug string) (int64, error) {
	if id, ok := imp.articleSlugs[slug]; ok {
		return id, nil
	}
	id, err := imp.q.ImportArticleIDBySlug(ctx, csvNullStr(slug))
	if errors.Is(err, sql.ErrNoRows) {
		imp.articleSlugs[slug] = 0
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	imp.articleSlugs[slug] = id
	return id, nil
}

func (imp *zipImport) resolveTagID(ctx context.Context, slug string) (int64, error) {
	if id, ok := imp.tagSlugs[slug]; ok {
		return id, nil
	}
	id, err := imp.q.ImportTagIDBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		imp.tagSlugs[slug] = 0
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	imp.tagSlugs[slug] = id
	return id, nil
}

func (imp *zipImport) resolveSubscriberID(ctx context.Context, email string) (int64, error) {
	if id, ok := imp.subscriberMails[email]; ok {
		return id, nil
	}
	id, err := imp.q.ImportSubscriberIDByEmail(ctx, email)
	if errors.Is(err, sql.ErrNoRows) {
		imp.subscriberMails[email] = 0
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	imp.subscriberMails[email] = id
	return id, nil
}

// importTags mirrors import_tags: skip slugs that already exist.
func (imp *zipImport) importTags(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "tags.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		slug := table.col(row, "slug")
		if id, err := imp.resolveTagID(ctx, slug); err != nil {
			return err
		} else if id != 0 {
			imp.skipped("tags")
			continue
		}
		newID, err := imp.q.ImportInsertTag(ctx, query.ImportInsertTagParams{
			Name:      table.col(row, "name"),
			Slug:      slug,
			CreatedAt: csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt: csvTimeNow(table.col(row, "updated_at"), imp.now),
		})
		if err != nil {
			return err
		}
		imp.tagSlugs[slug] = newID
		imp.imported("tags")
	}
	return nil
}

// importArticles mirrors import_articles: skip existing slugs, invalid
// statuses and blank content; overdue schedule rows lose their snapshots.
func (imp *zipImport) importArticles(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "articles.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		csvID := csvNullInt(table.col(row, "id"))
		slug := table.col(row, "slug")
		if id, err := imp.resolveArticleID(ctx, slug); err != nil {
			return err
		} else if id != 0 {
			if csvID.Valid {
				imp.articleIDs[csvID.Int64] = id
			}
			imp.skipped("articles")
			continue
		}

		status, statusOK := parseStatus(table.col(row, "status"))
		if !statusOK {
			imp.skipped("articles") // invalid_status
			continue
		}
		content := table.col(row, "content_html")
		if strings.TrimSpace(content) == "" {
			imp.skipped("articles") // content_blank
			continue
		}
		title := table.col(row, "title")
		if slug == "" {
			// Article#generate_slug fills a blank slug from the title; a
			// collision or a reserved slug aborts like the Rails validation.
			slug = domain.GenerateSlug("", title, time.Unix(imp.now, 0), nil)
			if domain.IsReservedSlug(slug) {
				return fmt.Errorf("slug %q is reserved", slug)
			}
			if id, err := imp.resolveArticleID(ctx, slug); err != nil {
				return err
			} else if id != 0 {
				return fmt.Errorf("generated slug %q already exists", slug)
			}
		}

		scheduledAt := csvNullInt(table.col(row, "scheduled_at"))
		platforms := table.col(row, "scheduled_crosspost_platforms")
		if platforms == "" {
			platforms = "[]"
		}
		sendNewsletter := csvBool(table.col(row, "scheduled_send_newsletter"))
		// Overdue schedule articles lose crosspost/newsletter snapshots
		// (overdue_scheduled_article?).
		if status == int64(domain.StatusSchedule) && scheduledAt.Valid && scheduledAt.Int64 <= imp.now {
			platforms = "[]"
			sendNewsletter = 0
		}
		contentType := table.col(row, "content_type")
		if contentType == "" {
			contentType = string(domain.ContentTypeRichText)
		}

		newID, err := imp.q.ImportInsertArticle(ctx, query.ImportInsertArticleParams{
			Title:                       csvNullStr(title),
			Slug:                        csvNullStr(slug),
			ContentHtml:                 csvNullStr(content),
			ContentType:                 contentType,
			Description:                 csvNullStr(table.col(row, "description")),
			Excerpt:                     csvNullStr(table.col(row, "excerpt")),
			MetaDescription:             csvNullStr(table.col(row, "meta_description")),
			MetaTitle:                   csvNullStr(table.col(row, "meta_title")),
			MetaImage:                   csvNullStr(table.col(row, "meta_image")),
			SourceAuthor:                csvNullStr(table.col(row, "source_author")),
			SourceUrl:                   csvNullStr(table.col(row, "source_url")),
			SourceContent:               csvNullStr(table.col(row, "source_content")),
			Status:                      status,
			Comment:                     csvBool(table.col(row, "comment")),
			ScheduledAt:                 scheduledAt,
			ScheduledCrosspostPlatforms: platforms,
			ScheduledSendNewsletter:     sendNewsletter,
			CreatedAt:                   csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt:                   csvTimeNow(table.col(row, "updated_at"), imp.now),
		})
		if err != nil {
			return err
		}
		imp.articleSlugs[slug] = newID
		if csvID.Valid {
			imp.articleIDs[csvID.Int64] = newID
		}
		imp.imported("articles")
	}
	return nil
}

// parseStatus validates the integer status against the 5-state enum (the Go
// CSV stores the enum value, not the Rails name).
func parseStatus(s string) (int64, bool) {
	v := csvNullInt(s)
	if !v.Valid || v.Int64 < int64(domain.StatusDraft) || v.Int64 > int64(domain.StatusShared) {
		return 0, false
	}
	return v.Int64, true
}

// importArticleTags mirrors import_article_tags: resolve both ends via the
// slug helper columns and skip existing pairs.
func (imp *zipImport) importArticleTags(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "article_tags.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		articleSlug := table.col(row, "article_slug")
		if articleSlug == "" {
			imp.skipped("article_tags")
			continue
		}
		articleID, err := imp.resolveArticleID(ctx, articleSlug)
		if err != nil {
			return err
		}
		if articleID == 0 {
			imp.skipped("article_tags") // article_not_found
			continue
		}
		tagSlug := table.col(row, "tag_slug")
		if tagSlug == "" {
			imp.skipped("article_tags")
			continue
		}
		tagID, err := imp.resolveTagID(ctx, tagSlug)
		if err != nil {
			return err
		}
		if tagID == 0 {
			imp.skipped("article_tags") // tag_not_found
			continue
		}
		if _, err := imp.q.ImportArticleTagID(ctx, query.ImportArticleTagIDParams{ArticleID: articleID, TagID: tagID}); err == nil {
			imp.skipped("article_tags") // already_exists
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := imp.q.ImportInsertArticleTag(ctx, query.ImportInsertArticleTagParams{
			ArticleID: articleID,
			TagID:     tagID,
			CreatedAt: csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt: csvTimeNow(table.col(row, "updated_at"), imp.now),
		}); err != nil {
			return err
		}
		imp.imported("article_tags")
	}
	return nil
}

// importPages mirrors import_pages; blank slugs are skipped (Pages have no
// generate_slug fallback in Rails).
func (imp *zipImport) importPages(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "pages.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		csvID := csvNullInt(table.col(row, "id"))
		slug := table.col(row, "slug")
		if slug == "" {
			imp.skipped("pages")
			continue
		}
		if _, err := imp.q.ImportPageIDBySlug(ctx, csvNullStr(slug)); err == nil {
			imp.skipped("pages") // already_exists
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		status, statusOK := parseStatus(table.col(row, "status"))
		if !statusOK {
			imp.skipped("pages") // invalid_status
			continue
		}
		content := table.col(row, "content_html")
		if strings.TrimSpace(content) == "" {
			imp.skipped("pages") // content_blank
			continue
		}
		contentType := table.col(row, "content_type")
		if contentType == "" {
			contentType = string(domain.ContentTypeRichText)
		}
		newID, err := imp.q.ImportInsertPage(ctx, query.ImportInsertPageParams{
			Title:       csvNullStr(table.col(row, "title")),
			Slug:        csvNullStr(slug),
			ContentHtml: csvNullStr(content),
			ContentType: contentType,
			RedirectUrl: csvNullStr(table.col(row, "redirect_url")),
			PageOrder:   csvIntOr(table.col(row, "page_order"), 0),
			Status:      status,
			Comment:     csvBool(table.col(row, "comment")),
			ScheduledAt: csvNullInt(table.col(row, "scheduled_at")),
			CreatedAt:   csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt:   csvTimeNow(table.col(row, "updated_at"), imp.now),
		})
		if err != nil {
			return err
		}
		if csvID.Valid {
			imp.pageIDs[csvID.Int64] = newID
		}
		imp.imported("pages")
	}
	return nil
}

// importComments mirrors import_comments: first pass inserts every comment
// with parent_id NULL, the second pass backfills parent_id through the
// old->new id map. Duplicates still feed the map so their children re-link.
func (imp *zipImport) importComments(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "comments.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		csvID := csvNullInt(table.col(row, "id"))
		articleSlug := table.col(row, "article_slug")
		if articleSlug == "" {
			imp.skipped("comments")
			continue
		}
		articleID, err := imp.resolveArticleID(ctx, articleSlug)
		if err != nil {
			return err
		}
		if articleID == 0 {
			imp.skipped("comments") // article_not_found
			continue
		}
		authorName := table.col(row, "author_name")
		content := table.col(row, "content")
		if authorName == "" || content == "" {
			imp.skipped("comments") // author_name_missing / content_blank
			continue
		}
		platform := table.col(row, "platform")
		externalID := table.col(row, "external_id")

		var existingID int64
		if platform != "" && externalID != "" {
			existingID, err = imp.findExternalComment(ctx, articleID, platform, externalID)
		} else {
			existingID, err = imp.findLocalComment(ctx, row, table, articleID, authorName, content)
		}
		if err != nil {
			return err
		}
		if existingID != 0 {
			if csvID.Valid {
				imp.commentIDs[csvID.Int64] = existingID
			}
			imp.skipped("comments") // already_exists
			continue
		}

		status := int64(0) // empty status imports as pending
		if s := strings.TrimSpace(table.col(row, "status")); s != "" {
			v, perr := strconv.ParseInt(s, 10, 64)
			// An unknown enum value raises in Rails, aborting the import.
			if perr != nil || v < int64(domain.CommentPending) || v > int64(domain.CommentRejected) {
				return fmt.Errorf("invalid comment status %q", s)
			}
			status = v
		}
		newID, err := imp.q.ImportInsertComment(ctx, query.ImportInsertCommentParams{
			CommentableType: sql.NullString{String: "Article", Valid: true},
			CommentableID:   sql.NullInt64{Int64: articleID, Valid: true},
			ArticleID:       sql.NullInt64{Int64: articleID, Valid: true},
			AuthorName:      authorName,
			AuthorEmail:     csvNullStr(table.col(row, "author_email")),
			AuthorUrl:       csvNullStr(table.col(row, "author_url")),
			AuthorUsername:  csvNullStr(table.col(row, "author_username")),
			AuthorAvatarUrl: csvNullStr(table.col(row, "author_avatar_url")),
			Content:         content,
			Status:          status,
			Platform:        csvNullStr(platform),
			ExternalID:      csvNullStr(externalID),
			Url:             csvNullStr(table.col(row, "url")),
			PublishedAt:     csvNullInt(table.col(row, "published_at")),
			CreatedAt:       csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt:       csvTimeNow(table.col(row, "updated_at"), imp.now),
		})
		if err != nil {
			return err
		}
		if csvID.Valid {
			imp.commentIDs[csvID.Int64] = newID
		}
		imp.imported("comments")
	}

	// Second pass: backfill parent_id.
	for _, row := range table.rows {
		parentID := csvNullInt(table.col(row, "parent_id"))
		csvID := csvNullInt(table.col(row, "id"))
		if !parentID.Valid || !csvID.Valid {
			continue
		}
		newID, ok1 := imp.commentIDs[csvID.Int64]
		newParentID, ok2 := imp.commentIDs[parentID.Int64]
		if !ok1 || !ok2 {
			continue
		}
		if err := imp.q.ImportSetCommentParent(ctx, query.ImportSetCommentParentParams{
			ParentID:  sql.NullInt64{Int64: newParentID, Valid: true},
			UpdatedAt: imp.now,
			ID:        newID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// findExternalComment dedupes on (article_id, platform, external_id).
func (imp *zipImport) findExternalComment(ctx context.Context, articleID int64, platform, externalID string) (int64, error) {
	id, err := imp.q.ImportExternalCommentID(ctx, query.ImportExternalCommentIDParams{
		ArticleID:  sql.NullInt64{Int64: articleID, Valid: true},
		Platform:   sql.NullString{String: platform, Valid: true},
		ExternalID: sql.NullString{String: externalID, Valid: true},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// findLocalComment dedupes local comments on author+content within a +-5s
// window on published_at (preferred) or created_at.
func (imp *zipImport) findLocalComment(ctx context.Context, row []string, table *csvTable, articleID int64, authorName, content string) (int64, error) {
	const window = 5
	article := sql.NullInt64{Int64: articleID, Valid: true}
	var id int64
	var err error
	if publishedAt := csvNullInt(table.col(row, "published_at")); publishedAt.Valid {
		id, err = imp.q.ImportLocalCommentIDByPublishedAt(ctx, query.ImportLocalCommentIDByPublishedAtParams{
			ArticleID:  article,
			AuthorName: authorName,
			Content:    content,
			FromTs:     sql.NullInt64{Int64: publishedAt.Int64 - window, Valid: true},
			ToTs:       sql.NullInt64{Int64: publishedAt.Int64 + window, Valid: true},
		})
	} else if createdAt := csvNullInt(table.col(row, "created_at")); createdAt.Valid {
		id, err = imp.q.ImportLocalCommentIDByCreatedAt(ctx, query.ImportLocalCommentIDByCreatedAtParams{
			ArticleID:  article,
			AuthorName: authorName,
			Content:    content,
			FromTs:     createdAt.Int64 - window,
			ToTs:       createdAt.Int64 + window,
		})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// importSubscribers mirrors import_subscribers: skip existing emails. The
// export never carries tokens, so fresh ones are generated like
// Subscriber#generate_tokens.
func (imp *zipImport) importSubscribers(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "subscribers.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		email := table.col(row, "email")
		if id, err := imp.resolveSubscriberID(ctx, email); err != nil {
			return err
		} else if id != 0 {
			imp.skipped("subscribers")
			continue
		}
		confirmToken, err := subscribers.NewToken()
		if err != nil {
			return err
		}
		unsubToken, err := subscribers.NewToken()
		if err != nil {
			return err
		}
		newID, err := imp.q.ImportInsertSubscriber(ctx, query.ImportInsertSubscriberParams{
			Email:             email,
			ConfirmationToken: sql.NullString{String: confirmToken, Valid: true},
			UnsubscribeToken:  sql.NullString{String: unsubToken, Valid: true},
			ConfirmedAt:       csvNullInt(table.col(row, "confirmed_at")),
			UnsubscribedAt:    csvNullInt(table.col(row, "unsubscribed_at")),
			CreatedAt:         csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt:         csvTimeNow(table.col(row, "updated_at"), imp.now),
		})
		if err != nil {
			return err
		}
		imp.subscriberMails[email] = newID
		imp.imported("subscribers")
	}
	return nil
}

// importSubscriberTags mirrors import_subscriber_tags.
func (imp *zipImport) importSubscriberTags(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "subscriber_tags.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		email := table.col(row, "subscriber_email")
		if email == "" {
			imp.skipped("subscriber_tags")
			continue
		}
		subscriberID, err := imp.resolveSubscriberID(ctx, email)
		if err != nil {
			return err
		}
		if subscriberID == 0 {
			imp.skipped("subscriber_tags") // subscriber_not_found
			continue
		}
		tagSlug := table.col(row, "tag_slug")
		if tagSlug == "" {
			imp.skipped("subscriber_tags")
			continue
		}
		tagID, err := imp.resolveTagID(ctx, tagSlug)
		if err != nil {
			return err
		}
		if tagID == 0 {
			imp.skipped("subscriber_tags") // tag_not_found
			continue
		}
		if _, err := imp.q.ImportSubscriberTagID(ctx, query.ImportSubscriberTagIDParams{SubscriberID: subscriberID, TagID: tagID}); err == nil {
			imp.skipped("subscriber_tags") // already_exists
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := imp.q.ImportInsertSubscriberTag(ctx, query.ImportInsertSubscriberTagParams{
			SubscriberID: subscriberID,
			TagID:        tagID,
			CreatedAt:    csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt:    csvTimeNow(table.col(row, "updated_at"), imp.now),
		}); err != nil {
			return err
		}
		imp.imported("subscriber_tags")
	}
	return nil
}

// importSettings mirrors import_settings: the singleton row is updated when
// present, otherwise created from the first CSV row.
func (imp *zipImport) importSettings(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "settings.csv")
	if table == nil || err != nil || len(table.rows) == 0 {
		return err
	}
	row := table.rows[0]
	timeZone := table.col(row, "time_zone")
	if timeZone == "" {
		timeZone = "UTC"
	}
	params := query.ImportUpdateSettingsParams{
		Title:          csvNullStr(table.col(row, "title")),
		Description:    csvNullStr(table.col(row, "description")),
		Author:         csvNullStr(table.col(row, "author")),
		Url:            csvNullStr(table.col(row, "url")),
		TimeZone:       timeZone,
		HeadCode:       csvNullStr(table.col(row, "head_code")),
		CustomCss:      csvNullStr(table.col(row, "custom_css")),
		ToolCode:       csvNullStr(table.col(row, "tool_code")),
		Giscus:         csvNullStr(table.col(row, "giscus")),
		SocialLinks:    csvNullStr(table.col(row, "social_links")),
		SetupCompleted: csvBool(table.col(row, "setup_completed")),
		CreatedAt:      csvTimeNow(table.col(row, "created_at"), imp.now),
		UpdatedAt:      csvTimeNow(table.col(row, "updated_at"), imp.now),
	}
	if _, err := imp.q.ImportSettingsID(ctx); err == nil {
		if err := imp.q.ImportUpdateSettings(ctx, params); err != nil {
			return err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if err := imp.q.ImportInsertSettings(ctx, query.ImportInsertSettingsParams(params)); err != nil {
			return err
		}
	} else {
		return err
	}
	imp.imported("settings")
	return nil
}

// crosspostPlatforms mirrors Crosspost::PLATFORMS.
var crosspostPlatforms = map[string]bool{"mastodon": true, "twitter": true, "bluesky": true, "xiaohongshu": true}

// importCrossposts mirrors import_crossposts: upsert by platform, [REDACTED]
// secrets keep the stored value, and a row enabled without its required
// credentials is imported disabled (the Rails validation-rescue fallback).
func (imp *zipImport) importCrossposts(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "crossposts.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		platform := strings.ToLower(strings.TrimSpace(table.col(row, "platform")))
		if platform == "" || !crosspostPlatforms[platform] {
			imp.skipped("crossposts") // platform_missing / unsupported_platform
			continue
		}
		existing, err := imp.q.ImportCrosspostByPlatform(ctx, platform)
		isNew := errors.Is(err, sql.ErrNoRows)
		if err != nil && !isNew {
			return err
		}

		// Secret columns: a [REDACTED] placeholder never overwrites the
		// stored value (new rows get NULL).
		secrets := crosspostSecrets{
			ApiKey:            imp.secretValue(table.col(row, "api_key"), existing.ApiKey, isNew),
			ApiKeySecret:      imp.secretValue(table.col(row, "api_key_secret"), existing.ApiKeySecret, isNew),
			AccessToken:       imp.secretValue(table.col(row, "access_token"), existing.AccessToken, isNew),
			AccessTokenSecret: imp.secretValue(table.col(row, "access_token_secret"), existing.AccessTokenSecret, isNew),
			ClientKey:         imp.secretValue(table.col(row, "client_key"), existing.ClientKey, isNew),
			ClientSecret:      imp.secretValue(table.col(row, "client_secret"), existing.ClientSecret, isNew),
			AppPassword:       imp.secretValue(table.col(row, "app_password"), existing.AppPassword, isNew),
			RefreshToken:      imp.secretValue(table.col(row, "refresh_token"), existing.RefreshToken, isNew),
		}
		enabled := csvBool(table.col(row, "enabled"))
		username := csvNullStr(table.col(row, "username"))
		if enabled == 1 && !crosspostCredentialsPresent(platform, secrets, username) {
			enabled = 0 // enabled without credentials: import disabled
		}
		maxCharacters := csvNullInt(table.col(row, "max_characters"))
		if maxCharacters.Valid && maxCharacters.Int64 <= 0 {
			maxCharacters = sql.NullInt64{}
		}

		if isNew {
			if err := imp.q.ImportInsertCrosspost(ctx, query.ImportInsertCrosspostParams{
				Platform:             platform,
				Enabled:              enabled,
				ApiKey:               secrets.ApiKey,
				ApiKeySecret:         secrets.ApiKeySecret,
				AccessToken:          secrets.AccessToken,
				AccessTokenSecret:    secrets.AccessTokenSecret,
				ClientID:             csvNullStr(table.col(row, "client_id")),
				ClientKey:            secrets.ClientKey,
				ClientSecret:         secrets.ClientSecret,
				AppPassword:          secrets.AppPassword,
				RefreshToken:         secrets.RefreshToken,
				TokenExpiresAt:       csvNullInt(table.col(row, "token_expires_at")),
				ServerUrl:            csvNullStr(table.col(row, "server_url")),
				Username:             username,
				MaxCharacters:        maxCharacters,
				AutoFetchComments:    csvBool(table.col(row, "auto_fetch_comments")),
				CommentFetchSchedule: csvNullStr(table.col(row, "comment_fetch_schedule")),
				Settings:             csvNullStr(table.col(row, "settings")),
				CreatedAt:            csvTimeNow(table.col(row, "created_at"), imp.now),
				UpdatedAt:            csvTimeNow(table.col(row, "updated_at"), imp.now),
			}); err != nil {
				return err
			}
			imp.imported("crossposts")
		} else {
			if err := imp.q.ImportUpdateCrosspost(ctx, query.ImportUpdateCrosspostParams{
				Enabled:              enabled,
				ApiKey:               secrets.ApiKey,
				ApiKeySecret:         secrets.ApiKeySecret,
				AccessToken:          secrets.AccessToken,
				AccessTokenSecret:    secrets.AccessTokenSecret,
				ClientID:             csvNullStr(table.col(row, "client_id")),
				ClientKey:            secrets.ClientKey,
				ClientSecret:         secrets.ClientSecret,
				AppPassword:          secrets.AppPassword,
				RefreshToken:         secrets.RefreshToken,
				TokenExpiresAt:       csvNullInt(table.col(row, "token_expires_at")),
				ServerUrl:            csvNullStr(table.col(row, "server_url")),
				Username:             username,
				MaxCharacters:        maxCharacters,
				AutoFetchComments:    csvBool(table.col(row, "auto_fetch_comments")),
				CommentFetchSchedule: csvNullStr(table.col(row, "comment_fetch_schedule")),
				Settings:             csvNullStr(table.col(row, "settings")),
				CreatedAt:            csvTimeNow(table.col(row, "created_at"), imp.now),
				UpdatedAt:            csvTimeNow(table.col(row, "updated_at"), imp.now),
				Platform:             platform,
			}); err != nil {
				return err
			}
			imp.updated("crossposts")
		}
	}
	return nil
}

// crosspostSecrets carries the credential columns of one crossposts row.
type crosspostSecrets struct {
	ApiKey, ApiKeySecret, AccessToken, AccessTokenSecret sql.NullString
	ClientKey, ClientSecret, AppPassword, RefreshToken   sql.NullString
}

// secretValue returns the CSV value unless it is the redaction placeholder,
// in which case the existing value is kept (NULL for new rows).
func (imp *zipImport) secretValue(csvVal string, existing sql.NullString, isNew bool) sql.NullString {
	if csvVal == RedactedValue {
		if isNew {
			return sql.NullString{}
		}
		return existing
	}
	return csvNullStr(csvVal)
}

// crosspostCredentialsPresent mirrors the enabled-state presence validations
// of the Crosspost model.
func crosspostCredentialsPresent(platform string, s crosspostSecrets, username sql.NullString) bool {
	present := func(v sql.NullString) bool { return v.Valid && v.String != "" }
	switch platform {
	case "mastodon":
		return present(s.ClientKey) && present(s.ClientSecret) && present(s.AccessToken)
	case "twitter":
		return present(s.AccessToken) && present(s.AccessTokenSecret) && present(s.ApiKey) && present(s.ApiKeySecret)
	case "bluesky":
		return present(username) && present(s.AppPassword)
	}
	return true
}

// importListmonks mirrors import_listmonks: singleton upsert; a redacted
// api_key keeps the stored value.
func (imp *zipImport) importListmonks(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "listmonks.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		listmonkURL := strings.TrimSpace(table.col(row, "url"))
		if listmonkURL == "" {
			imp.skipped("listmonks") // url_missing
			continue
		}
		existing, err := imp.q.ImportListmonk(ctx)
		isNew := errors.Is(err, sql.ErrNoRows)
		if err != nil && !isNew {
			return err
		}
		apiKey := imp.secretValue(table.col(row, "api_key"), existing.ApiKey, isNew)
		listID := csvNullInt(table.col(row, "list_id"))
		templateID := csvNullInt(table.col(row, "template_id"))
		enabled := csvBool(table.col(row, "enabled"))
		username := csvNullStr(table.col(row, "username"))
		createdAt := csvTimeNow(table.col(row, "created_at"), imp.now)
		updatedAt := csvTimeNow(table.col(row, "updated_at"), imp.now)

		if isNew {
			if err := imp.q.ImportInsertListmonk(ctx, query.ImportInsertListmonkParams{
				Url: sql.NullString{String: listmonkURL, Valid: true}, Username: username, ApiKey: apiKey,
				ListID: listID, TemplateID: templateID, Enabled: enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
			}); err != nil {
				return err
			}
			imp.imported("listmonks")
		} else {
			if err := imp.q.ImportUpdateListmonk(ctx, query.ImportUpdateListmonkParams{
				Url: sql.NullString{String: listmonkURL, Valid: true}, Username: username, ApiKey: apiKey,
				ListID: listID, TemplateID: templateID, Enabled: enabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
			}); err != nil {
				return err
			}
			imp.updated("listmonks")
		}
	}
	return nil
}

// importNewsletterSettings mirrors import_newsletter_settings: singleton
// upsert over the first CSV row; a redacted smtp_password keeps the stored
// value.
func (imp *zipImport) importNewsletterSettings(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "newsletter_settings.csv")
	if table == nil || err != nil || len(table.rows) == 0 {
		return err
	}
	row := table.rows[0]
	existing, err := imp.q.ImportNewsletterSetting(ctx)
	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return err
	}
	provider := table.col(row, "provider")
	if provider == "" {
		provider = "native"
	}
	smtpAuth := csvNullStr(table.col(row, "smtp_authentication"))
	if !smtpAuth.Valid {
		smtpAuth = sql.NullString{String: "plain", Valid: true}
	}
	params := query.ImportUpdateNewsletterSettingParams{
		Enabled:            csvBool(table.col(row, "enabled")),
		Provider:           provider,
		FromEmail:          csvNullStr(table.col(row, "from_email")),
		SmtpAddress:        csvNullStr(table.col(row, "smtp_address")),
		SmtpPort:           csvNullInt(table.col(row, "smtp_port")),
		SmtpUserName:       csvNullStr(table.col(row, "smtp_user_name")),
		SmtpPassword:       imp.secretValue(table.col(row, "smtp_password"), existing.SmtpPassword, isNew),
		SmtpDomain:         csvNullStr(table.col(row, "smtp_domain")),
		SmtpAuthentication: smtpAuth,
		SmtpEnableStarttls: sql.NullInt64{Int64: csvBool(table.col(row, "smtp_enable_starttls")), Valid: true},
		CreatedAt:          csvTimeNow(table.col(row, "created_at"), imp.now),
		UpdatedAt:          csvTimeNow(table.col(row, "updated_at"), imp.now),
	}
	if isNew {
		if err := imp.q.ImportInsertNewsletterSetting(ctx, query.ImportInsertNewsletterSettingParams(params)); err != nil {
			return err
		}
	} else {
		if err := imp.q.ImportUpdateNewsletterSetting(ctx, params); err != nil {
			return err
		}
	}
	imp.imported("newsletter_settings")
	return nil
}

// importSocialMediaPosts mirrors import_social_media_posts: resolve the
// article via article_slug, skip existing (article_id, platform) pairs.
func (imp *zipImport) importSocialMediaPosts(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "social_media_posts.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		articleSlug := table.col(row, "article_slug")
		if articleSlug == "" {
			imp.skipped("social_media_posts")
			continue
		}
		articleID, err := imp.resolveArticleID(ctx, articleSlug)
		if err != nil {
			return err
		}
		if articleID == 0 {
			imp.skipped("social_media_posts") // article_not_found
			continue
		}
		platform := table.col(row, "platform")
		if _, err := imp.q.ImportSocialMediaPostID(ctx, query.ImportSocialMediaPostIDParams{ArticleID: articleID, Platform: platform}); err == nil {
			imp.skipped("social_media_posts") // already_exists
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := imp.q.ImportInsertSocialMediaPost(ctx, query.ImportInsertSocialMediaPostParams{
			ArticleID: articleID,
			Platform:  platform,
			Url:       table.col(row, "url"),
			CreatedAt: csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt: csvTimeNow(table.col(row, "updated_at"), imp.now),
		}); err != nil {
			return err
		}
		imp.imported("social_media_posts")
	}
	return nil
}

// importRedirects mirrors import_redirects: skip regexes that already exist.
func (imp *zipImport) importRedirects(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "redirects.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		if _, err := imp.q.ImportRedirectIDByRegex(ctx, table.col(row, "regex")); err == nil {
			imp.skipped("redirects") // already_exists
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := imp.q.ImportInsertRedirect(ctx, query.ImportInsertRedirectParams{
			Regex:       table.col(row, "regex"),
			Replacement: table.col(row, "replacement"),
			Enabled:     csvBool(table.col(row, "enabled")),
			Permanent:   csvBool(table.col(row, "permanent")),
			CreatedAt:   csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt:   csvTimeNow(table.col(row, "updated_at"), imp.now),
		}); err != nil {
			return err
		}
		imp.imported("redirects")
	}
	return nil
}

// importFiles imports files.csv rows, keeping the exported key verbatim, and
// queues the staged blob contents for restoration after commit. Rows whose
// key already exists are skipped (their blob is only restored when missing
// on disk).
func (imp *zipImport) importFiles(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "files.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		csvID := csvNullInt(table.col(row, "id"))
		key := table.col(row, "key")
		filename := table.col(row, "filename")
		if key == "" || !media.ValidKey(key) {
			imp.skipped("files")
			continue
		}
		var staged string
		if csvID.Valid {
			staged = imp.stagedBlobPath(csvID.Int64, filename)
		}
		if existing, err := imp.q.ImportFileByKey(ctx, key); err == nil {
			if csvID.Valid {
				imp.fileIDs[csvID.Int64] = existing.ID
				imp.fileKeys[csvID.Int64] = key
				imp.restores = append(imp.restores, blobRestore{staged: staged, key: key, onlyIfMissing: true})
			}
			imp.skipped("files") // already_exists
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// variant_of remaps through the old->new id map; unresolved
		// references import as NULL rather than violating the FK.
		variantOf := sql.NullInt64{}
		if v := csvNullInt(table.col(row, "variant_of")); v.Valid {
			if mapped, ok := imp.fileIDs[v.Int64]; ok {
				variantOf = sql.NullInt64{Int64: mapped, Valid: true}
			}
		}
		newID, err := imp.q.ImportInsertFile(ctx, query.ImportInsertFileParams{
			Key:         key,
			Filename:    filename,
			ContentType: csvNullStr(table.col(row, "content_type")),
			ByteSize:    csvIntOr(table.col(row, "byte_size"), 0),
			Checksum:    csvNullStr(table.col(row, "checksum")),
			VariantOf:   variantOf,
			CreatedAt:   csvTimeNow(table.col(row, "created_at"), imp.now),
		})
		if err != nil {
			return err
		}
		if csvID.Valid {
			imp.fileIDs[csvID.Int64] = newID
			imp.fileKeys[csvID.Int64] = key
			imp.restores = append(imp.restores, blobRestore{staged: staged, key: key})
		}
		imp.imported("files")
	}
	return nil
}

// stagedBlobPath locates a blob inside the extracted bundle
// (attachments/files/<file_id>_<filename>, see exportFileContents).
func (imp *zipImport) stagedBlobPath(csvFileID int64, filename string) string {
	name := fmt.Sprintf("%d_%s", csvFileID, filepath.Base(filename))
	return filepath.Join(imp.base, "attachments", "files", name)
}

// importStaticFiles mirrors import_static_files: skip existing filenames,
// rows without a blob_filename, and rows whose file content is available
// neither in the bundle nor on disk.
func (imp *zipImport) importStaticFiles(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "static_files.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		csvID := csvNullInt(table.col(row, "id"))
		filename := table.col(row, "filename")
		if filename == "" {
			imp.skipped("static_files")
			continue
		}
		if id, err := imp.q.ImportStaticFileIDByFilename(ctx, filename); err == nil {
			if csvID.Valid {
				imp.staticFileIDs[csvID.Int64] = id
			}
			imp.skipped("static_files") // already_exists
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		blobFilename := table.col(row, "blob_filename")
		if blobFilename == "" {
			imp.skipped("static_files") // blob_filename_missing
			continue
		}
		fileCSV := csvNullInt(table.col(row, "file_id"))
		fileID, ok := imp.fileIDs[fileCSV.Int64]
		key := imp.fileKeys[fileCSV.Int64]
		if !fileCSV.Valid || !ok {
			imp.skipped("static_files") // file_not_found
			continue
		}
		staged := imp.stagedBlobPath(fileCSV.Int64, blobFilename)
		final := filepath.Join(imp.dataDir, "files", key[0:2], key[2:4], key)
		if !fileExists(staged) && !fileExists(final) {
			imp.skipped("static_files") // file_not_found
			continue
		}
		newID, err := imp.q.ImportInsertStaticFile(ctx, query.ImportInsertStaticFileParams{
			Filename:    filename,
			Description: csvNullStr(table.col(row, "description")),
			FileID:      fileID,
			CreatedAt:   csvTimeNow(table.col(row, "created_at"), imp.now),
			UpdatedAt:   csvTimeNow(table.col(row, "updated_at"), imp.now),
		})
		if err != nil {
			return err
		}
		if csvID.Valid {
			imp.staticFileIDs[csvID.Int64] = newID
		}
		imp.imported("static_files")
	}
	return nil
}

// importAttachments imports attachments.csv, remapping file_id through the
// files map and record_id through the article/page/static-file maps for the
// record types this schema links directly. Other record types (e.g. the
// migrated ActionText::RichText provenance rows) keep their record_id.
func (imp *zipImport) importAttachments(ctx context.Context) error {
	table, err := readCSVTable(imp.base, "attachments.csv")
	if table == nil || err != nil {
		return err
	}
	for _, row := range table.rows {
		fileCSV := csvNullInt(table.col(row, "file_id"))
		fileID, ok := imp.fileIDs[fileCSV.Int64]
		if !fileCSV.Valid || !ok {
			imp.skipped("attachments") // file not imported
			continue
		}
		recordType := table.col(row, "record_type")
		recordID := csvNullInt(table.col(row, "record_id"))
		if recordType == "" || !recordID.Valid {
			imp.skipped("attachments")
			continue
		}
		switch strings.ToLower(recordType) {
		case "article":
			if mapped, ok := imp.articleIDs[recordID.Int64]; ok {
				recordID = sql.NullInt64{Int64: mapped, Valid: true}
			}
		case "page":
			if mapped, ok := imp.pageIDs[recordID.Int64]; ok {
				recordID = sql.NullInt64{Int64: mapped, Valid: true}
			}
		case "staticfile":
			if mapped, ok := imp.staticFileIDs[recordID.Int64]; ok {
				recordID = sql.NullInt64{Int64: mapped, Valid: true}
			}
		}
		params := query.ImportAttachmentIDParams{
			RecordType: recordType,
			RecordID:   recordID.Int64,
			Name:       table.col(row, "name"),
			FileID:     fileID,
		}
		if _, err := imp.q.ImportAttachmentID(ctx, params); err == nil {
			imp.skipped("attachments") // already_exists
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := imp.q.ImportInsertAttachment(ctx, query.ImportInsertAttachmentParams{
			FileID:     params.FileID,
			RecordType: params.RecordType,
			RecordID:   params.RecordID,
			Name:       params.Name,
			CreatedAt:  csvTimeNow(table.col(row, "created_at"), imp.now),
		}); err != nil {
			return err
		}
		imp.imported("attachments")
	}
	return nil
}

// restoreBlobs moves the staged blob contents into the media layout
// (<DataDir>/files/xx/yy/<key>) after the transaction committed. A missing
// staged blob is logged and skipped, mirroring the Rails per-file rescue.
func (z *ZipImporter) restoreBlobs(ctx context.Context, imp *zipImport) {
	for _, rb := range imp.restores {
		if rb.staged == "" || !media.ValidKey(rb.key) {
			continue
		}
		if _, err := os.Stat(rb.staged); err != nil {
			activity.Log(ctx, z.DB, "warn", "import_file_skipped", "import", fmt.Sprintf("key=%s reason=missing_blob", rb.key))
			continue
		}
		final := filepath.Join(z.DataDir, "files", rb.key[0:2], rb.key[2:4], rb.key)
		if rb.onlyIfMissing {
			if _, err := os.Stat(final); err == nil {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			activity.Log(ctx, z.DB, "warn", "import_file_skipped", "import", fmt.Sprintf("key=%s reason=%s", rb.key, activity.Quote(err.Error())))
			continue
		}
		if err := os.Rename(rb.staged, final); err != nil {
			activity.Log(ctx, z.DB, "warn", "import_file_skipped", "import", fmt.Sprintf("key=%s reason=%s", rb.key, activity.Quote(err.Error())))
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RegisterImportHandlers installs the kind "import_zip" and "import_rss" job
// handlers (ImportFromZipJob / ImportFromRssJob). Import failures are logged
// to activity_logs and swallowed, like the Rails jobs (no job retry).
func RegisterImportHandlers(w *jobs.Worker, db *sql.DB, dataDir string) {
	w.Register(jobs.KindImportZip, func(ctx context.Context, payload json.RawMessage) error {
		var p ImportZipPayload
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &p); err != nil {
				return fmt.Errorf("import_zip: decode payload: %w", err)
			}
		}
		if p.Path == "" {
			return fmt.Errorf("import_zip: path required")
		}
		activity.Log(ctx, db, "info", "started", "import", fmt.Sprintf("source=\"zip\" file=%s", activity.Quote(p.Path)))
		_, err := (&ZipImporter{DB: db, DataDir: dataDir}).Import(ctx, p.Path)
		cleanupImportedZip(dataDir, p.Path)
		if err != nil {
			activity.Log(ctx, db, "error", "failed", "import", fmt.Sprintf("source=\"zip\" filename=%s error=%s", activity.Quote(filepath.Base(p.Path)), activity.Quote(err.Error())))
			return nil
		}
		activity.Log(ctx, db, "info", "completed", "import", fmt.Sprintf("source=\"zip\" filename=%s", activity.Quote(filepath.Base(p.Path))))
		return nil
	})
	registerImportRSSHandler(w, db, dataDir)
}

// cleanupImportedZip removes the uploaded zip when it lives under
// <dataDir>/imports (ImportFromZipJob#cleanup_temp_file).
func cleanupImportedZip(dataDir, zipPath string) {
	importsDir, err := filepath.Abs(filepath.Join(dataDir, "imports"))
	if err != nil {
		return
	}
	abs, err := filepath.Abs(zipPath)
	if err != nil || !strings.HasPrefix(abs, importsDir+string(os.PathSeparator)) {
		return
	}
	os.Remove(abs)
}
