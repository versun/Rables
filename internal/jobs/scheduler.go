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
	"strings"
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
	// ctx is the process shutdown context (the signal ctx in main); every
	// scheduled task runs with it so SIGTERM interrupts in-flight work.
	ctx context.Context
	// stopTimeout bounds how long Stop waits for a running task to finish.
	stopTimeout time.Duration

	hooks  map[string]Hook
	now    func() time.Time
	logger *slog.Logger
}

// NewScheduler builds a Scheduler rooted at dataDir (export/import zips live
// under dataDir/exports and dataDir/imports) and registers the four cron
// entries. Tasks run with ctx, so cancelling it (SIGTERM in main) aborts
// in-flight work such as the sync_twitter network fetch.
func NewScheduler(ctx context.Context, db *sql.DB, dataDir string) *Scheduler {
	s := &Scheduler{
		cron:        cron.New(),
		enq:         NewEnqueuer(db),
		q:           query.New(db),
		kv:          kv.NewStore(db),
		dataDir:     dataDir,
		ctx:         ctx,
		stopTimeout: 15 * time.Second,
		hooks:       map[string]Hook{},
		now:         time.Now,
		logger:      slog.Default(),
	}
	for spec, task := range map[string]func(context.Context) error{
		"12 * * * *":   s.cleanupFinishedJobRuns,
		"0 3 * * *":    s.cleanOldExports,
		"30 3 * * *":   s.deleteExpiredSessions,
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

// SetWake installs the worker nudge on the scheduler's own enqueuer, so a due
// comment fetch enqueued by the hourly tick starts immediately instead of up
// to one worker poll interval late.
func (s *Scheduler) SetWake(wake func()) { s.enq.SetWake(wake) }

// Start launches the cron goroutine.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop halts the cron goroutine, waiting for running jobs to finish. The
// wait is bounded by stopTimeout (15s, matching the worker wait in main) so
// a task that ignores the cancelled ctx cannot drag shutdown to SIGKILL.
func (s *Scheduler) Stop() {
	select {
	case <-s.cron.Stop().Done():
	case <-time.After(s.stopTimeout):
		s.logger.Warn("scheduler stop timed out; continuing shutdown", "timeout", s.stopTimeout)
	}
}

func (s *Scheduler) run(task func(context.Context) error) {
	// cron runs jobs on bare goroutines with no recovery (robfig/cron
	// startJob), so an unrecovered panic in a task would crash the whole
	// process. Convert it to a log line like the worker does for handlers.
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("scheduled task panicked", "panic", r)
		}
	}()
	if err := task(s.ctx); err != nil {
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

// deleteExpiredSessions deletes sessions older than 30 days, matching
// sessionTTL in internal/httpd: expired rows are normally deleted when the
// token is presented, so this sweeps the ones that never get used again.
func (s *Scheduler) deleteExpiredSessions(ctx context.Context) error {
	cutoff := s.now().UTC().AddDate(0, 0, -30).Unix()
	_, err := s.q.DeleteExpiredSessions(ctx, cutoff)
	return err
}

// cleanOldExports deletes export zips, stale export_* staging directories
// (SIGKILL/OOM leftovers of transfer.stagingDir, which hold a full database
// copy), and job-owned import uploads older than 7 days, plus activity_logs
// rows older than 90 days (CleanOldExportsJob). One entry — or a whole
// directory — that cannot be inspected or removed is reported but does not
// stop the sweep, and the activity-log prune always runs.
//
// An import upload still referenced by a queued/running import job is kept
// no matter its age, the same protection CleanupOrphanImportFiles applies at
// startup: a queued job re-reads its source file when a worker picks it up,
// so unlinking it would fail the import. When the reference lookup itself
// fails, no import_* / twitter_archive_* file is removed in that run.
// twitter_archive_* uploads are leftovers of the removed archive feature
// (migration 0006): nothing references them anymore, so the 7-day rule
// always reclaims them.
func (s *Scheduler) cleanOldExports(ctx context.Context) error {
	cutoff := s.now().UTC().Add(-7 * 24 * time.Hour)
	exportsDir := filepath.Join(s.dataDir, "exports")
	importsDir := filepath.Join(s.dataDir, "imports")
	var firstErr error
	protected, _, err := activeImportPaths(ctx, s.q)
	if err != nil {
		firstErr = fmt.Errorf("list active import paths: %w", err)
		protected = nil
	}
	for _, dir := range []string{exportsDir, importsDir} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// An unreadable directory is reported like a bad entry: the
			// other directory and the activity-log prune still run.
			if firstErr == nil {
				firstErr = fmt.Errorf("read %s: %w", dir, err)
			}
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				// A stale export_* dir is the stagingDir leftover of an
				// export killed between VACUUM INTO and the deferred
				// RemoveAll; reap it by the same 7-day rule. Other dirs
				// (extract_* in imports/) belong to CleanupOrphanImportFiles.
				if dir != exportsDir || !strings.HasPrefix(name, "export_") {
					continue
				}
			} else {
				if filepath.Ext(name) != ".zip" {
					continue
				}
				// In imports/ only job-owned uploads (import_* web uploads,
				// twitter_archive_* archive-upload leftovers) are reaped; any
				// other zip is a server-side file the admin copied there for
				// import_server, which nothing else ever cleans up.
				if dir == importsDir &&
					!strings.HasPrefix(name, "import_") && !strings.HasPrefix(name, "twitter_archive_") {
					continue
				}
			}
			info, err := entry.Info()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("stat %s/%s: %w", dir, name, err)
				}
				continue
			}
			if !info.ModTime().Before(cutoff) {
				continue
			}
			path := filepath.Join(dir, name)
			// A job-owned upload in imports/ that an active import still
			// references is kept no matter its age. protected == nil means
			// the reference lookup failed: skip every upload rather than
			// risk unlinking a file a queued job still needs.
			if dir == importsDir && (protected == nil || protected[path]) {
				continue
			}
			remove := os.Remove
			if entry.IsDir() {
				remove = os.RemoveAll
			}
			if err := remove(path); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("remove %s/%s: %w", dir, name, err)
				}
				continue
			}
		}
	}
	if _, err := s.q.DeleteOldActivityLogs(ctx, s.now().UTC().AddDate(0, 0, -90).Unix()); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// enqueueDueCommentFetches enqueues fetch_social_comments for every platform
