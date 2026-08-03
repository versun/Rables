-- Crosspost platform config + per-article image collection (T20).
-- Reads reuse GetCrosspostByPlatform / UpsertSocialMediaPost /
-- ListFetchableSocialPostsByPlatform from articles_admin.sql.

-- Crosspost.find_or_create_by(platform:) on the admin update path.
-- name: EnsureCrosspost :exec
INSERT OR IGNORE INTO crossposts (platform, created_at, updated_at) VALUES (?, ?, ?);

-- Admin update: full overlay of the permitted crosspost_params columns.
-- name: UpdateCrosspost :exec
UPDATE crossposts SET
  enabled = ?, server_url = ?, username = ?,
  access_token = ?, access_token_secret = ?,
  client_key = ?, client_secret = ?,
  api_key = ?, api_key_secret = ?, app_password = ?,
  max_characters = ?, auto_fetch_comments = ?, comment_fetch_schedule = ?,
  updated_at = ?
WHERE platform = ?;

-- Article#all_image_attachments path 1: image files attached to the article
-- (ActionText attachables in Rails), in attachment order.
-- name: ListImageAttachmentsForArticle :many
SELECT files.id, files.key, files.filename, files.content_type, files.byte_size, files.checksum, files.variant_of, files.created_at
FROM attachments
JOIN files ON files.id = attachments.file_id
WHERE attachments.record_type = 'Article' AND attachments.record_id = ?
  AND files.content_type LIKE 'image/%'
ORDER BY attachments.id;
