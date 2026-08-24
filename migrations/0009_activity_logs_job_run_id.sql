-- activity_logs rows written by a job handler (via the run id the worker puts
-- on the handler ctx) link back to that job_runs row, so /admin/jobs can show
-- each run's real log lines in its expandable detail row. Rows written from
-- HTTP request paths keep NULL. ON DELETE SET NULL keeps the audit rows when
-- DeleteFinishedJobRuns prunes old job rows (foreign_keys is enabled).

-- +goose Up
ALTER TABLE activity_logs ADD COLUMN job_run_id INTEGER REFERENCES job_runs(id) ON DELETE SET NULL;
CREATE INDEX idx_activity_logs_job_run ON activity_logs(job_run_id);

-- +goose Down
DROP INDEX IF EXISTS idx_activity_logs_job_run;
ALTER TABLE activity_logs DROP COLUMN job_run_id;
