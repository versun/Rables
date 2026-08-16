// Markdown import: batch-imports articles and pages from markdown documents
// with YAML front matter, the format produced by the Rables markdown export
// (one .md file per record, articles/ and pages/ directories, fields like
// type/title/slug/status/created_at/tags/redirect_url/page_order).
//
// The admin upload stages one ZIP under data/imports (multiple picked .md
// files are packed into a single archive first); the import_markdown job then
// imports every .md/.markdown entry. Documents are imported with fresh ids:
// the source id is ignored, and a document whose slug is already taken (or
// blank, or reserved) is skipped like an RSS entry — per-document failures
// are counted and never abort the batch.
package transfer

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	"rables/internal/service/activity"
	tagsvc "rables/internal/service/tags"
)

// maxMarkdownFileBytes caps one markdown document. Bodies go through
// RenderMarkdown + SanitizeHTML, whose html.ParseFragment bounds nesting
// depth but not the total node count (same rationale as MaxItemContentBytes),
// so a huge document would explode into millions of nodes.
const maxMarkdownFileBytes = 5 << 20

// ImportMarkdownPayload is the job_runs payload for kind "import_markdown".
type ImportMarkdownPayload struct {
	// Path is the staged upload (a ZIP of markdown files, or one bare
	// markdown file), usually <DataDir>/imports/import_*.
	Path string `json:"path"`
}

// markdownFrontmatter is the YAML header of one exported markdown document.
// Timestamps stay strings: quoted and unquoted scalars both reach string
// fields as their literal text, so the parsing layouts live in one place
// (importTime).
type markdownFrontmatter struct {
	Type        string   `yaml:"type"` // "article" (default) or "page"
	Title       string   `yaml:"title"`
	Slug        string   `yaml:"slug"`
	Status      string   `yaml:"status"`
	CreatedAt   string   `yaml:"created_at"`
	UpdatedAt   string   `yaml:"updated_at"`
	Tags        []string `yaml:"tags"`
	RedirectURL string   `yaml:"redirect_url"` // pages only
	PageOrder   int64    `yaml:"page_order"`   // pages only
}

// MarkdownImporter imports markdown documents into articles and pages.
type MarkdownImporter struct {
	DB *sql.DB
	// Now overrides the clock; nil uses time.Now.
	Now func() time.Time
}

// MarkdownImportResult counts imported and failed documents.
type MarkdownImportResult struct {
	Imported int
	Failed   int
}

// Import reads path — a ZIP of markdown files or one bare markdown file —
// and imports every document it carries. Entries that are not markdown files
// are ignored. A failure to open/read the archive aborts the import;
// per-document failures only count against the result, like the RSS import.
func (m *MarkdownImporter) Import(ctx context.Context, path string) (*MarkdownImportResult, error) {
	res := &MarkdownImportResult{}
	isZip, err := isZipBundle(path)
	if err != nil {
		return nil, err
	}
	if !isZip {
		raw, err := readMarkdownFile(path, maxMarkdownFileBytes)
		if err != nil {
			return nil, err
		}
		m.importDocument(ctx, filepath.Base(path), raw, res)
		return res, nil
	}

	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("import markdown: open zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !markdownFileExt(f.Name) {
			continue
		}
		raw, err := readMarkdownZipEntry(f)
		if err != nil {
			slog.Default().Warn("import markdown: entry unreadable", "entry", f.Name, "error", err)
			res.Failed++
			continue
		}
		m.importDocument(ctx, f.Name, raw, res)
	}
	return res, nil
}

// importDocument parses one markdown document and inserts it as an article
// (type missing or "article") or a page (type "page").
func (m *MarkdownImporter) importDocument(ctx context.Context, name string, raw []byte, res *MarkdownImportResult) {
	fm, body, err := parseMarkdownDocument(raw)
	if err != nil {
		slog.Default().Warn("import markdown: document failed", "entry", name, "error", err)
		res.Failed++
		return
	}
	switch strings.ToLower(strings.TrimSpace(fm.Type)) {
	case "", "article":
		err = m.importArticle(ctx, fm, body)
	case "page":
		err = m.importPage(ctx, fm, body)
	default:
		err = fmt.Errorf("unknown type %q", fm.Type)
	}
	if err != nil {
		slog.Default().Warn("import markdown: document failed", "entry", name, "error", err)
		res.Failed++
		return
	}
	res.Imported++
}

