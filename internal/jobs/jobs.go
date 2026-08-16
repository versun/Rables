// Package jobs implements the job_runs worker and cron scheduler that
// replace Solid Queue (plan §5).
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"rables/internal/db/query"
)

// Job kinds — the dispatch whitelist from the job_runs.kind comment (plan §3).
const (
	KindPublishArticle           = "publish_article"
	KindPublishPage              = "publish_page"
	KindSendNewsletter           = "send_newsletter"
	KindCrosspost                = "crosspost"
	KindFetchSocialComments      = "fetch_social_comments"
	KindExport                   = "export"
	KindImportDB                 = "import_db"
	KindImportRails              = "import_rails"
	KindImportRSS                = "import_rss"
	KindCommentReplyNotification = "comment_reply_notification"
	KindNewsletterConfirmation   = "newsletter_confirmation"
	KindPasswordReset            = "password_reset"
	KindTwitterSync              = "twitter_sync"
)

// Enqueuer inserts queued job_runs rows.
type Enqueuer struct {
	q *query.Queries
	// wake, installed via SetWake, is invoked after a row is inserted so the
	// worker polls immediately instead of discovering the job on its next
	// tick (up to PollInterval late).
	wake func()
}

// NewEnqueuer returns an Enqueuer backed by db.
func NewEnqueuer(db *sql.DB) *Enqueuer {
	return &Enqueuer{q: query.New(db)}
}

// SetWake installs the post-insert nudge (Worker.Wake in main). Without it,
// enqueued jobs wait for the worker's next poll tick. It is not synchronized:
// install it before the first Enqueue, as main does at startup.
func (e *Enqueuer) SetWake(wake func()) { e.wake = wake }

// Enqueue marshals payload to JSON and queues kind for execution at or after
// runAt. A nil payload is stored as SQL NULL. It returns the new row id. A
// successful insert fires the SetWake nudge, if any, so the worker polls
// immediately instead of discovering the job up to PollInterval late.
func (e *Enqueuer) Enqueue(ctx context.Context, kind string, payload any, runAt time.Time) (int64, error) {
	var p sql.NullString
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("marshal payload: %w", err)
		}
		p = sql.NullString{String: string(b), Valid: true}
	}
	now := time.Now().UTC().Unix()
	id, err := e.q.EnqueueJobRun(ctx, query.EnqueueJobRunParams{
		Kind:      kind,
		Payload:   p,
		RunAt:     runAt.UTC().Unix(),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err == nil && e.wake != nil {
		e.wake()
	}
	return id, err
}

// EnqueueUnlessActive queues kind like Enqueue, but atomically skips the
// insert when a queued or running job of the same kind already exists,
// reporting whether the row was enqueued. For singleton jobs (twitter_sync):
// the scheduler re-fires on a fixed cadence while the due timestamp
// (last_synced_at) only advances after a successful run, so without dedup a
// backed-up worker would accumulate duplicate rows.
func (e *Enqueuer) EnqueueUnlessActive(ctx context.Context, kind string, runAt time.Time) (bool, error) {
	now := time.Now().UTC().Unix()
	n, err := e.q.EnqueueJobRunUnlessActive(ctx, query.EnqueueJobRunUnlessActiveParams{
		Kind:      kind,
		RunAt:     runAt.UTC().Unix(),
		CreatedAt: now,
		UpdatedAt: now,
	})
	// A skipped insert (n == 0) means an active job already covers this run,
	// so there is nothing new to wake for.
	if err == nil && n > 0 && e.wake != nil {
		e.wake()
	}
	return n > 0, err
}
