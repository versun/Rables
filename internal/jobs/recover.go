package jobs

import (
	"context"
	"database/sql"
	"time"

	"rables/internal/db/query"
)

// RecoverStaleJobs requeues job_runs rows stuck in running since before
// cutoff — leftovers of a process that crashed (or was killed) between
// ClaimJobRun and the bookkeeping write. It runs at startup before the
// worker starts polling; the cutoff (now - 5 minutes in main) protects jobs
// freshly claimed by another process during a rolling deploy.
//
// The requeue consumes one attempt (RequeueStaleRunningJobRuns increments
// attempts), and a row whose attempt budget is exhausted by it is failed
// instead of requeued: the claim path never checks attempts, so a plain
// requeue would let a job that reliably crashes the process (OOM/SIGKILL)
// loop forever under systemd Restart=always. The trade-off: a healthy
// long-running job interrupted by a deploy is charged an extra attempt it
// did not really spend, but that over-count is bounded by deploy frequency.
//
// Caveat: job_runs has no heartbeat — updated_at is stamped only at claim
// time — so the cutoff cannot tell a dead process apart from a live one
// still running a job claimed longer ago than the cutoff. The recovery
// therefore assumes a single live process at a time; during a rolling
// deploy the old process must exit within the cutoff window, or its
// long-running jobs may be requeued and executed twice. It returns the
// number of recovered rows (requeued plus failed).
func RecoverStaleJobs(ctx context.Context, q *query.Queries, cutoff time.Time) (int64, error) {
	return q.RequeueStaleRunningJobRuns(ctx, query.RequeueStaleRunningJobRunsParams{
		MaxAttempts: MaxAttempts,
		CrashError:  sql.NullString{String: "process restarted while the job was running; attempt budget exhausted", Valid: true},
		Now:         time.Now().UTC().Unix(),
		Cutoff:      cutoff.UTC().Unix(),
	})
}