// parseMarkdownDocument splits the YAML front matter from the markdown body.
// The header is delimited by a first and a later line of exactly "---"; a
// document without one is rejected — the front matter carries the record
// fields, so a plain markdown file is not a valid export document.
func parseMarkdownDocument(raw []byte) (*markdownFrontmatter, string, error) {
	s := strings.TrimPrefix(string(raw), "\uFEFF")
	lines := strings.Split(s, "\n")
	if strings.TrimRight(lines[0], "\r") != "---" {
		return nil, "", errors.New("missing front matter (expected a first line of ---)")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") != "---" {
			continue
		}
		var fm markdownFrontmatter
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &fm); err != nil {
			return nil, "", fmt.Errorf("parse front matter: %w", err)
		}
		return &fm, strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\n"), nil
	}
	return nil, "", errors.New("unterminated front matter (no closing --- line)")
}

// importArticle inserts one article, mirroring the RSS importer's guards
// (blank/reserved/taken slug, blank content) and the admin markdown write
// path (RenderMarkdown + SanitizeHTML + AddLazyLoading, source kept in
// content_markdown). Front-matter tags are find-or-created and attached in
// the same transaction, like articles.Save.
func (m *MarkdownImporter) importArticle(ctx context.Context, fm *markdownFrontmatter, body string) error {
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	// A document with neither a title nor a slug (e.g. an empty front matter
	// block) is a malformed export, not an untitled article: importing it
	// under a time-derived slug would silently create garbage content.
	if domain.IsBlank(fm.Title) && domain.IsBlank(fm.Slug) {
		return errors.New("title and slug are both blank")
	}
	q := query.New(m.DB)
	exists := func(candidate string) bool {
		_, err := q.ImportArticleIDBySlug(ctx, sql.NullString{String: candidate, Valid: true})
		return err == nil
	}
	slug := domain.GenerateSlug(fm.Slug, fm.Title, now, exists)
	if domain.IsBlank(slug) {
		return errors.New("slug is blank")
	}
	if domain.IsReservedSlug(slug) || slices.Contains(domain.AdminArticleBatchSlugs, slug) {
		return fmt.Errorf("slug %q is reserved", slug)
	}
	if exists(slug) {
		return fmt.Errorf("slug %q already exists", slug)
	}
	if domain.IsBlank(body) {
		return errors.New("content blank")
	}

	contentHTML := domain.AddLazyLoading(domain.SanitizeHTML(domain.RenderMarkdown(body)))
	createdAt, updatedAt := importTimestamps(fm, now)

	tx, err := m.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := q.WithTx(tx)
	articleID, err := qtx.ImportInsertArticle(ctx, query.ImportInsertArticleParams{
		Title:                       sql.NullString{String: fm.Title, Valid: fm.Title != ""},
		Slug:                        sql.NullString{String: slug, Valid: true},
		ContentHtml:                 sql.NullString{String: contentHTML, Valid: true},
		ContentType:                 string(domain.ContentTypeMarkdown),
		ContentMarkdown:             sql.NullString{String: body, Valid: true},
		Excerpt:                     sql.NullString{String: domain.BuildExcerpt("", contentHTML), Valid: true},
		Status:                      int64(importStatus(fm.Status)),
		Comment:                     0,
		ScheduledCrosspostPlatforms: "[]",
		ScheduledSendNewsletter:     0,
		CreatedAt:                   createdAt,
		UpdatedAt:                   updatedAt,
	})
	if err != nil {
		return err
	}
	tagIDs, err := tagsvc.FindOrCreateByNames(ctx, qtx, fm.Tags)
	if err != nil {
		return err
	}
	for _, tagID := range tagIDs {
		if err := qtx.InsertArticleTag(ctx, query.InsertArticleTagParams{
			ArticleID: articleID, TagID: tagID, CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// importPage inserts one page, mirroring the admin page validations (title
// and slug required, redirect_url must be http(s) when present). Unlike
// articles the body may stay blank: redirect pages carry no content.
func (m *MarkdownImporter) importPage(ctx context.Context, fm *markdownFrontmatter, body string) error {
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	if domain.IsBlank(fm.Title) {
		return errors.New("title is blank")
	}
	slug := domain.CleanSlug(strings.TrimSpace(fm.Slug))
	if domain.IsBlank(slug) {
		return errors.New("slug is blank")
	}
	if slices.Contains(domain.AdminPageBatchSlugs, slug) {
		return fmt.Errorf("slug %q is reserved", slug)
	}
	q := query.New(m.DB)
	if taken, err := q.AdminPageSlugCount(ctx, query.AdminPageSlugCountParams{
		Slug: sql.NullString{String: slug, Valid: true}, ID: 0,
	}); err != nil {
		return err
	} else if taken > 0 {
		return fmt.Errorf("slug %q already exists", slug)
	}
	if redirect := strings.TrimSpace(fm.RedirectURL); redirect != "" && !validImportRedirectURL(redirect) {
		return fmt.Errorf("redirect url %q is not a valid URL", redirect)
	}

	var contentHTML, contentMarkdown string
	if !domain.IsBlank(body) {
		contentHTML = domain.AddLazyLoading(domain.SanitizeHTML(domain.RenderMarkdown(body)))
		contentMarkdown = body
	}
	createdAt, updatedAt := importTimestamps(fm, now)
	_, err := q.CreatePage(ctx, query.CreatePageParams{
		Title:           sql.NullString{String: fm.Title, Valid: true},
		Slug:            sql.NullString{String: slug, Valid: true},
		ContentHtml:     sql.NullString{String: contentHTML, Valid: true},
		ContentType:     string(domain.ContentTypeMarkdown),
		ContentMarkdown: sql.NullString{String: contentMarkdown, Valid: contentMarkdown != ""},
		RedirectUrl:     sql.NullString{String: strings.TrimSpace(fm.RedirectURL), Valid: true},
		PageOrder:       fm.PageOrder,
		Status:          int64(importStatus(fm.Status)),
		Comment:         0,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	})
	return err
}

// importStatus maps the front-matter status onto the article/page enum.
// Anything but an explicit "publish"/"draft" imports as a draft — a
// scheduled/trashed record must never go live because the import could not
// map its status. A missing status means publish, like the RSS import.
func importStatus(raw string) domain.Status {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "publish":
		return domain.StatusPublish
	default:
		return domain.StatusDraft
	}
}

// importTimestamps parses the front-matter created_at/updated_at (RFC3339 in
// the export). Blank or unparseable values fall back to now, and a missing
// updated_at falls back to created_at.
func importTimestamps(fm *markdownFrontmatter, now time.Time) (createdAt, updatedAt int64) {
	created := importTime(fm.CreatedAt, now)
	updated := importTime(fm.UpdatedAt, created)
	return created.Unix(), updated.Unix()
}

// importTime parses one timestamp, accepting RFC3339 plus the common
// space-separated variants; a blank or unparseable value yields fallback.
func importTime(raw string, fallback time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05 -07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return fallback
}

// validImportRedirectURL mirrors the admin page form's UrlValidator: http(s)
// scheme with a host.
func validImportRedirectURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// markdownFileExt reports whether name is a markdown document.
func markdownFileExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

// readMarkdownFile reads one staged markdown document, capped at limit.
func readMarkdownFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("import markdown: open source: %w", err)
	}
	defer f.Close()
	return readMarkdownCapped(f, path, limit)
}

// readMarkdownZipEntry reads one markdown ZIP entry, capped at
// maxMarkdownFileBytes.
func readMarkdownZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readMarkdownCapped(rc, f.Name, maxMarkdownFileBytes)
}

