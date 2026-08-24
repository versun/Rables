-- name: CreateActivityLog :exec
INSERT INTO activity_logs (level, action, target, description, job_run_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListRecentActivityLogs :many
SELECT * FROM activity_logs ORDER BY created_at DESC, id DESC LIMIT 100;

-- name: ListAdminJobRunActivity :many
-- Run-log lines for the /admin/jobs detail rows: activity entries the job
-- handlers recorded, linked through job_run_id. Global id ASC keeps each
-- run's lines in chronological order after the Go side groups by job_run_id.
SELECT * FROM activity_logs
WHERE job_run_id IN (sqlc.slice('ids'))
ORDER BY id ASC;
