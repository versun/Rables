-- Import queries (plan T26, section 4.11). All run inside the single import
-- transaction. Timestamps are unix seconds; booleans are 0/1.

-- tags: skip when the slug already exists.
-- name: ImportTagIDBySlug :one
SELECT id FROM tags WHERE slug = ?;

-- name: ImportInsertTag :one
INSERT INTO tags (name, slug, created_at, updated_at)
VALUES (?, ?, ?, ?) RETURNING id;

-- articles: skip when the slug already exists.
-- name: ImportArticleIDBySlug :one
SELECT id FROM articles WHERE slug = ?;

-- name: ImportInsertArticle :one
INSERT INTO articles (title, slug, content_html, content_type, description, excerpt,
  meta_description, meta_title, meta_image, source_author, source_url, source_content,
  status, comment, scheduled_at, scheduled_crosspost_platforms, scheduled_send_newsletter,
  created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- pages: skip when the slug already exists.
-- name: ImportPageIDBySlug :one
SELECT id FROM pages WHERE slug = ?;

-- name: ImportInsertPage :one
INSERT INTO pages (title, slug, content_html, content_type, redirect_url, page_order,
  status, comment, scheduled_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ImportArticleTagID :one
SELECT id FROM article_tags WHERE article_id = ? AND tag_id = ?;

-- name: ImportInsertArticleTag :exec
INSERT INTO article_tags (article_id, tag_id, created_at, updated_at)
VALUES (?, ?, ?, ?);

-- comments: external duplicates match (article_id, platform, external_id);
-- local duplicates match article + author + content within a +-5s window on
-- published_at (preferred) or created_at (ImportZip#import_comments).
-- name: ImportExternalCommentID :one
SELECT id FROM comments WHERE article_id = ? AND platform = ? AND external_id = ?;

-- name: ImportLocalCommentIDByPublishedAt :one
SELECT id FROM comments
WHERE article_id = sqlc.arg(article_id) AND platform IS NULL
  AND author_name = sqlc.arg(author_name) AND content = sqlc.arg(content)
  AND published_at >= sqlc.arg(from_ts) AND published_at <= sqlc.arg(to_ts)
LIMIT 1;

-- name: ImportLocalCommentIDByCreatedAt :one
SELECT id FROM comments
WHERE article_id = sqlc.arg(article_id) AND platform IS NULL
  AND author_name = sqlc.arg(author_name) AND content = sqlc.arg(content)
  AND created_at >= sqlc.arg(from_ts) AND created_at <= sqlc.arg(to_ts)
LIMIT 1;

-- First pass inserts with parent_id NULL; the second pass backfills it.
-- name: ImportInsertComment :one
INSERT INTO comments (commentable_type, commentable_id, article_id, parent_id,
  author_name, author_email, author_url, author_username, author_avatar_url,
  content, status, platform, external_id, url, published_at, created_at, updated_at)
VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ImportSetCommentParent :exec
UPDATE comments SET parent_id = ?, updated_at = ? WHERE id = ? AND parent_id IS NULL;

-- subscribers: skip when the email already exists.
-- name: ImportSubscriberIDByEmail :one
SELECT id FROM subscribers WHERE email = ?;

-- name: ImportInsertSubscriber :one
INSERT INTO subscribers (email, confirmation_token, unsubscribe_token,
  confirmed_at, unsubscribed_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ImportSubscriberTagID :one
SELECT id FROM subscriber_tags WHERE subscriber_id = ? AND tag_id = ?;

-- name: ImportInsertSubscriberTag :exec
INSERT INTO subscriber_tags (subscriber_id, tag_id, created_at, updated_at)
VALUES (?, ?, ?, ?);

-- settings is a singleton (id = 1): update the existing row, else insert.
-- name: ImportSettingsID :one
SELECT id FROM settings WHERE id = 1;

-- name: ImportInsertSettings :exec
INSERT INTO settings (id, title, description, author, url, time_zone, head_code,
  custom_css, tool_code, giscus, social_links, setup_completed, created_at, updated_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ImportUpdateSettings :exec
UPDATE settings SET title = ?, description = ?, author = ?, url = ?, time_zone = ?,
  head_code = ?, custom_css = ?, tool_code = ?, giscus = ?, social_links = ?,
  setup_completed = ?, created_at = ?, updated_at = ?
WHERE id = 1;

-- crossposts upsert by platform.
-- name: ImportCrosspostByPlatform :one
SELECT * FROM crossposts WHERE platform = ?;

-- name: ImportInsertCrosspost :exec
INSERT INTO crossposts (platform, enabled, api_key, api_key_secret, access_token,
  access_token_secret, client_id, client_key, client_secret, app_password,
  refresh_token, token_expires_at, server_url, username, max_characters,
  auto_fetch_comments, comment_fetch_schedule, settings, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ImportUpdateCrosspost :exec
UPDATE crossposts SET enabled = ?, api_key = ?, api_key_secret = ?, access_token = ?,
  access_token_secret = ?, client_id = ?, client_key = ?, client_secret = ?,
  app_password = ?, refresh_token = ?, token_expires_at = ?, server_url = ?,
  username = ?, max_characters = ?, auto_fetch_comments = ?, comment_fetch_schedule = ?,
  settings = ?, created_at = ?, updated_at = ?
WHERE platform = ?;

-- listmonks is a singleton (id = 1).
-- name: ImportListmonk :one
SELECT * FROM listmonks WHERE id = 1;

-- name: ImportInsertListmonk :exec
INSERT INTO listmonks (id, url, username, api_key, list_id, template_id, enabled,
  created_at, updated_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ImportUpdateListmonk :exec
UPDATE listmonks SET url = ?, username = ?, api_key = ?, list_id = ?, template_id = ?,
  enabled = ?, created_at = ?, updated_at = ?
WHERE id = 1;

-- newsletter_settings is a singleton (id = 1).
-- name: ImportNewsletterSetting :one
SELECT * FROM newsletter_settings WHERE id = 1;

-- name: ImportInsertNewsletterSetting :exec
INSERT INTO newsletter_settings (id, enabled, provider, from_email, smtp_address,
  smtp_port, smtp_user_name, smtp_password, smtp_domain, smtp_authentication,
  smtp_enable_starttls, created_at, updated_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ImportUpdateNewsletterSetting :exec
UPDATE newsletter_settings SET enabled = ?, provider = ?, from_email = ?,
  smtp_address = ?, smtp_port = ?, smtp_user_name = ?, smtp_password = ?,
  smtp_domain = ?, smtp_authentication = ?, smtp_enable_starttls = ?,
  created_at = ?, updated_at = ?
WHERE id = 1;

-- social_media_posts: skip when (article_id, platform) already exists.
-- name: ImportSocialMediaPostID :one
SELECT id FROM social_media_posts WHERE article_id = ? AND platform = ?;

-- name: ImportInsertSocialMediaPost :exec
INSERT INTO social_media_posts (article_id, platform, url, created_at, updated_at)
VALUES (?, ?, ?, ?, ?);

-- redirects: skip when the regex already exists.
-- name: ImportRedirectIDByRegex :one
SELECT id FROM redirects WHERE regex = ?;

-- name: ImportInsertRedirect :exec
INSERT INTO redirects (regex, replacement, enabled, permanent, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?);

-- files: skip when the key already exists; the CSV key is kept verbatim.
-- name: ImportFileByKey :one
SELECT * FROM files WHERE key = ?;

-- name: ImportInsertFile :one
INSERT INTO files (key, filename, content_type, byte_size, checksum, variant_of, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- attachments: skip when (record_type, record_id, name, file_id) already exists.
-- name: ImportAttachmentID :one
SELECT id FROM attachments WHERE record_type = ? AND record_id = ? AND name = ? AND file_id = ?;

-- name: ImportInsertAttachment :exec
INSERT INTO attachments (file_id, record_type, record_id, name, created_at)
VALUES (?, ?, ?, ?, ?);

-- static_files: skip when the filename already exists.
-- name: ImportStaticFileIDByFilename :one
SELECT id FROM static_files WHERE filename = ?;

-- name: ImportInsertStaticFile :one
INSERT INTO static_files (filename, description, file_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?) RETURNING id;
