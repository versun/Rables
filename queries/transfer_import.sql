-- Import queries used by the RSS importer (transfer/import_rss.go).
-- Timestamps are unix seconds; booleans are 0/1.

-- articles: skip when the slug already exists.
-- name: ImportArticleIDBySlug :one
SELECT id FROM articles WHERE slug = ?;

-- name: ImportInsertArticle :one
INSERT INTO articles (title, slug, content_html, content_type, description, excerpt,
  meta_description, meta_title, meta_image, source_author, source_url, source_content,
  status, comment, scheduled_at, scheduled_crosspost_platforms, scheduled_send_newsletter,
  created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;
