-- Admin job_runs read-only list (plan section 5): id DESC, 100 per page,
-- optional status filter. No Mission Control-style management actions.

-- name: ListAdminJobRuns :many
SELECT * FROM job_runs ORDER BY id DESC LIMIT ? OFFSET ?;

-- name: CountAdminJobRuns :one
SELECT COUNT(*) FROM job_runs;

-- name: ListAdminJobRunsByStatus :many
SELECT * FROM job_runs WHERE status = ? ORDER BY id DESC LIMIT ? OFFSET ?;

-- name: CountAdminJobRunsByStatus :one
SELECT COUNT(*) FROM job_runs WHERE status = ?;
