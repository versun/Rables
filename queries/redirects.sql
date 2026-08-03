-- name: ListRedirects :many
SELECT * FROM redirects ORDER BY created_at DESC;

-- name: ListEnabledRedirects :many
SELECT * FROM redirects WHERE enabled = 1 ORDER BY id;

-- name: GetRedirectByID :one
SELECT * FROM redirects WHERE id = ?;

-- name: CreateRedirect :one
INSERT INTO redirects (regex, replacement, permanent, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateRedirect :exec
UPDATE redirects
SET regex = ?, replacement = ?, permanent = ?, enabled = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteRedirect :exec
DELETE FROM redirects WHERE id = ?;
