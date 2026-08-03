package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"rables/internal/db/query"
)

// MaxAttempts is the number of executions after which a job is marked failed.
const MaxAttempts = 5

// Backoff lists the retry delays; a failure at attempts n (0-based) is
// rescheduled with Backoff[min(n, len(Backoff)-1)].
var Backoff = []time.Duration{30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// Handler executes one job payload.
type Handler func(ctx context.Context, payload json.RawMessage) error

// Worker polls job_runs and dispatches to registered handlers. It is
// single-goroutine; call Start once.
type Worker struct {
	q *query.Queries
	// PollInterval is how often Start polls for due jobs.
	PollInterval time.Duration

	handlers map[string]Handler
	now      func() time.Time
	logger   *slog.Logger
}

// NewWorker returns a Worker with a 30s poll interval (plan §5).
func NewWorker(db *sql.DB) *Worker {
	return &Worker{
		q:            query.New(db),
		PollInterval: 30 * time.Second,
		handlers:     map[string]Handler{},
		now:          time.Now,
		logger:       slog.Default(),
	}
}

// Register installs fn as the handler for kind, replacing any previous one.
func (w *Worker) Register(kind string, fn Handler) {
	w.handlers[kind] = fn
}

// Start polls for due jobs until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	t := time.NewTicker(w.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := w.RunOnce(ctx); err != nil {
				w.logger.Error("job run failed", "error", err)
			}
		}
	}
}

// RunOnce claims and executes the oldest due job, if any. It reports whether
// a job was claimed. Handler failures are recorded on the row (retry or
// failed) and are not returned; only infrastructure errors are.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	now := w.now().UTC()
	job, err := w.q.GetDueJobRun(ctx, now.Unix())
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get due job: %w", err)
	}
	claimed, err := w.q.ClaimJobRun(ctx, query.ClaimJobRunParams{UpdatedAt: now.Unix(), ID: job.ID})
	if err != nil {
		return false, fmt.Errorf("claim job %d: %w", job.ID, err)
	}
	if claimed == 0 {
		return false, nil // lost the race; picked up on a later poll
	}

	handler, ok := w.handlers[job.Kind]
	if !ok {
		err := fmt.Errorf("unknown job kind %q", job.Kind)
		return true, w.fail(ctx, job.ID, job.Attempts, err)
	}

	if runErr := w.run(ctx, handler, job.Payload); runErr != nil {
		attempts := job.Attempts + 1
		if attempts >= MaxAttempts {
			return true, w.fail(ctx, job.ID, attempts, runErr)
		}
		delay := Backoff[min(int(job.Attempts), len(Backoff)-1)]
		return true, w.q.RescheduleJobRun(ctx, query.RescheduleJobRunParams{
			Attempts:  attempts,
			RunAt:     now.Add(delay).Unix(),
			LastError: sql.NullString{String: runErr.Error(), Valid: true},
			UpdatedAt: now.Unix(),
			ID:        job.ID,
		})
	}
	return true, w.q.CompleteJobRun(ctx, query.CompleteJobRunParams{UpdatedAt: now.Unix(), ID: job.ID})
}

// run invokes fn, converting a panic into an error.
func (w *Worker) run(ctx context.Context, fn Handler, payload sql.NullString) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	var raw json.RawMessage
	if payload.Valid {
		raw = json.RawMessage(payload.String)
	}
	return fn(ctx, raw)
}

func (w *Worker) fail(ctx context.Context, id, attempts int64, cause error) error {
	return w.q.FailJobRun(ctx, query.FailJobRunParams{
		Attempts:  attempts,
		LastError: sql.NullString{String: cause.Error(), Valid: true},
		UpdatedAt: w.now().UTC().Unix(),
		ID:        id,
	})
}
