// Package transfer implements data export (plan §4.11): a ZIP of CSV table
// dumps plus attachment file contents, and a Markdown export of articles and
// pages.
//
// The ZIP layout is the canonical format the ZIP import (T26) consumes —
// file names and column order are fixed, do not reorder:
//
//	articles.csv:            id,title,slug,content_html,content_type,description,excerpt,meta_description,meta_title,meta_image,source_author,source_url,source_content,status,comment,scheduled_at,scheduled_crosspost_platforms,scheduled_send_newsletter,created_at,updated_at
//	pages.csv:               id,title,slug,content_html,content_type,redirect_url,page_order,status,comment,scheduled_at,created_at,updated_at
//	tags.csv:                id,name,slug,created_at,updated_at
//	comments.csv:            id,commentable_type,commentable_id,article_id,article_slug,parent_id,author_name,author_email,author_url,author_username,author_avatar_url,content,status,platform,external_id,url,published_at,created_at,updated_at
//	settings.csv:            id,title,description,author,url,time_zone,head_code,custom_css,tool_code,giscus,social_links,setup_completed,created_at,updated_at
//	crossposts.csv:          id,platform,enabled,api_key,api_key_secret,access_token,access_token_secret,client_id,client_key,client_secret,app_password,refresh_token,token_expires_at,server_url,username,max_characters,auto_fetch_comments,comment_fetch_schedule,settings,created_at,updated_at
//	listmonks.csv:           id,url,username,api_key,list_id,template_id,enabled,created_at,updated_at
//	social_media_posts.csv:  id,article_id,article_slug,platform,url,created_at,updated_at
//	static_files.csv:        id,filename,file_id,blob_filename,description,created_at,updated_at
//	redirects.csv:           id,regex,replacement,enabled,permanent,created_at,updated_at
//	newsletter_settings.csv: id,enabled,provider,from_email,smtp_address,smtp_port,smtp_user_name,smtp_password,smtp_domain,smtp_authentication,smtp_enable_starttls,created_at,updated_at
//	subscribers.csv:         id,email,confirmed_at,unsubscribed_at,created_at,updated_at
//	article_tags.csv:        id,article_id,article_slug,tag_id,tag_name,tag_slug,created_at,updated_at
//	subscriber_tags.csv:     id,subscriber_id,subscriber_email,tag_id,tag_name,tag_slug,created_at,updated_at
//	files.csv:               id,key,filename,content_type,byte_size,checksum,variant_of,created_at
//	attachments.csv:         id,file_id,record_type,record_id,name,created_at
//	attachments/files/<file_id>_<filename>: raw content of each files row
//
// Values follow the Go schema: NULL -> empty field, timestamps are unix
// seconds (UTC), booleans are 0/1, JSON columns are stored verbatim.
package transfer

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"rables/internal/db/query"
	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/media"
)

// RedactedValue replaces sensitive credentials in the CSVs unless the export
// explicitly keeps them (ExportDataJob / Export::REDACTED_VALUE).
const RedactedValue = "[REDACTED]"

// ExportPayload is the job_runs payload for kind "export".
type ExportPayload struct {
	// Format is "default"/"zip" (CSV bundle) or "markdown".
	Format string `json:"format"`
	// KeepCredentials disables redaction of sensitive credentials.
	KeepCredentials bool `json:"keep_credentials"`
}

// ZipExporter dumps every table to CSV plus the file contents into a zip
// under <DataDir>/exports (Export / Exports::ZipPackaging).
type ZipExporter struct {
	DB              *sql.DB
	DataDir         string
	KeepCredentials bool

	q *query.Queries
}

// Generate writes the export zip and returns its path.
func (e *ZipExporter) Generate(ctx context.Context) (string, error) {
	e.q = query.New(e.DB)
	stage, err := stagingDir(e.DataDir, "export")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)

	writers := []func(context.Context, string) error{
		e.exportArticles, e.exportPages, e.exportTags, e.exportComments,
		e.exportSettings, e.exportCrossposts, e.exportListmonks,
		e.exportSocialMediaPosts, e.exportStaticFiles, e.exportRedirects,
		e.exportNewsletterSettings, e.exportSubscribers, e.exportArticleTags,
		e.exportSubscriberTags, e.exportFiles, e.exportAttachments,
		e.exportFileContents,
	}
	for _, w := range writers {
		if err := w(ctx, stage); err != nil {
			return "", err
		}
	}
	return zipStaging(stage)
}

