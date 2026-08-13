package jobs

import (
	"testing"
	"time"

	"rables/internal/db/query"
)

func TestRecoverStaleJobs(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-5 * time.Minute)

	insert := func(status string, updatedAt time.Time, attempts int64) int64 {
		t.Helper()
		res, err := d.Exec(
			"INSERT INTO job_runs (kind, run_at, status, attempts, created_at, updated_at) VALUES ('export', 0, ?, ?, 0, ?)",
			status, attempts, updatedAt.Unix(),
		)
		if err != nil {
			t.Fatalf("insert job_run: %v", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("job_run id: %v", err)
		}
		return id
	}
	staleRunning := insert("running", now.Add(-time.Hour), 0)
	nearLimit := insert("running", now.Add(-time.Hour), MaxAttempts-1)
	freshRunning := insert("running", now.Add(-time.Minute), 0)
	staleQueued := insert("queued", now.Add(-time.Hour), 0)

	n, err := RecoverStaleJobs(t.Context(), query.New(d), cutoff)
	if err != nil {
		t.Fatalf("RecoverStaleJobs: %v", err)
	}
	if n != 2 {
		t.Errorf("recovered = %d, want 2", n)
	}
	// The requeue consumes an attempt so a job that keeps crashing the
	// process is bounded by MaxAttempts instead of looping on every restart.
	if job := getJobRun(t, d, staleRunning); job.Status != "queued" || job.Attempts != 1 {
		t.Errorf("stale running = (%q, attempts %d), want (queued, 1)", job.Status, job.Attempts)
	}
	// The claim path never checks attempts, so a requeue that exhausts the
	// budget must fail the row instead: a crash-looping job (OOM/SIGKILL)
	// would otherwise be requeued on every restart forever.
	if job := getJobRun(t, d, nearLimit); job.Status != "failed" || job.Attempts != MaxAttempts ||
		!job.LastError.Valid || job.LastError.String == "" {
		t.Errorf("near-limit running = (%q, attempts %d, last_error %+v), want (failed, %d, set)",
			job.Status, job.Attempts, job.LastError, MaxAttempts)
	}
	if job := getJobRun(t, d, freshRunning); job.Status != "running" || job.Attempts != 0 {
		t.Errorf("fresh running = (%q, attempts %d), want (running, 0) (cutoff protects live claims)", job.Status, job.Attempts)
	}
	if job := getJobRun(t, d, staleQueued); job.Status != "queued" || job.Attempts != 0 {
		t.Errorf("stale queued = (%q, attempts %d), want (queued, 0) (untouched)", job.Status, job.Attempts)
	}
}
