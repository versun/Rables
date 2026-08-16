-- name: EnqueueJobRun :execlastid
INSERT INTO job_runs (kind, payload, run_at, status, attempts, created_at, updated_at)
VALUES (?, ?, ?, 'queued', 0, ?, ?);

-- name: EnqueueJobRunUnlessActive :execrows
-- Dedup variant of EnqueueJobRun for singleton jobs (twitter_sync): inserts
-- only when no queued/running row of the same kind exists. The scheduler
-- re-fires on a fixed cadence while the due timestamp (last_synced_at) only
-- advances after a successful run, so without this a backed-up worker would
-- accumulate duplicate rows. Rows affected: 1 = enqueued, 0 = skipped.
INSERT INTO job_runs (kind, payload, run_at, status, attempts, created_at, updated_at)
SELECT :kind, :payload, :run_at, 'queued', 0, :created_at, :updated_at
WHERE NOT EXISTS (
  SELECT 1 FROM job_runs WHERE kind = :kind AND status IN ('queued', 'running')
);

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

-- name: RequeueStaleRunningJobRuns :execrows
-- Startup recovery: rows claimed (running) but untouched since :cutoff belong
-- to a dead process; requeue them so they run again. The requeue consumes an
-- attempt, and a row whose attempt budget is exhausted by it is failed
-- instead of requeued: the claim path never checks attempts, so a plain
-- requeue would let a job that reliably crashes the process (OOM/SIGKILL)
-- loop forever under Restart=always.
UPDATE job_runs SET
  status = CASE WHEN attempts + 1 >= :max_attempts THEN 'failed' ELSE 'queued' END,
  attempts = attempts + 1,
  last_error = CASE WHEN attempts + 1 >= :max_attempts THEN :crash_error ELSE last_error END,
  updated_at = :now
WHERE status = 'running' AND updated_at < :cutoff;

-- name: DeleteFinishedJobRuns :execrows
DELETE FROM job_runs
WHERE status IN ('done', 'failed') AND updated_at < ?;

-- name: ListActiveImportJobPayloads :many
-- Startup orphan-upload cleanup (jobs.CleanupOrphanImportFiles): payloads of
-- still-active import jobs reference the data/imports files that must not be
-- deleted.
SELECT kind, payload FROM job_runs
WHERE status IN ('queued', 'running')
  AND kind IN ('import_db', 'import_markdown');

-- name: DeleteOldActivityLogs :execrows
DELETE FROM activity_logs WHERE created_at < ?;

-- name: ListCommentFetchers :many
SELECT platform, comment_fetch_schedule FROM crossposts
WHERE enabled = 1 AND auto_fetch_comments = 1
  AND comment_fetch_schedule IS NOT NULL AND comment_fetch_schedule != '';

-- name: GetTwitterSync :one
SELECT * FROM twitter_syncs WHERE id = 1;
