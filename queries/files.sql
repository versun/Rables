-- name: CreateFile :one
INSERT INTO files (key, filename, content_type, byte_size, checksum, variant_of, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetFileByKey :one
SELECT * FROM files WHERE key = ?;

-- name: ListFileVariants :many
SELECT * FROM files WHERE variant_of = ? ORDER BY id;

-- name: CreateAttachment :exec
INSERT OR IGNORE INTO attachments (file_id, record_type, record_id, name, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: ListAttachmentsForFile :many
SELECT * FROM attachments WHERE file_id = ? ORDER BY id;
