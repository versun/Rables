package crosspost

import (
	"context"
	"errors"
	"log/slog"

	"rables/internal/db/query"
)

// xiaohongshuPlatform mirrors the Rails status quo for Xiaohongshu: there is
// no public API, so a selected crosspost is only logged
// (article.crosspost_skipped with reason no_public_api) and never posted.
type xiaohongshuPlatform struct{}

func init() { RegisterPlatform(xiaohongshuPlatform{}) }

func (xiaohongshuPlatform) Name() string { return "xiaohongshu" }

// Verify mirrors the Rails controller's unknown-platform branch: verification
// always fails with the generic message.
func (xiaohongshuPlatform) Verify(_ context.Context, _ query.Crosspost) error {
	return errors.New("Verification failed. Please check your settings and try again.")
}

// Post only logs; no URL is recorded, like CrosspostArticleJob's case
// falling through for xiaohongshu.
func (xiaohongshuPlatform) Post(_ context.Context, _ query.Crosspost, in PostInput) (string, error) {
	slog.Info("crosspost: xiaohongshu is log-only (no public api)", "article_id", in.ArticleID, "title", in.Title)
	return "", nil
}
