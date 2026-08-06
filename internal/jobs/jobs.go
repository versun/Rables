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
	KindTwitterArchiveImport     = "twitter_archive_import"
	KindCommentReplyNotification = "comment_reply_notification"
	KindNewsletterConfirmation   = "newsletter_confirmation"
	KindPasswordReset            = "password_reset"
)

// Enqueuer inserts queued job_runs rows.
type Enqueuer struct {
	q *query.Queries
}

// NewEnqueuer returns an Enqueuer backed by db.
func NewEnqueuer(db *sql.DB) *Enqueuer {
	return &Enqueuer{q: query.New(db)}
}

// Enqueue marshals payload to JSON and queues kind for execution at or after
// runAt. A nil payload is stored as SQL NULL. It returns the new row id.
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
	return e.q.EnqueueJobRun(ctx, query.EnqueueJobRunParams{
		Kind:      kind,
		Payload:   p,
		RunAt:     runAt.UTC().Unix(),
		CreatedAt: now,
		UpdatedAt: now,
	})
}
