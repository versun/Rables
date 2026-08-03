-- name: CreateActivityLog :exec
INSERT INTO activity_logs (level, action, target, description, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListRecentActivityLogs :many
SELECT * FROM activity_logs ORDER BY created_at DESC, id DESC LIMIT 100;
