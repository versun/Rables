// Package articles holds the article domain logic ported from
// app/models/article.rb: slug/excerpt syncing, validations, schedule
// snapshots, and the save-time side effects (schedule publication job,
// crosspost, newsletter, tag list, social media posts).
package articles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	tagsvc "rables/internal/service/tags"
)

// CrosspostPlatforms mirrors Article::CROSSPOST_PLATFORMS. The order drives
// snapshot serialization and crosspost iteration.
var CrosspostPlatforms = []string{"mastodon", "twitter", "bluesky", "xiaohongshu"}

// Job payloads. The keys mirror the Rails job arguments so later tasks
// (publish worker, crosspost worker) can rely on them.
type (

	// PublishArticlePayload is the kind=publish_article payload
	// (PublishScheduledArticlesJob arguments: article id).
	PublishArticlePayload struct {
		ArticleID int64 `json:"article_id"`
	}

	// CrosspostPayload is the kind=crosspost payload (CrosspostArticleJob
	// arguments: article id, platform, requested_at).
	CrosspostPayload struct {
		ArticleID   int64  `json:"article_id"`
		Platform    string `json:"platform"`
		RequestedAt int64  `json:"requested_at"`
	}

	// NewsletterPayload is the kind=send_newsletter payload
	// (NativeNewsletterSenderJob / ListmonkSenderJob argument; the provider
	// is resolved from newsletter_settings when the job runs).
	NewsletterPayload struct {
		ArticleID int64 `json:"article_id"`
	}

	// FetchCommentsPayload is the kind=fetch_social_comments payload for the
	// admin per-article fetch; ArticleID narrows the platform-wide cron
	// fetch (Scheduler uses {"platform": ...} only) to one article.
	FetchCommentsPayload struct {
		ArticleID int64  `json:"article_id,omitempty"`
		Platform  string `json:"platform"`
	}
)

// SaveParams carries the parsed article form (Admin::ArticlesController
// article_params). Crosspost and SocialURLs hold one entry per submitted
// platform checkbox / URL field; a missing key means the field was not part
// of the submission.
type SaveParams struct {
	Title           string
	Slug            string
	ContentType     string // domain.ContentTypeRichText | domain.ContentTypeHTML | domain.ContentTypeMarkdown
	ContentHTML     string // raw rich-text body, raw html_content, or markdown source
	Description     string
	MetaTitle       string
	MetaImage       string
	MetaDescription string
	SourceAuthor    string
	SourceURL       string
	SourceContent   string
	Status          domain.Status
	Comment         bool
	CreatedAt       time.Time  // zero mirrors article_params: blank => Time.current
	ScheduledAt     *time.Time // nil when the form field is blank
	SendNewsletter  bool
	Crosspost       map[string]bool
	TagList         string
	SocialURLs      map[string]string
	Now             time.Time // zero => time.Now
}

// ParseStatus maps the form status string to the domain enum; ok is false
// for unknown values.
func ParseStatus(s string) (domain.Status, bool) {
	switch s {
	case "draft":
		return domain.StatusDraft, true
	case "publish":
		return domain.StatusPublish, true
	case "schedule":
		return domain.StatusSchedule, true
	case "trash":
		return domain.StatusTrash, true
	case "shared":
		return domain.StatusShared, true
	}
	return 0, false
}

// SplitTagList mirrors the names_string.split(",") of
// Tag.find_or_create_by_names.
func SplitTagList(s string) []string {
	if domain.IsBlank(s) {
		return nil
	}
	return strings.Split(s, ",")
}

// ParseScheduledPlatforms ports normalize_crosspost_platforms for the stored
// JSON snapshot: known platforms only, deduped, in CROSSPOST_PLATFORMS order.
func ParseScheduledPlatforms(raw string) []string {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		// normalize_crosspost_platforms falls back to a comma split.
		values = strings.Split(raw, ",")
	}
	selected := map[string]bool{}
	for _, v := range values {
		selected[strings.TrimSpace(v)] = true
	}
	var out []string
	for _, platform := range CrosspostPlatforms {
		if selected[platform] {
			out = append(out, platform)
		}
	}
	return out
}

