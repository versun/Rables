package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
)

// Publish payload contracts. The newsletter payload is shared with the
// direct "send newsletter" enqueue path, so it stays {"article_id": <id>}.
type publishArticlePayload struct {
	ArticleID int64 `json:"article_id"`
}

type publishPagePayload struct {
	PageID int64 `json:"page_id"`
}

type publishCrosspostPayload struct {
	ArticleID int64    `json:"article_id"`
	Platforms []string `json:"platforms"`
}

// RegisterPublishHandlers installs the publish_article and publish_page
// handlers (plan §4.1; Rails PublishScheduledArticlesJob /
// PublishScheduledPagesJob + Article#publish_scheduled /
// Page#publish_scheduled).
func RegisterPublishHandlers(w *Worker, db *sql.DB, enq *Enqueuer) {
	q := query.New(db)
	w.Register(KindPublishArticle, publishArticleHandler(q, enq))
	w.Register(KindPublishPage, publishPageHandler(q))
}

func publishArticleHandler(q *query.Queries, enq *Enqueuer) Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p publishArticlePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("decode publish_article payload: %w", err)
		}
		state, err := q.GetArticlePublishState(ctx, p.ArticleID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // Rails rescues RecordNotFound and returns
		}
		if err != nil {
			return fmt.Errorf("load article %d: %w", p.ArticleID, err)
		}
		now := time.Now().UTC()
		// Rails should_publish?: schedule? && scheduled_at <= now. The job
		// also skips stale rows whose scheduled_at was cleared.
		if state.Status != int64(domain.StatusSchedule) || !state.ScheduledAt.Valid || state.ScheduledAt.Int64 > now.Unix() {
			return nil
		}
		flipped, err := q.PublishScheduledArticle(ctx, query.PublishScheduledArticleParams{
			PublishStatus:  int64(domain.StatusPublish),
			CreatedAt:      state.ScheduledAt.Int64,
			UpdatedAt:      now.Unix(),
			ID:             p.ArticleID,
			ScheduleStatus: int64(domain.StatusSchedule),
		})
		if err != nil {
			return fmt.Errorf("publish article %d: %w", p.ArticleID, err)
		}
		if flipped == 0 {
			return nil // already flipped; snapshot was consumed by the first run
		}
		// The guarded update cleared the snapshot, so from here it is consumed
		// exactly once: enqueue the follow-up jobs from the pre-update values.
		var platforms []string
		if err := json.Unmarshal([]byte(state.ScheduledCrosspostPlatforms), &platforms); err != nil {
			return fmt.Errorf("article %d: parse scheduled_crosspost_platforms: %w", p.ArticleID, err)
		}
		if len(platforms) > 0 {
			if _, err := enq.Enqueue(ctx, KindCrosspost, publishCrosspostPayload{ArticleID: p.ArticleID, Platforms: platforms}, now); err != nil {
				return fmt.Errorf("enqueue crosspost for article %d: %w", p.ArticleID, err)
			}
		}
		if state.ScheduledSendNewsletter != 0 {
			if _, err := enq.Enqueue(ctx, KindSendNewsletter, publishArticlePayload{ArticleID: p.ArticleID}, now); err != nil {
				return fmt.Errorf("enqueue newsletter for article %d: %w", p.ArticleID, err)
			}
		}
		return nil
	}
}

func publishPageHandler(q *query.Queries) Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p publishPagePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("decode publish_page payload: %w", err)
		}
		state, err := q.GetPagePublishState(ctx, p.PageID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load page %d: %w", p.PageID, err)
		}
		now := time.Now().UTC()
		if state.Status != int64(domain.StatusSchedule) || !state.ScheduledAt.Valid || state.ScheduledAt.Int64 > now.Unix() {
			return nil
		}
		// Rails Page#publish_scheduled does not backfill created_at.
		_, err = q.PublishScheduledPage(ctx, query.PublishScheduledPageParams{
			PublishStatus:  int64(domain.StatusPublish),
			UpdatedAt:      now.Unix(),
			ID:             p.PageID,
			ScheduleStatus: int64(domain.StatusSchedule),
		})
		if err != nil {
			return fmt.Errorf("publish page %d: %w", p.PageID, err)
		}
		return nil
	}
}
