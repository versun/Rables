-- name: ListRedirects :many
SELECT * FROM redirects ORDER BY position, id;

-- name: ListEnabledRedirects :many
SELECT * FROM redirects WHERE enabled = 1 ORDER BY position, id;

-- name: GetRedirectByID :one
SELECT * FROM redirects WHERE id = ?;

-- name: CreateRedirect :one
INSERT INTO redirects (regex, replacement, match_from, match_prefix, match_on, permanent, enabled, position, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM redirects), ?, ?)
RETURNING *;

-- name: UpdateRedirect :exec
UPDATE redirects
SET regex = ?, replacement = ?, match_from = ?, match_prefix = ?, match_on = ?, permanent = ?, enabled = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteRedirect :exec
DELETE FROM redirects WHERE id = ?;

-- name: SetRedirectPosition :exec
UPDATE redirects SET position = ? WHERE id = ?;
