package twittersync

import (
	"context"
	"encoding/json"

	"rables/internal/jobs"
)

// RegisterSyncHandler installs the twitter_sync job handler (SyncTwitterJob):
// both the recurring scheduler wake-up and the admin "Sync Now" button
// enqueue this job instead of running a sync inline, so every run is visible
// under /admin/jobs. Run swallows sync failures into the twitter_syncs row
// (last_error), so the job itself completes either way — mirroring the Rails
// job, which records the error and does not retry. The one error the handler
// does surface is a shutdown abort: when the worker ctx is cancelled
// mid-run, the returned ctx.Err() lets the worker requeue the job for free
// instead of marking an unfinished sync done.
func RegisterSyncHandler(w *jobs.Worker, syncer *Syncer) {
	w.Register(jobs.KindTwitterSync, func(ctx context.Context, _ json.RawMessage) error {
		if err := syncer.Run(ctx); err != nil {
			return err
		}
		return ctx.Err()
	})
}
