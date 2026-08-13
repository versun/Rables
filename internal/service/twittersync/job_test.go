package twittersync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"rables/internal/jobs"
)

// A twitter_sync run aborted by shutdown (SIGTERM cancels the worker ctx
// mid-run) must not be marked done: the handler surfaces the cancellation,
// so the worker requeues the job for free and the next process runs it.
func TestSyncJobRequeuedOnShutdown(t *testing.T) {
	database := newTestDB(t)
	enableSync(t, database, "alice")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	arrived := make(chan struct{})
	api := (&fakeX{
		t:      t,
		userID: "42",
		onTimeline: func() {
			close(arrived)
			<-ctx.Done() // hold the run until "SIGTERM" lands
		},
	}).server()
	syncer := newSyncer(database, t.TempDir(), api)

	worker := jobs.NewWorker(database)
	RegisterSyncHandler(worker, syncer)
	if _, err := jobs.NewEnqueuer(database).Enqueue(t.Context(), jobs.KindTwitterSync, nil, time.Now()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	runDone := make(chan error, 1)
	go func() {
		claimed, err := worker.RunOnce(ctx)
		if err == nil && !claimed {
			err = fmt.Errorf("job not claimed")
		}
		runDone <- err
	}()
	<-arrived
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var status string
	var attempts int64
	if err := database.QueryRow(`SELECT status, attempts FROM job_runs`).Scan(&status, &attempts); err != nil {
		t.Fatalf("query job_runs: %v", err)
	}
	if status != "queued" || attempts != 0 {
		t.Errorf("job = %q/attempts %d, want queued/0 (free requeue after shutdown)", status, attempts)
	}
}