// whose comment_fetch_schedule is due (ScheduledFetchSocialCommentsJob). The
// last-fetch timestamp is stamped only after a successful enqueue, so a failed
// enqueue does not silently postpone the platform by a whole schedule period
// (a failed stamp after a successful enqueue just re-enqueues next run, which
// the fetch handler tolerates).
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
		if _, err := s.enq.Enqueue(ctx, KindFetchSocialComments, map[string]string{"platform": f.Platform}, now); err != nil {
			return err
		}
		if err := s.kv.Set(ctx, key, strconv.FormatInt(now.Unix(), 10)); err != nil {
			return err
		}
	}
	return nil
}

// runSyncTwitterHook invokes the registered "sync_twitter" hook when the
// twitter_syncs row is enabled and due (SyncTwitterJob wake-up). The "daily"
// schedule is wall-clock based (08:00 in settings.time_zone), so the site
// time zone is loaded each tick; a missing row or an unknown zone falls back
// to UTC, matching the admin time displays.
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
	loc := time.UTC
	if settings, err := s.q.GetSettings(ctx); err == nil {
		if l, err := time.LoadLocation(settings.TimeZone); err == nil {
			loc = l
		}
	}
	if !TwitterSyncDue(sync.SyncSchedule, lastSynced, s.now().UTC(), loc) {
		return nil
	}
	hook := s.hooks["sync_twitter"]
	if hook == nil {
		return nil
	}
	return hook(ctx)
}