// stagingDir creates <dataDir>/exports/<prefix>_<ts>_<rand> for assembly.
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

// zipStaging packs the staging dir into <stage>.zip (entries sorted, relative
// slash paths) mirroring Exports::ZipPackaging#create_zip_file.
func zipStaging(stage string) (string, error) {
	zipPath := stage + ".zip"
	out, err := os.Create(zipPath)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(out)

	var files []string
	err = filepath.WalkDir(stage, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		zw.Close()
		out.Close()
		os.Remove(zipPath)
		return "", err
	}
	sort.Strings(files)
	for _, file := range files {
		rel, err := filepath.Rel(stage, file)
		if err != nil {
			break
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			break
		}
		f, err := os.Open(file)
		if err != nil {
			break
		}
		_, copyErr := io.Copy(w, f)
		f.Close()
		if copyErr != nil {
			err = copyErr
			break
		}
	}
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(zipPath)
		return "", err
	}
	return zipPath, nil
}

// writeCSV writes one CSV with the given fixed header.
func writeCSV(dir, name string, header []string, rows [][]string) error {
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return fmt.Errorf("export %s: %w", name, err)
	}
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		f.Close()
		return fmt.Errorf("export %s: %w", name, err)
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			f.Close()
			return fmt.Errorf("export %s: %w", name, err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return fmt.Errorf("export %s: %w", name, err)
	}
	return f.Close()
}

// Value formatters: NULL -> "", timestamps stay unix seconds.

func ns(v sql.NullString) string { return v.String }

func ni(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(v.Int64, 10)
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }

// redact mirrors Export#redact_secret: non-empty secrets become [REDACTED]
// unless credentials are kept.
func (e *ZipExporter) redact(v sql.NullString) string {
	if e.KeepCredentials || !v.Valid || v.String == "" {
		return v.String
	}
	return RedactedValue
}

