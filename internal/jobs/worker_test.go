package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/db/query"
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
