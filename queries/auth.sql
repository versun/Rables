-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;

-- name: GetUserByUserName :one
SELECT * FROM users WHERE user_name = ?;

-- name: CreateUser :one
INSERT INTO users (user_name, password_digest, created_at, updated_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: UpdateUser :exec
UPDATE users SET user_name = ?, password_digest = ?, updated_at = ?
WHERE id = ?;

-- name: CreateSession :one
INSERT INTO sessions (token, user_id, ip_address, user_agent, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetSessionByToken :one
SELECT * FROM sessions WHERE token = ?;

-- name: DeleteSessionByToken :exec
DELETE FROM sessions WHERE token = ?;

-- name: GetSettings :one
SELECT * FROM settings WHERE id = 1;

-- name: EnsureSettings :exec
INSERT INTO settings (id, created_at, updated_at)
VALUES (1, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: CompleteSetup :exec
UPDATE settings
SET title = ?, description = ?, author = ?, url = ?, time_zone = ?,
    setup_completed = 1, updated_at = ?
WHERE id = 1;
