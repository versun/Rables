-- name: GetKVValue :one
SELECT value FROM kv WHERE key = ?;

-- name: SetKVValue :exec
INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at;
