-- Comment chain (plan section 4.5). Commentable lookups are duplicated here under
-- feature-specific names so this file stays independent of other features'
-- query files.

-- name: GetCommentableArticleBySlug :one
SELECT * FROM articles WHERE slug = ?;

-- name: GetCommentableArticleByID :one
SELECT * FROM articles WHERE id = ?;

-- name: GetCommentablePageBySlug :one
SELECT * FROM pages WHERE slug = ?;

-- name: GetCommentablePageByID :one
SELECT * FROM pages WHERE id = ?;

-- name: CreateComment :one
INSERT INTO comments (
  commentable_type, commentable_id, article_id, parent_id,
  author_name, author_email, author_url, author_username, author_avatar_url,
  content, status, platform, external_id, url, published_at,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetCommentByID :one
SELECT * FROM comments WHERE id = ?;

-- name: ListCommentsForCommentable :many
-- Mirrors Comment.default_scope (published_at ASC; SQLite sorts NULLs first).
SELECT * FROM comments
WHERE commentable_type = ? AND commentable_id = ?
ORDER BY published_at ASC;

-- name: ListAdminComments :many
-- Admin moderation list (plan section 4.5):
-- COALESCE(published_at, created_at) DESC, 30 per page.
SELECT * FROM comments
ORDER BY COALESCE(published_at, created_at) DESC
LIMIT ? OFFSET ?;

-- name: ListAdminCommentsByStatus :many
SELECT * FROM comments
WHERE status = ?
ORDER BY COALESCE(published_at, created_at) DESC
LIMIT ? OFFSET ?;

-- name: CountAdminComments :one
SELECT COUNT(*) FROM comments;

-- name: CountAdminCommentsByStatus :one
SELECT COUNT(*) FROM comments WHERE status = ?;

-- name: UpdateCommentStatus :one
UPDATE comments SET status = ?, updated_at = ?
WHERE id = ?
RETURNING *;

-- name: DeleteComment :exec
DELETE FROM comments WHERE id = ?;

-- name: GetExternalComment :one
SELECT * FROM comments
WHERE commentable_type = ? AND commentable_id = ? AND platform = ? AND external_id = ?;

-- name: UpdateExternalComment :one
-- Re-fetched platform data never touches status (moderation decision).
UPDATE comments
SET author_name = ?, author_username = ?, author_avatar_url = ?,
    content = ?, published_at = ?, url = ?, updated_at = ?
WHERE id = ?
RETURNING *;
