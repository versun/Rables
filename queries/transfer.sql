-- name: ExportArticles :many
SELECT * FROM articles ORDER BY id;

-- name: ExportPages :many
SELECT * FROM pages ORDER BY id;

-- name: ExportTags :many
SELECT * FROM tags ORDER BY id;

-- name: ExportComments :many
SELECT comments.*, articles.slug AS article_slug
FROM comments
LEFT JOIN articles ON articles.id = comments.article_id
ORDER BY comments.id;

-- name: ExportSettings :many
SELECT * FROM settings ORDER BY id;

-- name: ExportCrossposts :many
SELECT * FROM crossposts ORDER BY id;

-- name: ExportListmonks :many
SELECT * FROM listmonks ORDER BY id;

-- name: ExportSocialMediaPosts :many
SELECT social_media_posts.*, articles.slug AS article_slug
FROM social_media_posts
LEFT JOIN articles ON articles.id = social_media_posts.article_id
ORDER BY social_media_posts.id;

-- name: ExportStaticFiles :many
SELECT static_files.*, files.filename AS blob_filename
FROM static_files
LEFT JOIN files ON files.id = static_files.file_id
ORDER BY static_files.id;

-- name: ExportRedirects :many
SELECT * FROM redirects ORDER BY id;

-- name: ExportNewsletterSettings :many
SELECT * FROM newsletter_settings ORDER BY id;

-- name: ExportSubscribers :many
SELECT * FROM subscribers ORDER BY id;

-- name: ExportArticleTags :many
SELECT article_tags.*, articles.slug AS article_slug, tags.name AS tag_name, tags.slug AS tag_slug
FROM article_tags
LEFT JOIN articles ON articles.id = article_tags.article_id
LEFT JOIN tags ON tags.id = article_tags.tag_id
ORDER BY article_tags.id;

-- name: ExportSubscriberTags :many
SELECT subscriber_tags.*, subscribers.email AS subscriber_email, tags.name AS tag_name, tags.slug AS tag_slug
FROM subscriber_tags
LEFT JOIN subscribers ON subscribers.id = subscriber_tags.subscriber_id
LEFT JOIN tags ON tags.id = subscriber_tags.tag_id
ORDER BY subscriber_tags.id;

-- name: ExportFiles :many
SELECT * FROM files ORDER BY id;

-- name: ExportAttachments :many
SELECT * FROM attachments ORDER BY id;