func (e *ZipExporter) exportArticles(ctx context.Context, dir string) error {
	rows, err := e.q.ExportArticles(ctx)
	if err != nil {
		return fmt.Errorf("export articles: %w", err)
	}
	header := []string{"id", "title", "slug", "content_html", "content_type", "description", "excerpt", "meta_description", "meta_title", "meta_image", "source_author", "source_url", "source_content", "status", "comment", "scheduled_at", "scheduled_crosspost_platforms", "scheduled_send_newsletter", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, a := range rows {
		data = append(data, []string{
			i64(a.ID), ns(a.Title), ns(a.Slug), ns(a.ContentHtml), a.ContentType,
			ns(a.Description), ns(a.Excerpt), ns(a.MetaDescription), ns(a.MetaTitle), ns(a.MetaImage),
			ns(a.SourceAuthor), ns(a.SourceUrl), ns(a.SourceContent),
			i64(a.Status), i64(a.Comment), ni(a.ScheduledAt),
			a.ScheduledCrosspostPlatforms, i64(a.ScheduledSendNewsletter),
			i64(a.CreatedAt), i64(a.UpdatedAt),
		})
	}
	return writeCSV(dir, "articles.csv", header, data)
}

func (e *ZipExporter) exportPages(ctx context.Context, dir string) error {
	rows, err := e.q.ExportPages(ctx)
	if err != nil {
		return fmt.Errorf("export pages: %w", err)
	}
	header := []string{"id", "title", "slug", "content_html", "content_type", "redirect_url", "page_order", "status", "comment", "scheduled_at", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, p := range rows {
		data = append(data, []string{
			i64(p.ID), ns(p.Title), ns(p.Slug), ns(p.ContentHtml), p.ContentType,
			ns(p.RedirectUrl), i64(p.PageOrder), i64(p.Status), i64(p.Comment), ni(p.ScheduledAt),
			i64(p.CreatedAt), i64(p.UpdatedAt),
		})
	}
	return writeCSV(dir, "pages.csv", header, data)
}

func (e *ZipExporter) exportTags(ctx context.Context, dir string) error {
	rows, err := e.q.ExportTags(ctx)
	if err != nil {
		return fmt.Errorf("export tags: %w", err)
	}
	header := []string{"id", "name", "slug", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, t := range rows {
		data = append(data, []string{i64(t.ID), t.Name, t.Slug, i64(t.CreatedAt), i64(t.UpdatedAt)})
	}
	return writeCSV(dir, "tags.csv", header, data)
}

func (e *ZipExporter) exportComments(ctx context.Context, dir string) error {
	rows, err := e.q.ExportComments(ctx)
	if err != nil {
		return fmt.Errorf("export comments: %w", err)
	}
	header := []string{"id", "commentable_type", "commentable_id", "article_id", "article_slug", "parent_id", "author_name", "author_email", "author_url", "author_username", "author_avatar_url", "content", "status", "platform", "external_id", "url", "published_at", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, c := range rows {
		data = append(data, []string{
			i64(c.ID), ns(c.CommentableType), ni(c.CommentableID), ni(c.ArticleID), ns(c.ArticleSlug), ni(c.ParentID),
			c.AuthorName, ns(c.AuthorEmail), ns(c.AuthorUrl), ns(c.AuthorUsername), ns(c.AuthorAvatarUrl),
			c.Content, i64(c.Status), ns(c.Platform), ns(c.ExternalID), ns(c.Url), ni(c.PublishedAt),
			i64(c.CreatedAt), i64(c.UpdatedAt),
		})
	}
	return writeCSV(dir, "comments.csv", header, data)
}

func (e *ZipExporter) exportSettings(ctx context.Context, dir string) error {
	rows, err := e.q.ExportSettings(ctx)
	if err != nil {
		return fmt.Errorf("export settings: %w", err)
	}
	header := []string{"id", "title", "description", "author", "url", "time_zone", "head_code", "custom_css", "tool_code", "giscus", "social_links", "setup_completed", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, st := range rows {
		data = append(data, []string{
			i64(st.ID), ns(st.Title), ns(st.Description), ns(st.Author), ns(st.Url), st.TimeZone,
			ns(st.HeadCode), ns(st.CustomCss), ns(st.ToolCode), ns(st.Giscus), ns(st.SocialLinks),
			i64(st.SetupCompleted), i64(st.CreatedAt), i64(st.UpdatedAt),
		})
	}
	return writeCSV(dir, "settings.csv", header, data)
}

func (e *ZipExporter) exportCrossposts(ctx context.Context, dir string) error {
	rows, err := e.q.ExportCrossposts(ctx)
	if err != nil {
		return fmt.Errorf("export crossposts: %w", err)
	}
	header := []string{"id", "platform", "enabled", "api_key", "api_key_secret", "access_token", "access_token_secret", "client_id", "client_key", "client_secret", "app_password", "refresh_token", "token_expires_at", "server_url", "username", "max_characters", "auto_fetch_comments", "comment_fetch_schedule", "settings", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, c := range rows {
		data = append(data, []string{
			i64(c.ID), c.Platform, i64(c.Enabled),
			e.redact(c.ApiKey), e.redact(c.ApiKeySecret), e.redact(c.AccessToken), e.redact(c.AccessTokenSecret),
			ns(c.ClientID), e.redact(c.ClientKey), e.redact(c.ClientSecret),
			e.redact(c.AppPassword), e.redact(c.RefreshToken), ni(c.TokenExpiresAt),
			ns(c.ServerUrl), ns(c.Username), ni(c.MaxCharacters),
			i64(c.AutoFetchComments), ns(c.CommentFetchSchedule), ns(c.Settings),
			i64(c.CreatedAt), i64(c.UpdatedAt),
		})
	}
	return writeCSV(dir, "crossposts.csv", header, data)
}

func (e *ZipExporter) exportListmonks(ctx context.Context, dir string) error {
	rows, err := e.q.ExportListmonks(ctx)
	if err != nil {
		return fmt.Errorf("export listmonks: %w", err)
	}
	header := []string{"id", "url", "username", "api_key", "list_id", "template_id", "enabled", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, l := range rows {
		data = append(data, []string{
			i64(l.ID), ns(l.Url), ns(l.Username), e.redact(l.ApiKey), ni(l.ListID), ni(l.TemplateID),
			i64(l.Enabled), i64(l.CreatedAt), i64(l.UpdatedAt),
		})
	}
	return writeCSV(dir, "listmonks.csv", header, data)
}

func (e *ZipExporter) exportSocialMediaPosts(ctx context.Context, dir string) error {
	rows, err := e.q.ExportSocialMediaPosts(ctx)
	if err != nil {
		return fmt.Errorf("export social media posts: %w", err)
	}
	header := []string{"id", "article_id", "article_slug", "platform", "url", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, p := range rows {
		data = append(data, []string{
			i64(p.ID), i64(p.ArticleID), ns(p.ArticleSlug), p.Platform, p.Url, i64(p.CreatedAt), i64(p.UpdatedAt),
		})
	}
	return writeCSV(dir, "social_media_posts.csv", header, data)
}

func (e *ZipExporter) exportStaticFiles(ctx context.Context, dir string) error {
	rows, err := e.q.ExportStaticFiles(ctx)
	if err != nil {
		return fmt.Errorf("export static files: %w", err)
	}
	header := []string{"id", "filename", "file_id", "blob_filename", "description", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, sf := range rows {
		data = append(data, []string{
			i64(sf.ID), sf.Filename, i64(sf.FileID), ns(sf.BlobFilename), ns(sf.Description),
			i64(sf.CreatedAt), i64(sf.UpdatedAt),
		})
	}
	return writeCSV(dir, "static_files.csv", header, data)
}

func (e *ZipExporter) exportRedirects(ctx context.Context, dir string) error {
	rows, err := e.q.ExportRedirects(ctx)
	if err != nil {
		return fmt.Errorf("export redirects: %w", err)
	}
	header := []string{"id", "regex", "replacement", "enabled", "permanent", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, rd := range rows {
		data = append(data, []string{
			i64(rd.ID), rd.Regex, rd.Replacement, i64(rd.Enabled), i64(rd.Permanent), i64(rd.CreatedAt), i64(rd.UpdatedAt),
		})
	}
	return writeCSV(dir, "redirects.csv", header, data)
}

func (e *ZipExporter) exportNewsletterSettings(ctx context.Context, dir string) error {
	rows, err := e.q.ExportNewsletterSettings(ctx)
	if err != nil {
		return fmt.Errorf("export newsletter settings: %w", err)
	}
	header := []string{"id", "enabled", "provider", "from_email", "smtp_address", "smtp_port", "smtp_user_name", "smtp_password", "smtp_domain", "smtp_authentication", "smtp_enable_starttls", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, n := range rows {
		data = append(data, []string{
			i64(n.ID), i64(n.Enabled), n.Provider, ns(n.FromEmail),
			ns(n.SmtpAddress), ni(n.SmtpPort), ns(n.SmtpUserName), e.redact(n.SmtpPassword),
			ns(n.SmtpDomain), ns(n.SmtpAuthentication), ni(n.SmtpEnableStarttls),
			i64(n.CreatedAt), i64(n.UpdatedAt),
		})
	}
	return writeCSV(dir, "newsletter_settings.csv", header, data)
}

// exportSubscribers never exports the confirm/unsubscribe tokens: they
// authenticate subscriber links (Export#export_subscribers).
func (e *ZipExporter) exportSubscribers(ctx context.Context, dir string) error {
	rows, err := e.q.ExportSubscribers(ctx)
	if err != nil {
		return fmt.Errorf("export subscribers: %w", err)
	}
	header := []string{"id", "email", "confirmed_at", "unsubscribed_at", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, sub := range rows {
		data = append(data, []string{
			i64(sub.ID), sub.Email, ni(sub.ConfirmedAt), ni(sub.UnsubscribedAt), i64(sub.CreatedAt), i64(sub.UpdatedAt),
		})
	}
	return writeCSV(dir, "subscribers.csv", header, data)
}

func (e *ZipExporter) exportArticleTags(ctx context.Context, dir string) error {
	rows, err := e.q.ExportArticleTags(ctx)
	if err != nil {
		return fmt.Errorf("export article tags: %w", err)
	}
	header := []string{"id", "article_id", "article_slug", "tag_id", "tag_name", "tag_slug", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, at := range rows {
		data = append(data, []string{
			i64(at.ID), i64(at.ArticleID), ns(at.ArticleSlug), i64(at.TagID), ns(at.TagName), ns(at.TagSlug),
			i64(at.CreatedAt), i64(at.UpdatedAt),
		})
	}
	return writeCSV(dir, "article_tags.csv", header, data)
}

func (e *ZipExporter) exportSubscriberTags(ctx context.Context, dir string) error {
	rows, err := e.q.ExportSubscriberTags(ctx)
	if err != nil {
		return fmt.Errorf("export subscriber tags: %w", err)
	}
	header := []string{"id", "subscriber_id", "subscriber_email", "tag_id", "tag_name", "tag_slug", "created_at", "updated_at"}
	data := make([][]string, 0, len(rows))
	for _, st := range rows {
		data = append(data, []string{
			i64(st.ID), i64(st.SubscriberID), ns(st.SubscriberEmail), i64(st.TagID), ns(st.TagName), ns(st.TagSlug),
			i64(st.CreatedAt), i64(st.UpdatedAt),
		})
	}
	return writeCSV(dir, "subscriber_tags.csv", header, data)
}

func (e *ZipExporter) exportFiles(ctx context.Context, dir string) error {
	rows, err := e.q.ExportFiles(ctx)
	if err != nil {
		return fmt.Errorf("export files: %w", err)
	}
	header := []string{"id", "key", "filename", "content_type", "byte_size", "checksum", "variant_of", "created_at"}
	data := make([][]string, 0, len(rows))
	for _, f := range rows {
		data = append(data, []string{
			i64(f.ID), f.Key, f.Filename, ns(f.ContentType), i64(f.ByteSize), ns(f.Checksum), ni(f.VariantOf), i64(f.CreatedAt),
		})
	}
	return writeCSV(dir, "files.csv", header, data)
}

func (e *ZipExporter) exportAttachments(ctx context.Context, dir string) error {
	rows, err := e.q.ExportAttachments(ctx)
	if err != nil {
		return fmt.Errorf("export attachments: %w", err)
	}
	header := []string{"id", "file_id", "record_type", "record_id", "name", "created_at"}
	data := make([][]string, 0, len(rows))
	for _, a := range rows {
		data = append(data, []string{
			i64(a.ID), i64(a.FileID), a.RecordType, i64(a.RecordID), a.Name, i64(a.CreatedAt),
		})
	}
	return writeCSV(dir, "attachments.csv", header, data)
}

// exportFileContents copies each files row's disk content into
// attachments/files/<id>_<filename>. Rows whose blob is missing on disk are
// skipped with a warn log, mirroring the Rails per-file rescue.
func (e *ZipExporter) exportFileContents(ctx context.Context, dir string) error {
	rows, err := e.q.ExportFiles(ctx)
	if err != nil {
		return fmt.Errorf("export file contents: %w", err)
	}
	outDir := filepath.Join(dir, "attachments", "files")
	for _, f := range rows {
		if !media.ValidKey(f.Key) {
			activity.Log(ctx, e.DB, "warn", "export_file_skipped", "export", fmt.Sprintf("file_id=%d invalid key", f.ID))
			continue
		}
		src := filepath.Join(e.DataDir, "files", f.Key[0:2], f.Key[2:4], f.Key)
		in, err := os.Open(src)
		if err != nil {
			activity.Log(ctx, e.DB, "warn", "export_file_skipped", "export", fmt.Sprintf("file_id=%d missing blob", f.ID))
			continue
		}
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			in.Close()
			return fmt.Errorf("export file contents: %w", err)
		}
		name := fmt.Sprintf("%d_%s", f.ID, filepath.Base(f.Filename))
		out, err := os.Create(filepath.Join(outDir, name))
		if err != nil {
			in.Close()
			return fmt.Errorf("export file contents: %w", err)
		}
		_, copyErr := io.Copy(out, in)
		in.Close()
		if err := out.Close(); copyErr == nil {
			copyErr = err
		}
		if copyErr != nil {
			return fmt.Errorf("export file contents: %w", copyErr)
		}
	}
	return nil
}

// RegisterExportHandlers installs the kind "export" job handler
// (ExportDataJob): it runs the requested exporter into <dataDir>/exports and
// logs the completed/failed activity.
func RegisterExportHandlers(w *jobs.Worker, db *sql.DB, dataDir string) {
	w.Register(jobs.KindExport, func(ctx context.Context, payload json.RawMessage) error {
		var p ExportPayload
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &p); err != nil {
				return fmt.Errorf("export: decode payload: %w", err)
			}
		}

		var path string
		var err error
		switch p.Format {
		case "", "default", "zip":
			path, err = (&ZipExporter{DB: db, DataDir: dataDir, KeepCredentials: p.KeepCredentials}).Generate(ctx)
		case "markdown":
			path, err = (&MarkdownExporter{DB: db, DataDir: dataDir}).Generate(ctx)
		default:
			err = fmt.Errorf("export: unsupported format %q", p.Format)
		}
		if err != nil {
			activity.Log(ctx, db, "error", "failed", "export", fmt.Sprintf("format=%s error=%s", p.Format, activity.Quote(err.Error())))
			return err
		}
		activity.Log(ctx, db, "info", "completed", "export", fmt.Sprintf("format=%s file=%s", p.Format, activity.Quote(path)))
		return nil
	})
}
