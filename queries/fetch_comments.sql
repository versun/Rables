-- Social comment fetching (T22, plan section 4.8): the cron scan target list
-- and the parent backfill of the two-pass external upsert.
-- Reads reuse GetCrosspostByPlatform (articles_admin.sql) and GetExternalComment
-- / CreateComment / UpdateExternalComment (comments.sql).

-- FetchSocialCommentsJob scan: published articles (status = 1,
-- Article.published) that have a social_media_posts URL for the platform.
-- UNIQUE(article_id, platform) makes the Rails .distinct unnecessary; the id
-- order only keeps the scan deterministic.
-- name: ListCommentFetchTargets :many
SELECT a.id, p.url
FROM articles a
JOIN social_media_posts p ON p.article_id = a.id
WHERE a.status = 1 AND p.platform = ? AND p.url != ''
ORDER BY a.id;

-- Second pass of the two-pass external upsert
-- (Admin::ArticlesController#fetch_comments): attach a reply to its parent.
-- name: SetCommentParent :exec
UPDATE comments SET parent_id = ?, updated_at = ? WHERE id = ?;
