package transfer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"gopkg.in/yaml.v3"

	"rables/internal/db/query"
	"rables/internal/domain"
)

// MarkdownExporter writes articles and pages as Markdown files (YAML front
// matter + converted body) into articles/ and pages/ subdirs, zipped into
// <DataDir>/exports (MarkdownExport).
type MarkdownExporter struct {
	DB      *sql.DB
	DataDir string

	q *query.Queries
}

// Front matter mirrors MarkdownExport#write_markdown_file; timestamps are
// RFC3339 (the iso8601 the Rails export wrote). Optional fields are omitted
// when empty, like the Rails .compact.

type articleFrontMatter struct {
	Type        string   `yaml:"type"`
	ID          int64    `yaml:"id"`
	Title       string   `yaml:"title,omitempty"`
	Slug        string   `yaml:"slug,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Status      string   `yaml:"status"`
	ScheduledAt string   `yaml:"scheduled_at,omitempty"`
	CreatedAt   string   `yaml:"created_at"`
	UpdatedAt   string   `yaml:"updated_at"`
	Tags        []string `yaml:"tags"`
}

type pageFrontMatter struct {
	Type        string `yaml:"type"`
	ID          int64  `yaml:"id"`
	Title       string `yaml:"title,omitempty"`
	Slug        string `yaml:"slug,omitempty"`
	Status      string `yaml:"status"`
	RedirectURL string `yaml:"redirect_url,omitempty"`
	PageOrder   int64  `yaml:"page_order"`
	CreatedAt   string `yaml:"created_at"`
	UpdatedAt   string `yaml:"updated_at"`
}

// Generate writes markdown_export_<ts>_<rand>.zip and returns its path.
func (e *MarkdownExporter) Generate(ctx context.Context) (string, error) {
	e.q = query.New(e.DB)
	stage, err := stagingDir(e.DataDir, "markdown_export")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)

	if err := e.exportArticles(ctx, stage); err != nil {
		return "", err
	}
	if err := e.exportPages(ctx, stage); err != nil {
		return "", err
	}
	return zipStaging(stage)
}

func (e *MarkdownExporter) exportArticles(ctx context.Context, stage string) error {
	rows, err := e.q.ExportArticles(ctx)
	if err != nil {
		return fmt.Errorf("markdown export articles: %w", err)
	}
	dir := filepath.Join(stage, "articles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	used := map[string]bool{}
	for _, a := range rows {
		markdown, err := htmlToMarkdown(a.ContentHtml.String)
		if err != nil {
			return fmt.Errorf("markdown export article %d: %w", a.ID, err)
		}
		body := strings.TrimSpace(strings.Join(nonEmpty(referenceMarkdown(a), markdown), "\n\n"))

		tags, err := e.q.ListTagsForArticle(ctx, a.ID)
		if err != nil {
			return fmt.Errorf("markdown export article %d tags: %w", a.ID, err)
		}
		names := make([]string, 0, len(tags))
		for _, t := range tags {
			names = append(names, t.Name)
		}

		fm := articleFrontMatter{
			Type:        "article",
			ID:          a.ID,
			Title:       a.Title.String,
			Slug:        a.Slug.String,
			Description: a.Description.String,
			Status:      domain.Status(a.Status).String(),
			ScheduledAt: rfc3339(a.ScheduledAt),
			CreatedAt:   time.Unix(a.CreatedAt, 0).UTC().Format(time.RFC3339),
			UpdatedAt:   time.Unix(a.UpdatedAt, 0).UTC().Format(time.RFC3339),
			Tags:        names,
		}
		base := a.Slug.String
		if base == "" {
			base = fmt.Sprintf("article_%d", a.ID)
		}
		if err := writeMarkdownFile(dir, dedupeBasename(safeBasename(base), used, a.Slug.String, a.ID), fm, body); err != nil {
			return err
		}
	}
	return nil
}

func (e *MarkdownExporter) exportPages(ctx context.Context, stage string) error {
	rows, err := e.q.ExportPages(ctx)
	if err != nil {
		return fmt.Errorf("markdown export pages: %w", err)
	}
	dir := filepath.Join(stage, "pages")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	used := map[string]bool{}
	for _, p := range rows {
		markdown, err := htmlToMarkdown(p.ContentHtml.String)
		if err != nil {
			return fmt.Errorf("markdown export page %d: %w", p.ID, err)
		}
		fm := pageFrontMatter{
			Type:        "page",
			ID:          p.ID,
			Title:       p.Title.String,
			Slug:        p.Slug.String,
			Status:      domain.Status(p.Status).String(),
			RedirectURL: p.RedirectUrl.String,
			PageOrder:   p.PageOrder,
			CreatedAt:   time.Unix(p.CreatedAt, 0).UTC().Format(time.RFC3339),
			UpdatedAt:   time.Unix(p.UpdatedAt, 0).UTC().Format(time.RFC3339),
		}
		base := p.Slug.String
		if base == "" {
			base = fmt.Sprintf("page_%d", p.ID)
		}
		if err := writeMarkdownFile(dir, dedupeBasename(safeBasename(base), used, p.Slug.String, p.ID), fm, strings.TrimSpace(markdown)); err != nil {
			return err
		}
	}
	return nil
}

// htmlToMarkdown mirrors the ReverseMarkdown.convert call with
// github_flavored: the commonmark base plus the GFM strikethrough/table
// extensions.
func htmlToMarkdown(html string) (string, error) {
	if strings.TrimSpace(html) == "" {
		return "", nil
	}
	conv := converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
			strikethrough.NewStrikethroughPlugin(),
			table.NewTablePlugin(),
		),
	)
	return conv.ConvertString(html)
}

// referenceMarkdown ports MarkdownExport#reference_markdown_for: a
// "Reference:" block quoting the source author/content/URL for articles
// archived from external posts.
func referenceMarkdown(a query.Article) string {
	author := sanitizeSourceText(a.SourceAuthor.String, false)
	content := sanitizeSourceText(a.SourceContent.String, true)
	url := sanitizeSourceURL(a.SourceUrl.String)
	if author == "" && content == "" && url == "" {
		return ""
	}

	lines := []string{"Reference:"}
	if author != "" {
		lines = append(lines, "Source: "+author)
	}
	var quote []string
	if content != "" {
		quote = append(quote, strings.Split(content, "\n")...)
	}
	if url != "" {
		if content != "" {
			quote = append(quote, "")
		}
		quote = append(quote, "Original: "+url)
	}
	if len(quote) > 0 {
		lines = append(lines, "")
		for _, line := range quote {
			if line == "" {
				lines = append(lines, ">")
			} else {
				lines = append(lines, "> "+line)
			}
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// sanitizeSourceText strips HTML from source fields
// (MarkdownExport#sanitize_source_text); with preserveLineBreaks, br/p tags
// become newlines first.
func sanitizeSourceText(text string, preserveLineBreaks bool) string {
	if preserveLineBreaks {
		text = brTagRe.ReplaceAllString(text, "\n")
		text = pCloseTagRe.ReplaceAllString(text, "\n")
		text = pOpenTagRe.ReplaceAllString(text, "")
	}
	text = domain.PlainText(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.TrimSpace(text)
}

func sanitizeSourceURL(url string) string {
	first, _, _ := strings.Cut(sanitizeSourceText(url, false), "\n")
	return strings.TrimSpace(first)
}

var (
	brTagRe     = regexp.MustCompile(`(?i)<\s*br\s*/?>`)
	pCloseTagRe = regexp.MustCompile(`(?i)</\s*p\s*>`)
	pOpenTagRe  = regexp.MustCompile(`(?i)<\s*p[^>]*>`)
)

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

func rfc3339(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return time.Unix(v.Int64, 0).UTC().Format(time.RFC3339)
}

// writeMarkdownFile emits "---\n<yaml>---\n\n<body>\n"
// (MarkdownExport#write_markdown_file).
func writeMarkdownFile(dir, basename string, frontMatter any, body string) error {
	yml, err := yaml.Marshal(frontMatter)
	if err != nil {
		return fmt.Errorf("markdown front matter: %w", err)
	}
	content := "---\n" + string(yml) + "---\n\n" + body + "\n"
	return os.WriteFile(filepath.Join(dir, basename+".md"), []byte(content), 0o644)
}

// safeBasename ports MarkdownExport#safe_basename: strips characters that are
// unsafe or unusual in file names while keeping unicode letters/marks/digits.
func safeBasename(value string) string {
	value = strings.TrimSpace(value)
	value = unsafeNameCharsRe.ReplaceAllString(value, "_")
	value = nonWordCharsRe.ReplaceAllString(value, "_")
	value = strings.ReplaceAll(value, " ", "_")
	value = underscoresRe.ReplaceAllString(value, "_")
	value = strings.Trim(value, "_")
	value = strings.TrimLeft(value, ".")
	value = trailingDotsSpacesRe.ReplaceAllString(value, "")
	if value == "" {
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return "export"
		}
		return hex.EncodeToString(rnd[:])
	}
	return value
}

var (
	// [/\\:*?"<>|] and control characters.
	unsafeNameCharsRe = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1F]`)
	// Anything that is not a unicode letter/mark/number, underscore, dot,
	// dash or space.
	nonWordCharsRe       = regexp.MustCompile(`[^\p{L}\p{M}\p{N}_.\- ]+`)
	underscoresRe        = regexp.MustCompile(`_+`)
	trailingDotsSpacesRe = regexp.MustCompile(`[. ]+$`)
)

// dedupeBasename ports MarkdownExport#dedupe_basename: different slugs can
// normalize to the same basename, so a slug/id suffix is appended on
// collision.
func dedupeBasename(basename string, used map[string]bool, slug string, id int64) string {
	if !used[basename] {
		used[basename] = true
		return basename
	}
	for _, suffix := range []string{slug, fmt.Sprint(id)} {
		if suffix == "" {
			continue
		}
		candidate := basename + "-" + suffix
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err == nil {
		basename = basename + "-" + hex.EncodeToString(rnd[:])
	}
	used[basename] = true
	return basename
}