// readMarkdownCapped reads one document up to limit bytes; over the limit is
// an error, not a truncation (a truncated document would import half a
// post).
func readMarkdownCapped(r io.Reader, name string, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s exceeds the %d byte document limit", name, limit)
	}
	return raw, nil
}

// registerImportMarkdownHandler installs the kind "import_markdown" job
// handler; called from RegisterImportHandlers. The staged upload is removed
// either way once the job ran (web uploads are always removed, like the db
// import's), and failures are logged to activity_logs, not retried.
func registerImportMarkdownHandler(w *jobs.Worker, db *sql.DB, dataDir string) {
	w.Register(jobs.KindImportMarkdown, func(ctx context.Context, payload json.RawMessage) error {
		var p ImportMarkdownPayload
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &p); err != nil {
				return fmt.Errorf("import_markdown: decode payload: %w", err)
			}
		}
		if p.Path == "" {
			return fmt.Errorf("import_markdown: path required")
		}
		activity.Log(ctx, db, "info", "started", "import", fmt.Sprintf("source=\"markdown\" file=%s", activity.Quote(filepath.Base(p.Path))))
		res, err := (&MarkdownImporter{DB: db}).Import(ctx, p.Path)
		cleanupImportUpload(dataDir, p.Path)
		if err != nil {
			activity.Log(ctx, db, "error", "failed", "import", fmt.Sprintf("source=\"markdown\" file=%s error=%s", activity.Quote(filepath.Base(p.Path)), activity.Quote(err.Error())))
			return nil
		}
		activity.Log(ctx, db, "info", "completed", "import", fmt.Sprintf("source=\"markdown\" file=%s failed_count=%d imported_count=%d", activity.Quote(filepath.Base(p.Path)), res.Failed, res.Imported))
		return nil
	})
}
