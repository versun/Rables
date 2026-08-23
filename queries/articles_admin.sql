-- Admin article management (plan section 4.1). Feature-local lookups are
-- named independently of other query files on purpose.

-- name: GetAdminArticleBySlug :one
SELECT * FROM articles WHERE slug = ?;

-- name: GetAdminArticleByID :one
SELECT * FROM articles WHERE id = ?;

-- Slug-uniqueness probe (excludeID mirrors where.not(id: id || 0)).
-- name: CountAdminArticlesBySlug :one
SELECT COUNT(*) FROM articles WHERE slug = ? AND id != ?;

-- Admin list (fetch_articles), 100 per page. The optional filters are shared
-- by all variants: status_filter -1 lists every status, an empty tag lists
-- every tag (the filter matches on the tag name), untagged 1 narrows to
-- articles with no tags at all (the caller's "none" filter choice; combine it
-- with an empty tag), an empty search_like skips
-- Article.search_content (LIKE on title/slug/description/content_html with
-- ESCAPE '\', written in the equivalent like(pattern, string, escape)
-- function form; the caller pre-escapes %, _ and backslash, then wraps the
-- term in %...%, sanitize_sql_like semantics). sqlc cannot parameterize the
-- ORDER BY column or direction, so the four sort combinations the admin list
-- offers (created_at/updated_at x asc/desc) are separate queries; the default
-- is created_at DESC. id breaks sort-key ties so pagination is stable. The
-- CASTs pin the reused filter params' Go types (without them sqlc falls back
-- to interface{}).
-- name: ListAdminArticlesFilteredCreatedDesc :many
SELECT * FROM articles
WHERE (CAST(sqlc.arg(status_filter) AS INTEGER) = -1 OR status = CAST(sqlc.arg(status_filter) AS INTEGER))
  AND (CAST(sqlc.arg(search_like) AS TEXT) = '' OR like(CAST(sqlc.arg(search_like) AS TEXT), title, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), slug, '\')
       OR like(CAST(sqlc.arg(search_like) AS TEXT), description, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), content_html, '\'))
  AND (CAST(sqlc.arg(tag) AS TEXT) = '' OR EXISTS (
       SELECT 1 FROM article_tags JOIN tags ON tags.id = article_tags.tag_id
       WHERE article_tags.article_id = articles.id AND tags.name = CAST(sqlc.arg(tag) AS TEXT)))
  AND (CAST(sqlc.arg(untagged) AS INTEGER) = 0 OR NOT EXISTS (
       SELECT 1 FROM article_tags WHERE article_tags.article_id = articles.id))
ORDER BY created_at DESC, id DESC LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: ListAdminArticlesFilteredCreatedAsc :many
SELECT * FROM articles
WHERE (CAST(sqlc.arg(status_filter) AS INTEGER) = -1 OR status = CAST(sqlc.arg(status_filter) AS INTEGER))
  AND (CAST(sqlc.arg(search_like) AS TEXT) = '' OR like(CAST(sqlc.arg(search_like) AS TEXT), title, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), slug, '\')
       OR like(CAST(sqlc.arg(search_like) AS TEXT), description, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), content_html, '\'))
  AND (CAST(sqlc.arg(tag) AS TEXT) = '' OR EXISTS (
       SELECT 1 FROM article_tags JOIN tags ON tags.id = article_tags.tag_id
       WHERE article_tags.article_id = articles.id AND tags.name = CAST(sqlc.arg(tag) AS TEXT)))
  AND (CAST(sqlc.arg(untagged) AS INTEGER) = 0 OR NOT EXISTS (
       SELECT 1 FROM article_tags WHERE article_tags.article_id = articles.id))
ORDER BY created_at ASC, id ASC LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: ListAdminArticlesFilteredUpdatedDesc :many
SELECT * FROM articles
WHERE (CAST(sqlc.arg(status_filter) AS INTEGER) = -1 OR status = CAST(sqlc.arg(status_filter) AS INTEGER))
  AND (CAST(sqlc.arg(search_like) AS TEXT) = '' OR like(CAST(sqlc.arg(search_like) AS TEXT), title, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), slug, '\')
       OR like(CAST(sqlc.arg(search_like) AS TEXT), description, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), content_html, '\'))
  AND (CAST(sqlc.arg(tag) AS TEXT) = '' OR EXISTS (
       SELECT 1 FROM article_tags JOIN tags ON tags.id = article_tags.tag_id
       WHERE article_tags.article_id = articles.id AND tags.name = CAST(sqlc.arg(tag) AS TEXT)))
  AND (CAST(sqlc.arg(untagged) AS INTEGER) = 0 OR NOT EXISTS (
       SELECT 1 FROM article_tags WHERE article_tags.article_id = articles.id))
ORDER BY updated_at DESC, id DESC LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: ListAdminArticlesFilteredUpdatedAsc :many
SELECT * FROM articles
WHERE (CAST(sqlc.arg(status_filter) AS INTEGER) = -1 OR status = CAST(sqlc.arg(status_filter) AS INTEGER))
  AND (CAST(sqlc.arg(search_like) AS TEXT) = '' OR like(CAST(sqlc.arg(search_like) AS TEXT), title, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), slug, '\')
       OR like(CAST(sqlc.arg(search_like) AS TEXT), description, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), content_html, '\'))
  AND (CAST(sqlc.arg(tag) AS TEXT) = '' OR EXISTS (
       SELECT 1 FROM article_tags JOIN tags ON tags.id = article_tags.tag_id
       WHERE article_tags.article_id = articles.id AND tags.name = CAST(sqlc.arg(tag) AS TEXT)))
  AND (CAST(sqlc.arg(untagged) AS INTEGER) = 0 OR NOT EXISTS (
       SELECT 1 FROM article_tags WHERE article_tags.article_id = articles.id))
ORDER BY updated_at ASC, id ASC LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountAdminArticlesFiltered :one
SELECT COUNT(*) FROM articles
WHERE (CAST(sqlc.arg(status_filter) AS INTEGER) = -1 OR status = CAST(sqlc.arg(status_filter) AS INTEGER))
  AND (CAST(sqlc.arg(search_like) AS TEXT) = '' OR like(CAST(sqlc.arg(search_like) AS TEXT), title, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), slug, '\')
       OR like(CAST(sqlc.arg(search_like) AS TEXT), description, '\') OR like(CAST(sqlc.arg(search_like) AS TEXT), content_html, '\'))
  AND (CAST(sqlc.arg(tag) AS TEXT) = '' OR EXISTS (
       SELECT 1 FROM article_tags JOIN tags ON tags.id = article_tags.tag_id
       WHERE article_tags.article_id = articles.id AND tags.name = CAST(sqlc.arg(tag) AS TEXT)))
  AND (CAST(sqlc.arg(untagged) AS INTEGER) = 0 OR NOT EXISTS (
       SELECT 1 FROM article_tags WHERE article_tags.article_id = articles.id));

-- name: CreateArticle :one
INSERT INTO articles (
  title, slug, content_html, content_type, content_markdown, description, excerpt,
  meta_description, meta_title, meta_image,
  source_author, source_url, source_content,
  status, comment, scheduled_at,
  scheduled_crosspost_platforms, scheduled_send_newsletter,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateArticle :one
UPDATE articles
SET title = ?, slug = ?, content_html = ?, content_type = ?, content_markdown = ?, description = ?, excerpt = ?,
    meta_description = ?, meta_title = ?, meta_image = ?,
    source_author = ?, source_url = ?, source_content = ?,
    status = ?, comment = ?, scheduled_at = ?,
    scheduled_crosspost_platforms = ?, scheduled_send_newsletter = ?,
    created_at = ?, updated_at = ?
WHERE id = ?
RETURNING *;

-- Status-only transitions (publish/unpublish/trash). The Article sync
-- callbacks clear the schedule snapshots unless the target is schedule, so
-- the caller passes the already-synced snapshot values.
-- name: UpdateArticleStatus :one
UPDATE articles
SET status = ?, scheduled_crosspost_platforms = ?, scheduled_send_newsletter = ?, updated_at = ?
WHERE id = ?
RETURNING *;

-- Real delete: dependent-destroy rows go first (comments are reachable via
-- the polymorphic key and the legacy article_id FK).
-- name: DeleteArticle :exec
DELETE FROM articles WHERE id = ?;

-- name: DeleteArticleTagsByArticleID :exec
DELETE FROM article_tags WHERE article_id = ?;

-- name: DeleteSocialMediaPostsByArticleID :exec
DELETE FROM social_media_posts WHERE article_id = ?;

-- name: DeleteCommentsForArticle :exec
DELETE FROM comments WHERE (commentable_type = 'Article' AND commentable_id = ?) OR article_id = ?;

-- tag_list assignment: reset join rows, then re-insert (find_or_create_by
-- semantics via the UNIQUE(article_id, tag_id) conflict target).
-- name: InsertArticleTag :exec
INSERT OR IGNORE INTO article_tags (article_id, tag_id, created_at, updated_at) VALUES (?, ?, ?, ?);

-- name: ListArticleTagNames :many
SELECT article_tags.article_id, tags.name
FROM article_tags
JOIN tags ON tags.id = article_tags.tag_id
WHERE article_tags.article_id IN (sqlc.slice('ids'))
ORDER BY tags.name;

-- Admin index comment counts (load_comment_counts: all statuses).
-- name: CountCommentsForArticles :many
SELECT commentable_id, COUNT(*) AS comment_count
FROM comments
WHERE commentable_type = 'Article' AND commentable_id IN (sqlc.slice('ids'))
GROUP BY commentable_id;

-- name: ListSocialPostsForArticles :many
SELECT * FROM social_media_posts WHERE article_id IN (sqlc.slice('ids')) ORDER BY platform;

-- name: ListSocialPostsByArticleID :many
SELECT * FROM social_media_posts WHERE article_id = ? ORDER BY platform;

-- Nested-attributes sync: blank url destroys the row, otherwise upsert.
-- name: UpsertSocialMediaPost :exec
INSERT INTO social_media_posts (article_id, platform, url, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(article_id, platform) DO UPDATE SET url = excluded.url, updated_at = excluded.updated_at;

-- name: DeleteSocialMediaPost :exec
DELETE FROM social_media_posts WHERE article_id = ? AND platform = ?;

-- fetch_comments sources: posts with a url, optionally platform-filtered.
-- name: ListFetchableSocialPosts :many
SELECT * FROM social_media_posts WHERE article_id = ? AND url != '' ORDER BY platform;

-- name: ListFetchableSocialPostsByPlatform :many
SELECT * FROM social_media_posts WHERE article_id = ? AND url != '' AND platform = ?;

-- Crosspost config lookup (set_form_options / batch_crosspost / handle_crosspost).
-- name: GetCrosspostByPlatform :one
SELECT * FROM crossposts WHERE platform = ?;

-- NewsletterSetting.instance (first_or_initialize: a missing row means disabled).
-- name: GetNewsletterSettings :one
SELECT * FROM newsletter_settings ORDER BY id LIMIT 1;

-- name: GetListmonk :one
SELECT * FROM listmonks ORDER BY id LIMIT 1;

-- Queued publish_article jobs, cancelled before a reschedule
-- (PublishScheduledArticlesJob.cancel_old_jobs).
-- name: ListQueuedJobRunsByKind :many
SELECT * FROM job_runs WHERE kind = ? AND status = 'queued';

-- name: DeleteJobRun :exec
DELETE FROM job_runs WHERE id = ?;