// Save creates (existing == nil) or updates an article, mirroring Article's
// before_validation chain (slug, excerpt, snapshot sync), validations, and
// after_save side effects (schedule_publication, handle_crosspost,
// handle_newsletter). It returns the Rails full-message validation failures;
// when they are non-empty nothing was written. All writes and job enqueues
// commit in one transaction, like the Rails save wrapped around after_save.
func Save(ctx context.Context, db *sql.DB, existing *query.Article, p SaveParams) (query.Article, []string, error) {
	q := query.New(db)
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	var excludeID int64
	if existing != nil {
		excludeID = existing.ID
	}
	exists := func(candidate string) bool {
		n, err := q.CountAdminArticlesBySlug(ctx, query.CountAdminArticlesBySlugParams{
			Slug: sql.NullString{String: candidate, Valid: true},
			ID:   excludeID,
		})
		return err == nil && n > 0
	}

	slug := domain.GenerateSlug(p.Slug, p.Title, now, exists)
	// Markdown articles store the source in content_markdown and the rendered
	// HTML in content_html, so the sanitize/lazy-load write path and every
	// content_html reader (public page, feed, newsletter, crosspost) treat
	// them exactly like rich-text articles.
	isMarkdown := p.ContentType == string(domain.ContentTypeMarkdown)
	body := p.ContentHTML
	if isMarkdown {
		body = domain.RenderMarkdown(body)
	}
	contentHTML := domain.AddLazyLoading(domain.SanitizeHTML(body))
	excerpt := domain.BuildExcerpt(p.Description, contentHTML)

	var errs []string
	if exists(slug) {
		errs = append(errs, "Slug has already been taken")
	}
	if domain.IsReservedSlug(slug) {
		errs = append(errs, "Slug is reserved")
	}
	if p.Status == domain.StatusSchedule && p.ScheduledAt == nil {
		errs = append(errs, "Scheduled at can't be blank")
	}
	switch {
	case p.ContentType == string(domain.ContentTypeHTML):
		if domain.IsBlank(p.ContentHTML) {
			errs = append(errs, "Html content can't be blank")
		}
	case isMarkdown:
		if domain.IsBlank(p.ContentHTML) {
			errs = append(errs, "Content can't be blank")
		}
	case domain.IsBlank(domain.PlainText(contentHTML)):
		errs = append(errs, "Content can't be blank")
	}
	if len(errs) > 0 {
		return query.Article{}, errs, nil
	}

	// sync_scheduled_crosspost_platforms / sync_scheduled_newsletter_selection.
	platformsJSON := "[]"
	var sendNewsletter int64
	if p.Status == domain.StatusSchedule {
		platforms := SelectedPlatforms(p.Crosspost)
		b, err := json.Marshal(platforms)
		if err != nil {
			return query.Article{}, nil, err
		}
		platformsJSON = string(b)
		if p.SendNewsletter {
			sendNewsletter = 1
		}
	}

	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	var scheduledAt sql.NullInt64
	if p.ScheduledAt != nil {
		scheduledAt = sql.NullInt64{Int64: p.ScheduledAt.UTC().Unix(), Valid: true}
	}
	comment := int64(0)
	if p.Comment {
		comment = 1
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return query.Article{}, nil, err
	}
	defer tx.Rollback()
	qtx := q.WithTx(tx)
	nowUnix := now.Unix()

	// Markdown source is kept only while the article is markdown; switching
	// to another content type clears it.
	var contentMarkdown sql.NullString
	if isMarkdown {
		contentMarkdown = nullString(p.ContentHTML)
	}

	columns := query.CreateArticleParams{
		Title:                       nullString(p.Title),
		Slug:                        nullString(slug),
		ContentHtml:                 nullString(contentHTML),
		ContentType:                 contentTypeOrDefault(p.ContentType),
		ContentMarkdown:             contentMarkdown,
		Description:                 nullString(p.Description),
		Excerpt:                     nullString(excerpt),
		MetaDescription:             nullString(p.MetaDescription),
		MetaTitle:                   nullString(p.MetaTitle),
		MetaImage:                   nullString(p.MetaImage),
		SourceAuthor:                nullString(p.SourceAuthor),
		SourceUrl:                   nullString(p.SourceURL),
		SourceContent:               nullString(p.SourceContent),
		Status:                      int64(p.Status),
		Comment:                     comment,
		ScheduledAt:                 scheduledAt,
		ScheduledCrosspostPlatforms: platformsJSON,
		ScheduledSendNewsletter:     sendNewsletter,
		CreatedAt:                   createdAt.Unix(),
		UpdatedAt:                   nowUnix,
	}
	var article query.Article
	if existing == nil {
		article, err = qtx.CreateArticle(ctx, columns)
	} else {
		article, err = qtx.UpdateArticle(ctx, query.UpdateArticleParams{
			Title:                       columns.Title,
			Slug:                        columns.Slug,
			ContentHtml:                 columns.ContentHtml,
			ContentType:                 columns.ContentType,
			ContentMarkdown:             columns.ContentMarkdown,
			Description:                 columns.Description,
			Excerpt:                     columns.Excerpt,
			MetaDescription:             columns.MetaDescription,
			MetaTitle:                   columns.MetaTitle,
			MetaImage:                   columns.MetaImage,
			SourceAuthor:                columns.SourceAuthor,
			SourceUrl:                   columns.SourceUrl,
			SourceContent:               columns.SourceContent,
			Status:                      columns.Status,
			Comment:                     columns.Comment,
			ScheduledAt:                 columns.ScheduledAt,
			ScheduledCrosspostPlatforms: columns.ScheduledCrosspostPlatforms,
			ScheduledSendNewsletter:     columns.ScheduledSendNewsletter,
			CreatedAt:                   columns.CreatedAt,
			UpdatedAt:                   columns.UpdatedAt,
			ID:                          existing.ID,
		})
	}
	if err != nil {
		if isUniqueViolation(err) {
			// Lost a slug race; report it like the uniqueness validation.
			return query.Article{}, []string{"Slug has already been taken"}, nil
		}
		return query.Article{}, nil, err
	}

	// tag_list=: reset the join rows from the submitted list.
	tagIDs, err := tagsvc.FindOrCreateByNames(ctx, qtx, SplitTagList(p.TagList))
	if err != nil {
		return query.Article{}, nil, err
	}
	if err := qtx.DeleteArticleTagsByArticleID(ctx, article.ID); err != nil {
		return query.Article{}, nil, err
	}
	for _, tagID := range tagIDs {
		if err := qtx.InsertArticleTag(ctx, query.InsertArticleTagParams{
			ArticleID: article.ID, TagID: tagID, CreatedAt: nowUnix, UpdatedAt: nowUnix,
		}); err != nil {
			return query.Article{}, nil, err
		}
	}

	// social_media_posts nested attributes: blank url destroys the row
	// (cleanup_empty_social_media_posts), otherwise upsert.
	for _, platform := range CrosspostPlatforms {
		url, ok := p.SocialURLs[platform]
		if !ok {
			continue
		}
		if domain.IsBlank(url) {
			err = qtx.DeleteSocialMediaPost(ctx, query.DeleteSocialMediaPostParams{ArticleID: article.ID, Platform: platform})
		} else {
			err = qtx.UpsertSocialMediaPost(ctx, query.UpsertSocialMediaPostParams{
				ArticleID: article.ID, Platform: platform, Url: strings.TrimSpace(url),
				CreatedAt: nowUnix, UpdatedAt: nowUnix,
			})
		}
		if err != nil {
			return query.Article{}, nil, err
		}
	}

	// after_save :schedule_publication — cancel_old_jobs then set(wait_until:).
	if p.Status == domain.StatusSchedule && p.ScheduledAt != nil {
		if err := CancelQueuedPublishJobs(ctx, qtx, article.ID); err != nil {
			return query.Article{}, nil, err
		}
		if err := enqueueTx(ctx, qtx, now, jobs.KindPublishArticle, PublishArticlePayload{ArticleID: article.ID}, *p.ScheduledAt); err != nil {
			return query.Article{}, nil, err
		}
	}
	// after_save :handle_crosspost / :handle_newsletter.
	if p.Status == domain.StatusPublish {
		if err := EnqueuePublishEffects(ctx, qtx, article.ID, p.Crosspost, p.SendNewsletter, now); err != nil {
			return query.Article{}, nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return query.Article{}, nil, err
	}
	return article, nil, nil
}

// TransitionStatus ports the status-only updates behind the publish /
// unpublish / destroy (to trash) controller actions: update(status: ...) with
// no other attributes. The schedule snapshots are cleared (sync callbacks),
// and publishing a scheduled article restores the snapshot selections so
// handle_crosspost / handle_newsletter fire for them.
func TransitionStatus(ctx context.Context, db *sql.DB, id int64, target domain.Status, now time.Time) (query.Article, error) {
	q := query.New(db)
	existing, err := q.GetAdminArticleByID(ctx, id)
	if err != nil {
		return query.Article{}, err
	}
	now = now.UTC()

	crosspost := map[string]bool{}
	sendNewsletter := false
	if target == domain.StatusPublish && domain.Status(existing.Status) == domain.StatusSchedule {
		for _, platform := range ParseScheduledPlatforms(existing.ScheduledCrosspostPlatforms) {
			crosspost[platform] = true
		}
		sendNewsletter = existing.ScheduledSendNewsletter == 1
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return query.Article{}, err
	}
	defer tx.Rollback()
	qtx := q.WithTx(tx)

	article, err := qtx.UpdateArticleStatus(ctx, query.UpdateArticleStatusParams{
		Status:                      int64(target),
		ScheduledCrosspostPlatforms: "[]",
		ScheduledSendNewsletter:     0,
		UpdatedAt:                   now.Unix(),
		ID:                          id,
	})
	if err != nil {
		return query.Article{}, err
	}
	if target == domain.StatusPublish {
		if err := EnqueuePublishEffects(ctx, qtx, id, crosspost, sendNewsletter, now); err != nil {
			return query.Article{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return query.Article{}, err
	}
	return article, nil
}

// Destroy removes the article with its dependent rows (dependent: :destroy on
// comments, article_tags, social_media_posts) and drops any queued
// publish_article job for it.
func Destroy(ctx context.Context, db *sql.DB, id int64) error {
	q := query.New(db)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := q.WithTx(tx)

	if err := qtx.DeleteCommentsForArticle(ctx, query.DeleteCommentsForArticleParams{
		CommentableID: sql.NullInt64{Int64: id, Valid: true},
		ArticleID:     sql.NullInt64{Int64: id, Valid: true},
	}); err != nil {
		return err
	}
	if err := qtx.DeleteArticleTagsByArticleID(ctx, id); err != nil {
		return err
	}
	if err := qtx.DeleteSocialMediaPostsByArticleID(ctx, id); err != nil {
		return err
	}
	if err := CancelQueuedPublishJobs(ctx, qtx, id); err != nil {
		return err
	}
	if err := qtx.DeleteArticle(ctx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CancelQueuedPublishJobs drops queued publish_article rows for the article
// (PublishScheduledArticlesJob.cancel_old_jobs).
func CancelQueuedPublishJobs(ctx context.Context, q *query.Queries, articleID int64) error {
	runs, err := q.ListQueuedJobRunsByKind(ctx, jobs.KindPublishArticle)
	if err != nil {
		return err
	}
	for _, run := range runs {
		var payload PublishArticlePayload
		if !run.Payload.Valid || json.Unmarshal([]byte(run.Payload.String), &payload) != nil {
			continue
		}
		if payload.ArticleID == articleID {
			if err := q.DeleteJobRun(ctx, run.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// SelectedPlatforms normalizes the checkbox map to CROSSPOST_PLATFORMS order.
func SelectedPlatforms(crosspost map[string]bool) []string {
	out := []string{}
	for _, platform := range CrosspostPlatforms {
		if crosspost[platform] {
			out = append(out, platform)
		}
	}
	return out
}

// EnqueuePublishEffects ports handle_crosspost and handle_newsletter: one
// crosspost job per selected, enabled platform (xiaohongshu is log-only in
// Rails and never enqueued), and one send_newsletter job when the newsletter
// is selected and ready.
func EnqueuePublishEffects(ctx context.Context, q *query.Queries, articleID int64, crosspost map[string]bool, sendNewsletter bool, now time.Time) error {
	for _, platform := range CrosspostPlatforms {
		if !crosspost[platform] {
			continue
		}
		enabled, err := CrosspostEnabled(ctx, q, platform)
		if err != nil {
			return err
		}
		if !enabled || platform == "xiaohongshu" {
			continue
		}
		if err := enqueueTx(ctx, q, now, jobs.KindCrosspost, CrosspostPayload{
			ArticleID: articleID, Platform: platform, RequestedAt: now.Unix(),
		}, now); err != nil {
			return err
		}
	}
	if sendNewsletter {
		ready, err := NewsletterReady(ctx, q)
		if err != nil {
			return err
		}
		if ready {
			if err := enqueueTx(ctx, q, now, jobs.KindSendNewsletter, NewsletterPayload{ArticleID: articleID}, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// CrosspostEnabled mirrors Crosspost.find_by(platform:)&.enabled?.
func CrosspostEnabled(ctx context.Context, q *query.Queries, platform string) (bool, error) {
	row, err := q.GetCrosspostByPlatform(ctx, platform)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return row.Enabled == 1, nil
}

// NewsletterReady mirrors NewsletterSetting#enabled? && #configured?.
func NewsletterReady(ctx context.Context, q *query.Queries) (bool, error) {
	ns, err := q.GetNewsletterSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ns.Enabled != 1 {
		return false, nil
	}
	if ns.Provider == "listmonk" {
		lm, err := q.GetListmonk(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return present(lm.ApiKey) && present(lm.Username) && present(lm.Url), nil
	}
	return present(ns.SmtpAddress) && ns.SmtpPort.Valid &&
		present(ns.SmtpUserName) && present(ns.SmtpPassword) && present(ns.FromEmail), nil
}

// NewsletterEnabled mirrors NewsletterSetting.instance.enabled? (a missing
// row is a new, disabled instance).
func NewsletterEnabled(ctx context.Context, q *query.Queries) (bool, error) {
	ns, err := q.GetNewsletterSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ns.Enabled == 1, nil
}

// enqueueTx inserts a queued job_runs row inside the article transaction,
// mirroring an after_save perform_later.
func enqueueTx(ctx context.Context, q *query.Queries, now time.Time, kind string, payload any, runAt time.Time) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = q.EnqueueJobRun(ctx, query.EnqueueJobRunParams{
		Kind:      kind,
		Payload:   sql.NullString{String: string(b), Valid: true},
		RunAt:     runAt.UTC().Unix(),
		CreatedAt: now.Unix(),
		UpdatedAt: now.Unix(),
	})
	return err
}

func present(s sql.NullString) bool { return s.Valid && s.String != "" }

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: !domain.IsBlank(s)}
}

func contentTypeOrDefault(ct string) string {
	switch ct {
	case string(domain.ContentTypeHTML):
		return string(domain.ContentTypeHTML)
	case string(domain.ContentTypeMarkdown):
		return string(domain.ContentTypeMarkdown)
	}
	return string(domain.ContentTypeRichText)
}

// isUniqueViolation reports a SQLite UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
