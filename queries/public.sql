-- Public site queries (plan T12). Visibility follows plan section 4.1: only
-- status = 1 (publish) is listed publicly; show pages additionally accept
-- status = 4 (shared) and authenticated previews of any status.

-- name: GetPublicArticleBySlug :one
SELECT * FROM articles WHERE slug = ?;

-- name: ListPublishedArticles :many
SELECT * FROM articles
WHERE status = 1
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountPublishedArticles :one
SELECT COUNT(*) FROM articles WHERE status = 1;

-- Mirrors Article.search_content + published scope. The caller escapes %, _
-- and backslash in the term and wraps it in %...%; ESCAPE '\' makes the
-- escaping effective.
-- name: SearchPublishedArticles :many
SELECT * FROM articles
WHERE status = 1 AND (
  (title LIKE ? ESCAPE '\') OR
  (slug LIKE ? ESCAPE '\') OR
  (description LIKE ? ESCAPE '\') OR
  (content_html LIKE ? ESCAPE '\')
)
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountSearchPublishedArticles :one
SELECT COUNT(*) FROM articles
WHERE status = 1 AND (
  (title LIKE ? ESCAPE '\') OR
  (slug LIKE ? ESCAPE '\') OR
  (description LIKE ? ESCAPE '\') OR
  (content_html LIKE ? ESCAPE '\')
);

-- name: ListPublishedArticleSitemap :many
SELECT slug, updated_at FROM articles WHERE status = 1 ORDER BY created_at DESC;

-- name: LatestPublishedArticleUpdate :one
SELECT MAX(updated_at) FROM articles WHERE status = 1;

-- name: GetPublicPageBySlug :one
SELECT * FROM pages WHERE slug = ?;

-- name: ListPublishedPageSitemap :many
SELECT slug, updated_at FROM pages WHERE status = 1 ORDER BY created_at DESC;

-- CacheableSettings.navbar_items: published pages, page_order DESC.
-- name: ListNavbarPages :many
SELECT id, title, slug, redirect_url FROM pages WHERE status = 1 ORDER BY page_order DESC;

-- Tag.alphabetical (order(:name)).
-- name: ListPublicTags :many
SELECT * FROM tags ORDER BY name;

-- name: GetPublicTagBySlug :one
SELECT * FROM tags WHERE slug = ?;

-- name: CountPublicTags :one
SELECT COUNT(*) FROM tags;

-- TagsController#index: published article counts grouped by tag_id.
-- name: ListPublishedCountsByTag :many
SELECT article_tags.tag_id, COUNT(*) AS published_count
FROM article_tags
JOIN articles ON articles.id = article_tags.article_id
WHERE articles.status = 1
GROUP BY article_tags.tag_id;

-- name: ListPublishedArticlesByTag :many
SELECT articles.* FROM articles
JOIN article_tags ON article_tags.article_id = articles.id
WHERE articles.status = 1 AND article_tags.tag_id = ?
ORDER BY articles.created_at DESC
LIMIT ? OFFSET ?;

-- name: CountPublishedArticlesByTag :one
SELECT COUNT(*) FROM articles
JOIN article_tags ON article_tags.article_id = articles.id
WHERE articles.status = 1 AND article_tags.tag_id = ?;

-- Tags shown next to each article in the public list (Tag.alphabetical).
-- name: ListTagsForArticle :many
SELECT tags.* FROM tags
JOIN article_tags ON article_tags.tag_id = tags.id
WHERE article_tags.article_id = ?
ORDER BY tags.name;
