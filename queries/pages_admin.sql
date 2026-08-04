-- Admin page management (plan section 4.1, task T10). Mirrors
-- Admin::PagesController: lookup by slug, page_order DESC listing with a
-- 100-per-page window, and cancellation of superseded publish_page jobs.

-- name: GetAdminPageBySlug :one
SELECT * FROM pages WHERE slug = ?;

-- name: CountAdminPages :one
SELECT COUNT(*) FROM pages;

-- name: CountAdminPagesByStatus :one
SELECT COUNT(*) FROM pages WHERE status = ?;

-- name: ListAdminPages :many
SELECT * FROM pages
ORDER BY page_order DESC
LIMIT ? OFFSET ?;

-- name: ListAdminPagesByStatus :many
SELECT * FROM pages
WHERE status = ?
ORDER BY page_order DESC
LIMIT ? OFFSET ?;

-- name: AdminPageSlugCount :one
-- Uniqueness check; excludeID is 0 on create so any row matches.
SELECT COUNT(*) FROM pages WHERE slug = ? AND id != ?;

-- name: CreatePage :one
INSERT INTO pages (
  title, slug, content_html, content_type, content_markdown, redirect_url,
  page_order, status, comment, scheduled_at,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdatePage :one
UPDATE pages
SET title = ?, slug = ?, content_html = ?, content_type = ?, content_markdown = ?, redirect_url = ?,
    page_order = ?, status = ?, comment = ?, scheduled_at = ?, updated_at = ?
WHERE id = ?
RETURNING *;

-- name: UpdatePageStatus :exec
UPDATE pages SET status = ?, updated_at = ?
WHERE id = ?;

-- name: DeletePageByID :exec
DELETE FROM pages WHERE id = ?;

-- name: DeletePageComments :exec
-- Page has_many :comments, as: :commentable, dependent: :destroy; the
-- polymorphic association has no FK, so real deletes clean it up by hand.
DELETE FROM comments WHERE commentable_type = 'Page' AND commentable_id = ?;

-- name: CancelQueuedPublishPageJobs :execrows
-- Drops still-queued publish_page jobs for one page before a new schedule is
-- enqueued (article-side cancel_old_jobs semantics, per task T10).
UPDATE job_runs SET status = 'failed', last_error = ?, updated_at = ?
WHERE kind = 'publish_page' AND status = 'queued'
  AND json_extract(payload, '$.page_id') = CAST(sqlc.arg(page_id) AS INTEGER);
