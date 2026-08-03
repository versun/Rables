-- name: ListStaticFiles :many
SELECT sf.id, sf.filename, sf.description, sf.file_id, sf.created_at, sf.updated_at,
  f.key, f.byte_size, f.content_type
FROM static_files sf
JOIN files f ON f.id = sf.file_id
ORDER BY sf.created_at DESC;

-- name: GetStaticFileByID :one
SELECT * FROM static_files WHERE id = ?;

-- name: GetStaticFileByFilename :one
SELECT * FROM static_files WHERE filename = ?;

-- name: GetFileForStaticFilename :one
SELECT f.* FROM static_files sf
JOIN files f ON f.id = sf.file_id
WHERE sf.filename = ?;

-- name: CreateStaticFile :one
INSERT INTO static_files (filename, description, file_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateStaticFile :exec
UPDATE static_files
SET description = ?, file_id = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteStaticFile :exec
DELETE FROM static_files WHERE id = ?;

-- name: DeleteFileByID :exec
DELETE FROM files WHERE id = ?;
