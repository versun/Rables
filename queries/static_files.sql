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

-- name: UpdateStaticFile :execrows
UPDATE static_files
SET description = ?, file_id = ?, updated_at = ?
WHERE id = ? AND file_id = sqlc.arg(expected_file_id);

-- name: DeleteStaticFile :one
DELETE FROM static_files WHERE id = ? RETURNING file_id;

-- Record cleanup (articles.Destroy, twitter archive replace) deletes files
-- rows left without references; static_files.file_id references files(id)
-- under foreign_keys enforcement, so these references count too.
-- name: CountStaticFilesForFile :one
SELECT COUNT(*) FROM static_files WHERE file_id = ?;

-- name: DeleteFileByID :exec
DELETE FROM files WHERE id = ?;
