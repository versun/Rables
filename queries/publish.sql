-- name: GetArticlePublishState :one
SELECT status, scheduled_at, scheduled_crosspost_platforms, scheduled_send_newsletter
FROM articles WHERE id = ?;

-- name: PublishScheduledArticle :execrows
-- Flips schedule -> publish, backfills created_at with scheduled_at, and
-- clears the consumed snapshot. Guarded by the schedule status so a rerun
-- after a successful flip updates 0 rows and never re-consumes the snapshot.
UPDATE articles
SET status = :publish_status,
    created_at = :created_at,
    scheduled_at = NULL,
    scheduled_crosspost_platforms = '[]',
    scheduled_send_newsletter = 0,
    updated_at = :updated_at
WHERE id = :id AND status = :schedule_status;

-- name: GetPagePublishState :one
SELECT status, scheduled_at FROM pages WHERE id = ?;

-- name: PublishScheduledPage :execrows
-- Rails Page#publish_scheduled does NOT backfill created_at (unlike Article).
UPDATE pages
SET status = :publish_status,
    scheduled_at = NULL,
    updated_at = :updated_at
WHERE id = :id AND status = :schedule_status;
