-- name: EnqueueJobRun :execlastid
INSERT INTO job_runs (kind, payload, run_at, status, attempts, created_at, updated_at)
VALUES (?, ?, ?, 'queued', 0, ?, ?);

-- name: GetDueJobRun :one
SELECT * FROM job_runs
WHERE status = 'queued' AND run_at <= ?
ORDER BY run_at
LIMIT 1;

-- name: ClaimJobRun :execrows
UPDATE job_runs SET status = 'running', updated_at = ?
WHERE id = ? AND status = 'queued';

-- name: CompleteJobRun :exec
UPDATE job_runs SET status = 'done', last_error = NULL, updated_at = ?
WHERE id = ?;

-- name: RescheduleJobRun :exec
UPDATE job_runs SET status = 'queued', attempts = ?, run_at = ?, last_error = ?, updated_at = ?
WHERE id = ?;

-- name: FailJobRun :exec
UPDATE job_runs SET status = 'failed', attempts = ?, last_error = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteFinishedJobRuns :execrows
DELETE FROM job_runs
WHERE status IN ('done', 'failed') AND updated_at < ?;

-- name: DeleteOldActivityLogs :execrows
DELETE FROM activity_logs WHERE created_at < ?;

-- name: ListCommentFetchers :many
SELECT platform, comment_fetch_schedule FROM crossposts
WHERE enabled = 1 AND auto_fetch_comments = 1
  AND comment_fetch_schedule IS NOT NULL AND comment_fetch_schedule != '';

-- name: GetTwitterSync :one
SELECT * FROM twitter_syncs WHERE id = 1;
