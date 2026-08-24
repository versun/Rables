package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/service/activity"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newTestWorker(d *sql.DB, now time.Time) *Worker {
	w := NewWorker(d)
	w.now = func() time.Time { return now }
	return w
}

func getJobRun(t *testing.T, d *sql.DB, id int64) query.JobRun {
	t.Helper()
	var job query.JobRun
	err := d.QueryRow(
		"SELECT id, kind, payload, run_at, status, attempts, last_error, created_at, updated_at FROM job_runs WHERE id = ?",
		id,
	).Scan(&job.ID, &job.Kind, &job.Payload, &job.RunAt, &job.Status, &job.Attempts, &job.LastError, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		t.Fatalf("load job_run %d: %v", id, err)
	}
	return job
}

func TestEnqueueAndRunOnceSuccess(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	w := newTestWorker(d, now)
	enq := NewEnqueuer(d)

	var got json.RawMessage
	w.Register(KindCrosspost, func(_ context.Context, payload json.RawMessage) error {
		got = payload
		return nil
	})

	id, err := enq.Enqueue(t.Context(), KindCrosspost, map[string]any{"article_id": 42}, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job := getJobRun(t, d, id)
	if job.Status != "queued" || job.Attempts != 0 {
		t.Fatalf("after enqueue: status=%q attempts=%d", job.Status, job.Attempts)
	}

	ran, err := w.RunOnce(t.Context())
	if err != nil || !ran {
		t.Fatalf("RunOnce = (%v, %v), want (true, nil)", ran, err)
	}
	job = getJobRun(t, d, id)
	if job.Status != "done" {
		t.Errorf("status = %q, want done", job.Status)
	}
	var payload struct {
		ArticleID int `json:"article_id"`
	}
	if err := json.Unmarshal(got, &payload); err != nil || payload.ArticleID != 42 {
		t.Errorf("payload = %s, err = %v", got, err)
	}
}

// TestRunOnceLinksHandlerActivity: activity rows a handler writes through its
// ctx link back to the job run, so /admin/jobs can show them as the run's log.
func TestRunOnceLinksHandlerActivity(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	w := newTestWorker(d, now)

	w.Register(KindCrosspost, func(ctx context.Context, _ json.RawMessage) error {
		activity.Log(ctx, d, "info", "posted", "crosspost", "platforms=mastodon")
		return nil
	})

	id, err := NewEnqueuer(d).Enqueue(t.Context(), KindCrosspost, nil, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if ran, err := w.RunOnce(t.Context()); err != nil || !ran {
		t.Fatalf("RunOnce = (%v, %v), want (true, nil)", ran, err)
	}

	rows, err := query.New(d).ListRecentActivityLogs(t.Context())
	if err != nil {
		t.Fatalf("list activity logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !rows[0].JobRunID.Valid || rows[0].JobRunID.Int64 != id {
		t.Errorf("job_run_id = %v, want %d", rows[0].JobRunID, id)
	}
}

func TestRunOnceSkipsFutureJob(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	w := newTestWorker(d, now)
	w.Register(KindCrosspost, func(context.Context, json.RawMessage) error { return nil })

	id, err := NewEnqueuer(d).Enqueue(t.Context(), KindCrosspost, nil, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ran, err := w.RunOnce(t.Context())
	if err != nil || ran {
		t.Fatalf("RunOnce = (%v, %v), want (false, nil)", ran, err)
	}
	if job := getJobRun(t, d, id); job.Status != "queued" {
		t.Errorf("status = %q, want queued", job.Status)
	}
}

func TestRunOnceBackoffOnFailure(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	w := newTestWorker(d, now)
	w.Register(KindExport, func(context.Context, json.RawMessage) error { return errors.New("boom") })

	id, err := NewEnqueuer(d).Enqueue(t.Context(), KindExport, nil, now)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for i, want := range Backoff[:MaxAttempts-1] {
		ran, err := w.RunOnce(t.Context())
		if err != nil || !ran {
			t.Fatalf("failure %d: RunOnce = (%v, %v)", i+1, ran, err)
		}
		job := getJobRun(t, d, id)
		if job.Status != "queued" {
			t.Errorf("failure %d: status = %q, want queued", i+1, job.Status)
		}
		if job.Attempts != int64(i+1) {
			t.Errorf("failure %d: attempts = %d, want %d", i+1, job.Attempts, i+1)
		}
		if job.RunAt != now.Add(want).Unix() {
			t.Errorf("failure %d: run_at = %d, want now+%s (%d)", i+1, job.RunAt, want, now.Add(want).Unix())
		}
		if !job.LastError.Valid || job.LastError.String != "boom" {
			t.Errorf("failure %d: last_error = %+v", i+1, job.LastError)
		}
		// Advance past the backoff so the rescheduled job is due again.
		now = now.Add(want)
		w.now = func() time.Time { return now }
	}
}

func TestRunOnceFailsAtMaxAttempts(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	w := newTestWorker(d, now)
	w.Register(KindExport, func(context.Context, json.RawMessage) error { return errors.New("boom") })

	id, err := NewEnqueuer(d).Enqueue(t.Context(), KindExport, nil, now)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := d.Exec("UPDATE job_runs SET attempts = ? WHERE id = ?", MaxAttempts-1, id); err != nil {
		t.Fatalf("set attempts: %v", err)
	}
	ran, err := w.RunOnce(t.Context())
	if err != nil || !ran {
		t.Fatalf("RunOnce = (%v, %v)", ran, err)
	}
	job := getJobRun(t, d, id)
	if job.Status != "failed" {
		t.Errorf("status = %q, want failed", job.Status)
	}
	if job.Attempts != MaxAttempts {
		t.Errorf("attempts = %d, want %d", job.Attempts, MaxAttempts)
	}
	if !job.LastError.Valid || job.LastError.String != "boom" {
		t.Errorf("last_error = %+v", job.LastError)
	}
}

func TestRunOnceUnknownKindFails(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	w := newTestWorker(d, now)

	id, err := NewEnqueuer(d).Enqueue(t.Context(), "no_such_kind", nil, now)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ran, err := w.RunOnce(t.Context())
	if err != nil || !ran {
		t.Fatalf("RunOnce = (%v, %v)", ran, err)
	}
	job := getJobRun(t, d, id)
	if job.Status != "failed" {
		t.Errorf("status = %q, want failed", job.Status)
	}
	if !job.LastError.Valid || job.LastError.String == "" {
		t.Errorf("last_error = %+v, want set", job.LastError)
	}
}

func TestWorkerStartPolls(t *testing.T) {
	d := openDB(t)
	w := NewWorker(d)
	w.PollInterval = 10 * time.Millisecond

	done := make(chan struct{})
	w.Register(KindPublishArticle, func(context.Context, json.RawMessage) error {
		close(done)
		return nil
	})
	if _, err := NewEnqueuer(d).Enqueue(t.Context(), KindPublishArticle, nil, time.Now()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Start(ctx)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not execute the job within 5s")
	}
}

// A handler aborted by shutdown (ctx cancelled, e.g. SIGTERM) did not fail:
// the job is requeued without consuming an attempt or adding backoff, so
// frequent deploys cannot push a healthy job to MaxAttempts. A real timeout
// (context.DeadlineExceeded) still counts as a failure.
func TestRunOnceCancellationVsFailure(t *testing.T) {
	tests := []struct {
		name         string
		handlerErr   error
		wantAttempts int64
		wantDelay    time.Duration // 0 means immediately due again
	}{
		{"context.Canceled requeues without attempt", context.Canceled, 0, 0},
		{"context.DeadlineExceeded counts as failure", context.DeadlineExceeded, 1, Backoff[0]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openDB(t)
			now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
			w := newTestWorker(d, now)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w.Register(KindExport, func(context.Context, json.RawMessage) error {
				cancel() // simulate SIGTERM landing while the handler runs
				return tt.handlerErr
			})

			id, err := NewEnqueuer(d).Enqueue(t.Context(), KindExport, nil, now)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			ran, err := w.RunOnce(ctx)
			if err != nil || !ran {
				t.Fatalf("RunOnce = (%v, %v), want (true, nil)", ran, err)
			}
			job := getJobRun(t, d, id)
			if job.Status != "queued" {
				t.Errorf("status = %q, want queued", job.Status)
			}
			if job.Attempts != tt.wantAttempts {
				t.Errorf("attempts = %d, want %d", job.Attempts, tt.wantAttempts)
			}
			if want := now.Add(tt.wantDelay).Unix(); job.RunAt != want {
				t.Errorf("run_at = %d, want now+%s (%d)", job.RunAt, tt.wantDelay, want)
			}
		})
	}
}

// Only a cancellation of the worker's own ctx (shutdown) earns the free
// requeue. A handler returning context.Canceled produced by an internal ctx
// it cancelled itself — while the worker ctx is still alive — hit a real
// failure: it must consume an attempt and back off, or it would be requeued
// for free forever.
func TestRunOnceHandlerInternalCancellationCountsAsFailure(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	w := newTestWorker(d, now)
	w.Register(KindExport, func(context.Context, json.RawMessage) error {
		return fmt.Errorf("inner ctx: %w", context.Canceled)
	})

	id, err := NewEnqueuer(d).Enqueue(t.Context(), KindExport, nil, now)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ran, err := w.RunOnce(t.Context())
	if err != nil || !ran {
		t.Fatalf("RunOnce = (%v, %v), want (true, nil)", ran, err)
	}
	job := getJobRun(t, d, id)
	if job.Status != "queued" {
		t.Errorf("status = %q, want queued", job.Status)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (internal cancellation is a failure)", job.Attempts)
	}
	if want := now.Add(Backoff[0]).Unix(); job.RunAt != want {
		t.Errorf("run_at = %d, want now+%s (%d)", job.RunAt, Backoff[0], want)
	}
}

// The handler runs with the caller's ctx, but the post-run bookkeeping must
// survive a cancellation that arrives mid-run (SIGTERM during a job):
// otherwise the row would stay running until the startup reaper fires.
func TestRunOnceBookkeepingSurvivesCancellation(t *testing.T) {
	tests := []struct {
		name       string
		handlerErr error
		wantStatus string
	}{
		{"failure is rescheduled", errors.New("boom"), "queued"},
		{"success is completed", nil, "done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openDB(t)
			now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
			w := newTestWorker(d, now)

			ctx, cancel := context.WithCancel(t.Context())
			w.Register(KindExport, func(context.Context, json.RawMessage) error {
				cancel() // simulate SIGTERM landing while the handler runs
				return tt.handlerErr
			})

			id, err := NewEnqueuer(d).Enqueue(t.Context(), KindExport, nil, now)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			ran, err := w.RunOnce(ctx)
			if err != nil || !ran {
				t.Fatalf("RunOnce = (%v, %v), want (true, nil)", ran, err)
			}
			job := getJobRun(t, d, id)
			if job.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q (bookkeeping must outlive the cancelled ctx)", job.Status, tt.wantStatus)
			}
			if tt.handlerErr != nil && job.Attempts != 1 {
				t.Errorf("attempts = %d, want 1", job.Attempts)
			}
		})
	}
}

// An enqueued due job must not wait out the poll interval when the worker
// was nudged: Enqueue with SetWake triggers an immediate Start poll.
func TestWorkerWakePollsImmediately(t *testing.T) {
	d := openDB(t)
	w := NewWorker(d)
	w.PollInterval = time.Hour // the tick must not fire during the test

	ran := make(chan struct{})
	w.Register(KindExport, func(context.Context, json.RawMessage) error {
		close(ran)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Start(ctx)

	enq := NewEnqueuer(d)
	enq.SetWake(w.Wake)
	if _, err := enq.Enqueue(t.Context(), KindExport, nil, time.Now()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Error("job did not run within 5s of the wake nudge")
	}
}

// One wake drains every due job, so a burst of enqueues whose nudges
// coalesced (crossposts on publish fan out one job per platform) does not
// trickle out one job per poll.
func TestWorkerWakeDrainsDueJobs(t *testing.T) {
	d := openDB(t)
	w := NewWorker(d)
	w.PollInterval = time.Hour // the tick must not fire during the test

	ran := make(chan struct{}, 2)
	w.Register(KindCrosspost, func(context.Context, json.RawMessage) error {
		ran <- struct{}{}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Start(ctx)

	enq := NewEnqueuer(d)
	enq.SetWake(w.Wake)
	for i := 0; i < 2; i++ {
		if _, err := enq.Enqueue(t.Context(), KindCrosspost, nil, time.Now()); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ran:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 due jobs ran within 5s of the wake", i)
		}
	}
}

// A handler aborted mid-drain by shutdown is requeued due-immediately; the
// drain must notice the cancelled ctx and stop instead of reclaiming that
// row in a loop.
func TestWorkerDrainStopsOnShutdown(t *testing.T) {
	d := openDB(t)
	w := NewWorker(d)
	w.PollInterval = time.Hour // the tick must not fire during the test

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stopped := make(chan struct{})
	w.Register(KindExport, func(context.Context, json.RawMessage) error {
		cancel() // SIGTERM lands while the handler runs
		return context.Canceled
	})
	go func() {
		w.Start(ctx)
		close(stopped)
	}()

	if _, err := NewEnqueuer(d).Enqueue(t.Context(), KindExport, nil, time.Now()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	w.Wake()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after shutdown during a drain")
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM job_runs`).Scan(&status); err != nil {
		t.Fatalf("query job_runs: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued (free requeue after shutdown)", status)
	}
}
