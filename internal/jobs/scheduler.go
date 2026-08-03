package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"

	"rables/internal/db/query"
	"rables/internal/kv"
)

// Hook is a named callback invoked by a scheduled task.
type Hook func(ctx context.Context) error

// Scheduler runs the recurring tasks from plan §5 (aligned with
// config/recurring.yml production).
type Scheduler struct {
	cron    *cron.Cron
	enq     *Enqueuer
	q       *query.Queries
	kv      *kv.Store
	dataDir string

	hooks  map[string]Hook
	now    func() time.Time
	logger *slog.Logger
}

// NewScheduler builds a Scheduler rooted at dataDir (export/import zips live
// under dataDir/exports and dataDir/imports) and registers the four cron
// entries.
func NewScheduler(db *sql.DB, dataDir string) *Scheduler {
	s := &Scheduler{
		cron:    cron.New(),
		enq:     NewEnqueuer(db),
		q:       query.New(db),
		kv:      kv.NewStore(db),
		dataDir: dataDir,
		hooks:   map[string]Hook{},
		now:     time.Now,
		logger:  slog.Default(),
	}
	for spec, task := range map[string]func(context.Context) error{
		"12 * * * *":   s.cleanupFinishedJobRuns,
		"0 3 * * *":    s.cleanOldExports,
		"0 * * * *":    s.enqueueDueCommentFetches,
		"*/15 * * * *": s.runSyncTwitterHook,
	} {
		if _, err := s.cron.AddFunc(spec, func() { s.run(task) }); err != nil {
			// The specs above are fixed literals; a failure here is a bug.
			panic(fmt.Sprintf("cron spec %q: %v", spec, err))
		}
	}
	return s
}

// RegisterHook installs fn under name (currently only "sync_twitter"),
// replacing the default no-op.
func (s *Scheduler) RegisterHook(name string, fn Hook) {
	s.hooks[name] = fn
}

// Start launches the cron goroutine.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop halts the cron goroutine, waiting for running jobs to finish.
func (s *Scheduler) Stop() { <-s.cron.Stop().Done() }

func (s *Scheduler) run(task func(context.Context) error) {
	if err := task(context.Background()); err != nil {
		s.logger.Error("scheduled task failed", "error", err)
	}
}

// cleanupFinishedJobRuns deletes done/failed job_runs older than 24h
// (replaces Solid Queue clear_finished).
func (s *Scheduler) cleanupFinishedJobRuns(ctx context.Context) error {
	cutoff := s.now().UTC().Add(-24 * time.Hour).Unix()
	_, err := s.q.DeleteFinishedJobRuns(ctx, cutoff)
	return err
}

// cleanOldExports deletes export/import zips older than 7 days and
// activity_logs rows older than 90 days (CleanOldExportsJob).
func (s *Scheduler) cleanOldExports(ctx context.Context) error {
	cutoff := s.now().UTC().Add(-7 * 24 * time.Hour)
	for _, dir := range []string{filepath.Join(s.dataDir, "exports"), filepath.Join(s.dataDir, "imports")} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".zip" {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("stat %s/%s: %w", dir, entry.Name(), err)
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
					return fmt.Errorf("remove %s/%s: %w", dir, entry.Name(), err)
				}
			}
		}
	}
	_, err := s.q.DeleteOldActivityLogs(ctx, s.now().UTC().AddDate(0, 0, -90).Unix())
	return err
}

// enqueueDueCommentFetches enqueues fetch_social_comments for every platform
// whose comment_fetch_schedule is due (ScheduledFetchSocialCommentsJob). The
// last-fetch timestamp is stamped before enqueueing to avoid duplicate runs.
func (s *Scheduler) enqueueDueCommentFetches(ctx context.Context) error {
	fetchers, err := s.q.ListCommentFetchers(ctx)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, f := range fetchers {
		key := "last_fetch_comments_" + f.Platform
		var lastFetch *time.Time
		if value, found, err := s.kv.Get(ctx, key); err != nil {
			return err
		} else if found {
			if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
				t := time.Unix(unix, 0).UTC()
				lastFetch = &t
			}
		}
		if !CommentFetchDue(f.CommentFetchSchedule.String, lastFetch, now) {
			continue
		}
		if err := s.kv.Set(ctx, key, strconv.FormatInt(now.Unix(), 10)); err != nil {
			return err
		}
		if _, err := s.enq.Enqueue(ctx, KindFetchSocialComments, map[string]string{"platform": f.Platform}, now); err != nil {
			return err
		}
	}
	return nil
}

// runSyncTwitterHook invokes the registered "sync_twitter" hook when the
// twitter_syncs row is enabled and due (SyncTwitterJob wake-up).
func (s *Scheduler) runSyncTwitterHook(ctx context.Context) error {
	sync, err := s.q.GetTwitterSync(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if sync.Enabled != 1 {
		return nil
	}
	var lastSynced *time.Time
	if sync.LastSyncedAt.Valid {
		t := time.Unix(sync.LastSyncedAt.Int64, 0).UTC()
		lastSynced = &t
	}
	if !TwitterSyncDue(sync.SyncSchedule, lastSynced, s.now().UTC()) {
		return nil
	}
	hook := s.hooks["sync_twitter"]
	if hook == nil {
		return nil
	}
	return hook(ctx)
}
