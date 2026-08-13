-- name: CreateFile :one
INSERT INTO files (key, filename, content_type, byte_size, checksum, variant_of, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetFileByID :one
SELECT * FROM files WHERE id = ?;

-- name: GetFileByKey :one
SELECT * FROM files WHERE key = ?;

-- name: ListFileVariants :many
SELECT * FROM files WHERE variant_of = ? ORDER BY id;

-- name: CreateAttachment :exec
INSERT OR IGNORE INTO attachments (file_id, record_type, record_id, name, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: ListAttachmentsForFile :many
SELECT * FROM attachments WHERE file_id = ? ORDER BY id;

-- Media cleanup for record destroy (mirrors the ListTwitterArchiveTweetMediaFiles
-- pair in twitter_archive.sql): the files behind one record's attachments.
-- name: ListAttachmentFilesForRecord :many
SELECT f.id, f.key FROM attachments a
JOIN files f ON f.id = a.file_id
WHERE a.record_type = ? AND a.record_id = ?;

-- name: DeleteAttachmentsForRecord :exec
DELETE FROM attachments WHERE record_type = ? AND record_id = ?;

-- Startup orphan sweep (jobs.ReapOrphanFiles): family roots (variants are
-- reachable only through their original, so they share the original's fate)
-- created before the cutoff that no attachment or static_files entry
-- references, neither directly nor through a family variant.
-- name: ListOrphanFileCandidates :many
SELECT f.id, f.key FROM files f
WHERE f.created_at < sqlc.arg(cutoff)
  AND f.variant_of IS NULL
  AND NOT EXISTS (SELECT 1 FROM attachments a WHERE a.file_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM static_files sf WHERE sf.file_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM files v JOIN attachments av ON av.file_id = v.id WHERE v.variant_of = f.id)
  AND NOT EXISTS (SELECT 1 FROM files v JOIN static_files sv ON sv.file_id = v.id WHERE v.variant_of = f.id)
ORDER BY f.id;

-- Files can also be referenced by URL (/files/<key>) from stored content:
-- admin uploads and RSS-imported images are embedded in article/page bodies
-- and never get an attachment row. Keys are alphanumeric (media.ValidKey),
-- so the LIKE pattern needs no metacharacter escaping; a prefix collision
-- with a longer key only over-protects.
-- name: CountFileKeyContentReferences :one
SELECT
  (SELECT COUNT(*) FROM articles
   WHERE content_html LIKE '%/files/' || sqlc.arg(key) || '%'
      OR meta_image LIKE '%/files/' || sqlc.arg(key) || '%') +
  (SELECT COUNT(*) FROM pages
   WHERE content_html LIKE '%/files/' || sqlc.arg(key) || '%') +
  (SELECT COUNT(*) FROM comments
   WHERE content LIKE '%/files/' || sqlc.arg(key) || '%') +
  (SELECT COUNT(*) FROM settings
   WHERE head_code LIKE '%/files/' || sqlc.arg(key) || '%'
      OR custom_css LIKE '%/files/' || sqlc.arg(key) || '%'
      OR tool_code LIKE '%/files/' || sqlc.arg(key) || '%'
      OR social_links LIKE '%/files/' || sqlc.arg(key) || '%') +
  (SELECT COUNT(*) FROM redirects
   WHERE replacement LIKE '%/files/' || sqlc.arg(key) || '%');
