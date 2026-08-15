package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"rables/internal/kv"
)

func newTestScheduler(d *sql.DB, dataDir string, now time.Time) *Scheduler {
	s := NewScheduler(context.Background(), d, dataDir)
	s.now = func() time.Time { return now }
	return s
}

func countRows(t *testing.T, d *sql.DB, where string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM job_runs "+where, args...).Scan(&n); err != nil {
		t.Fatalf("count job_runs: %v", err)
	}
	return n
}

func TestCleanupFinishedJobRuns(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	s := newTestScheduler(d, t.TempDir(), now)

	old := now.Add(-25 * time.Hour).Unix()
	recent := now.Add(-time.Hour).Unix()
	insert := func(status string, updatedAt int64) {
		t.Helper()
		if _, err := d.Exec(
			"INSERT INTO job_runs (kind, run_at, status, attempts, created_at, updated_at) VALUES ('export', 0, ?, 0, 0, ?)",
			status, updatedAt,
		); err != nil {
			t.Fatalf("insert job_run: %v", err)
		}
	}
	insert("done", old)
	insert("failed", old)
	insert("done", recent)
	insert("queued", old)
	insert("running", old)

	if err := s.cleanupFinishedJobRuns(t.Context()); err != nil {
		t.Fatalf("cleanupFinishedJobRuns: %v", err)
	}
	if n := countRows(t, d, ""); n != 3 {
		t.Errorf("remaining rows = %d, want 3 (recent done, queued, running)", n)
	}
	if n := countRows(t, d, "WHERE status IN ('done','failed')"); n != 1 {
		t.Errorf("remaining finished rows = %d, want 1", n)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	s := newTestScheduler(d, t.TempDir(), now)

	if _, err := d.Exec(
		"INSERT INTO users (user_name, password_digest, created_at, updated_at) VALUES ('admin', 'x', 0, 0)",
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	insert := func(token string, createdAt int64) {
		t.Helper()
		if _, err := d.Exec(
			"INSERT INTO sessions (token, user_id, created_at, updated_at) VALUES (?, 1, ?, ?)",
			token, createdAt, createdAt,
		); err != nil {
			t.Fatalf("insert session %s: %v", token, err)
		}
	}
	insert("expired", now.AddDate(0, 0, -31).Unix())
	insert("boundary", now.AddDate(0, 0, -30).Unix())
	insert("fresh", now.AddDate(0, 0, -1).Unix())

	if err := s.deleteExpiredSessions(t.Context()); err != nil {
		t.Fatalf("deleteExpiredSessions: %v", err)
	}
	var tokens []string
	rows, err := d.Query("SELECT token FROM sessions ORDER BY token")
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatalf("scan token: %v", err)
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sessions: %v", err)
	}
	if len(tokens) != 2 || tokens[0] != "boundary" || tokens[1] != "fresh" {
		t.Errorf("remaining sessions = %v, want [boundary fresh]", tokens)
	}
}

func TestCleanOldExports(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	s := newTestScheduler(d, dataDir, now)

	oldTime := now.Add(-8 * 24 * time.Hour)
	newTime := now.Add(-2 * 24 * time.Hour)
	files := []struct {
		dir      string
		name     string
		modTime  time.Time
		wantGone bool
	}{
		{"exports", "export_old.zip", oldTime, true},
		{"exports", "export_new.zip", newTime, false},
		{"exports", "notes.txt", oldTime, false},
		{"imports", "import_old.zip", oldTime, true},
		{"imports", "twitter_archive_old.zip", oldTime, true},
		{"imports", "import_new.zip", newTime, false},
		// A server-side file the admin copied in for import_server is not
		// job-owned and must survive no matter its age.
		{"imports", "home-backup.zip", oldTime, false},
	}
	for _, f := range files {
		dir := filepath.Join(dataDir, f.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, []byte("zip"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		if err := os.Chtimes(path, f.modTime, f.modTime); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
	insertLog := func(createdAt int64) {
		t.Helper()
		if _, err := d.Exec(
			"INSERT INTO activity_logs (level, action, created_at, updated_at) VALUES (0, 'test', ?, ?)",
			createdAt, createdAt,
		); err != nil {
			t.Fatalf("insert activity_log: %v", err)
		}
	}
	insertLog(now.AddDate(0, 0, -91).Unix())
	insertLog(now.AddDate(0, 0, -10).Unix())

	if err := s.cleanOldExports(t.Context()); err != nil {
		t.Fatalf("cleanOldExports: %v", err)
	}
	for _, f := range files {
		_, err := os.Stat(filepath.Join(dataDir, f.dir, f.name))
		if gone := os.IsNotExist(err); gone != f.wantGone {
			t.Errorf("%s/%s gone = %v, want %v", f.dir, f.name, gone, f.wantGone)
		}
	}
	var logs int
	if err := d.QueryRow("SELECT COUNT(*) FROM activity_logs").Scan(&logs); err != nil {
		t.Fatalf("count activity_logs: %v", err)
	}
	if logs != 1 {
		t.Errorf("activity_logs remaining = %d, want 1", logs)
	}
}

// A stale import_* upload still referenced by a queued import job must
// survive the cron sweep: the startup cleanup (CleanupOrphanImportFiles)
// protects it, and the worker re-reads the file when the job is picked up
// (the twitter archive import even opens the zip twice), so unlinking it
// would fail the import with "no such file".
func TestCleanOldExportsKeepsReferencedImportUpload(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	s := newTestScheduler(d, dataDir, now)
	oldTime := now.Add(-8 * 24 * time.Hour)

	referenced := writeImportFile(t, dataDir, "import_800_aaaabbbb.zip", oldTime)
	if _, err := NewEnqueuer(d).Enqueue(t.Context(), KindImportDB, map[string]any{"path": referenced}, now); err != nil {
		t.Fatalf("enqueue import_db: %v", err)
	}
	orphan := writeImportFile(t, dataDir, "import_801_ccccdddd.zip", oldTime)

	if err := s.cleanOldExports(t.Context()); err != nil {
		t.Fatalf("cleanOldExports: %v", err)
	}
	if !fileExists(referenced) {
		t.Errorf("referenced import upload was removed, want kept")
	}
	if fileExists(orphan) {
		t.Errorf("orphan import upload still exists, want removed")
	}
}

// One unremovable file must not abort the sweep: the remaining files are
// still reaped, the 90-day activity-log prune still runs, and the failure is
// reported (the firstErr pattern of CleanupOrphanImportFiles).
func TestCleanOldExportsContinuesAfterFileFailure(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	s := newTestScheduler(d, dataDir, now)
	oldTime := now.Add(-8 * 24 * time.Hour)

	// exports/ is read-only, so removing its old zip fails.
	exportsDir := filepath.Join(dataDir, "exports")
	if err := os.MkdirAll(exportsDir, 0o755); err != nil {
		t.Fatalf("mkdir exports: %v", err)
	}
	stuck := filepath.Join(exportsDir, "export_stuck.zip")
	if err := os.WriteFile(stuck, []byte("zip"), 0o644); err != nil {
		t.Fatalf("write stuck zip: %v", err)
	}
	if err := os.Chtimes(stuck, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes stuck zip: %v", err)
	}
	if err := os.Chmod(exportsDir, 0o555); err != nil {
		t.Fatalf("chmod exports: %v", err)
	}
	defer func() {
		if err := os.Chmod(exportsDir, 0o755); err != nil { // let t.TempDir cleanup succeed
			t.Errorf("restore exports perms: %v", err)
		}
	}()

	// imports/ stays writable; its old upload must still be reaped.
	importsDir := filepath.Join(dataDir, "imports")
	if err := os.MkdirAll(importsDir, 0o755); err != nil {
		t.Fatalf("mkdir imports: %v", err)
	}
	importOld := filepath.Join(importsDir, "import_old.zip")
	if err := os.WriteFile(importOld, []byte("zip"), 0o644); err != nil {
		t.Fatalf("write import_old.zip: %v", err)
	}
	if err := os.Chtimes(importOld, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes import_old.zip: %v", err)
	}
	if _, err := d.Exec(
		"INSERT INTO activity_logs (level, action, created_at, updated_at) VALUES (0, 'test', ?, ?)",
		now.AddDate(0, 0, -91).Unix(), now.AddDate(0, 0, -91).Unix(),
	); err != nil {
		t.Fatalf("insert activity_log: %v", err)
	}

	err := s.cleanOldExports(t.Context())
	if err == nil {
		t.Fatal("cleanOldExports with an unremovable file: want error, got nil")
	}
	if !fileExists(stuck) {
		t.Errorf("stuck zip was removed, want kept (exports dir is read-only)")
	}
	if fileExists(importOld) {
		t.Errorf("import_old.zip still exists, want removed despite the exports failure")
	}
	var logs int
	if err := d.QueryRow("SELECT COUNT(*) FROM activity_logs").Scan(&logs); err != nil {
		t.Fatalf("count activity_logs: %v", err)
	}
	if logs != 0 {
		t.Errorf("activity_logs remaining = %d, want 0 (prune must run despite the file failure)", logs)
	}
}

// A directory that cannot even be listed must not abort the sweep either:
// the other directory is still reaped, the 90-day activity-log prune still
// runs, and the failure is reported.
func TestCleanOldExportsContinuesAfterDirReadFailure(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	s := newTestScheduler(d, dataDir, now)
	oldTime := now.Add(-8 * 24 * time.Hour)

	// exports/ is unreadable, so listing it fails.
	exportsDir := filepath.Join(dataDir, "exports")
	if err := os.MkdirAll(exportsDir, 0o755); err != nil {
		t.Fatalf("mkdir exports: %v", err)
	}
	if err := os.Chmod(exportsDir, 0o000); err != nil {
		t.Fatalf("chmod exports: %v", err)
	}
	defer func() {
		if err := os.Chmod(exportsDir, 0o755); err != nil { // let t.TempDir cleanup succeed
			t.Errorf("restore exports perms: %v", err)
		}
	}()

	// imports/ stays readable; its old upload must still be reaped.
	importsDir := filepath.Join(dataDir, "imports")
	if err := os.MkdirAll(importsDir, 0o755); err != nil {
		t.Fatalf("mkdir imports: %v", err)
	}
	importOld := filepath.Join(importsDir, "import_old.zip")
	if err := os.WriteFile(importOld, []byte("zip"), 0o644); err != nil {
		t.Fatalf("write import_old.zip: %v", err)
	}
	if err := os.Chtimes(importOld, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes import_old.zip: %v", err)
	}
	if _, err := d.Exec(
		"INSERT INTO activity_logs (level, action, created_at, updated_at) VALUES (0, 'test', ?, ?)",
		now.AddDate(0, 0, -91).Unix(), now.AddDate(0, 0, -91).Unix(),
	); err != nil {
		t.Fatalf("insert activity_log: %v", err)
	}

	err := s.cleanOldExports(t.Context())
	if err == nil {
		t.Fatal("cleanOldExports with an unreadable dir: want error, got nil")
	}
	if fileExists(importOld) {
		t.Errorf("import_old.zip still exists, want removed despite the unreadable exports dir")
	}
	var logs int
	if err := d.QueryRow("SELECT COUNT(*) FROM activity_logs").Scan(&logs); err != nil {
		t.Fatalf("count activity_logs: %v", err)
	}
	if logs != 0 {
		t.Errorf("activity_logs remaining = %d, want 0 (prune must run despite the dir failure)", logs)
	}
}

// A stale export_* staging dir (crash leftover of transfer.stagingDir,
// holding a full database copy) is reaped by the same 7-day rule; fresh
// staging dirs and dirs without the export_ prefix are kept.
func TestCleanOldExportsRemovesStaleExportStagingDir(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	s := newTestScheduler(d, dataDir, now)
	oldTime := now.Add(-8 * 24 * time.Hour)
	newTime := now.Add(-2 * 24 * time.Hour)

	stale := makeExportStagingDir(t, dataDir, "export_20260701_000000_1_abcd1234", oldTime)
	fresh := makeExportStagingDir(t, dataDir, "export_20260801_000000_1_abcd1234", newTime)
	other := makeExportStagingDir(t, dataDir, "other_dir", oldTime)

	if err := s.cleanOldExports(t.Context()); err != nil {
		t.Fatalf("cleanOldExports: %v", err)
	}
	if fileExists(stale) {
		t.Errorf("stale export staging dir still exists, want removed")
	}
	for _, dir := range []string{fresh, other} {
		if !fileExists(dir) {
			t.Errorf("%s was removed, want kept", filepath.Base(dir))
		}
	}
}

// makeExportStagingDir creates a staging dir like transfer.stagingDir (with
// the database copy inside) and back-dates its mtime.
func makeExportStagingDir(t *testing.T, dataDir, name string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(dataDir, "exports", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rables.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write inside %s: %v", name, err)
	}
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return dir
}

func insertCrosspost(t *testing.T, d *sql.DB, platform string, enabled, autoFetch int64, schedule any) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT INTO crossposts (platform, enabled, auto_fetch_comments, comment_fetch_schedule, created_at, updated_at) VALUES (?, ?, ?, ?, 0, 0)",
		platform, enabled, autoFetch, schedule,
	); err != nil {
		t.Fatalf("insert crosspost %s: %v", platform, err)
	}
}

func TestEnqueueDueCommentFetches(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	s := newTestScheduler(d, t.TempDir(), now)

	insertCrosspost(t, d, "mastodon", 1, 1, "daily")
	insertCrosspost(t, d, "bluesky", 1, 0, "daily") // auto_fetch off
	insertCrosspost(t, d, "twitter", 0, 1, "daily") // disabled
	insertCrosspost(t, d, "xiaohongshu", 1, 1, nil) // no schedule

	if err := s.enqueueDueCommentFetches(t.Context()); err != nil {
		t.Fatalf("enqueueDueCommentFetches: %v", err)
	}
	if n := countRows(t, d, "WHERE kind = ?", KindFetchSocialComments); n != 1 {
		t.Fatalf("enqueued jobs = %d, want 1", n)
	}
	var payload string
	if err := d.QueryRow("SELECT payload FROM job_runs WHERE kind = ?", KindFetchSocialComments).Scan(&payload); err != nil {
		t.Fatalf("load payload: %v", err)
	}
	var p struct {
		Platform string `json:"platform"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Platform != "mastodon" {
		t.Errorf("payload = %q, err = %v; want platform mastodon", payload, err)
	}
	value, found, err := kv.NewStore(d).Get(t.Context(), "last_fetch_comments_mastodon")
	if err != nil || !found {
		t.Fatalf("kv last_fetch_comments_mastodon: found=%v err=%v", found, err)
	}
	if want := strconv.FormatInt(now.Unix(), 10); value != want {
		t.Errorf("kv value = %q, want %q (now unix)", value, want)
	}

	// Second run at the same instant: nothing is due anymore.
	if err := s.enqueueDueCommentFetches(t.Context()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := countRows(t, d, "WHERE kind = ?", KindFetchSocialComments); n != 1 {
		t.Errorf("enqueued jobs after second run = %d, want 1", n)
	}

	// 25 hours later the daily platform is due again.
	later := now.Add(25 * time.Hour)
	s.now = func() time.Time { return later }
	if err := s.enqueueDueCommentFetches(t.Context()); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if n := countRows(t, d, "WHERE kind = ?", KindFetchSocialComments); n != 2 {
		t.Errorf("enqueued jobs after third run = %d, want 2", n)
	}
}

// A failed enqueue must not stamp the last-fetch timestamp: otherwise the
// platform would be silently postponed by a whole schedule period.
func TestEnqueueDueCommentFetchesDoesNotStampOnEnqueueFailure(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	s := newTestScheduler(d, t.TempDir(), now)
	insertCrosspost(t, d, "mastodon", 1, 1, "daily")

	// Force Enqueue to fail while crossposts/kv stay readable.
	if _, err := d.Exec("DROP TABLE job_runs"); err != nil {
		t.Fatalf("drop job_runs: %v", err)
	}
	if err := s.enqueueDueCommentFetches(t.Context()); err == nil {
		t.Fatal("enqueueDueCommentFetches with dropped job_runs: want error, got nil")
	}
	value, found, err := kv.NewStore(d).Get(t.Context(), "last_fetch_comments_mastodon")
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	if found {
		t.Errorf("kv stamped on enqueue failure (value %q), want unstamped", value)
	}
}

func insertTwitterSync(t *testing.T, d *sql.DB, enabled int64, schedule string, lastSyncedAt any) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT INTO twitter_syncs (id, enabled, sync_schedule, last_synced_at, created_at, updated_at) VALUES (1, ?, ?, ?, 0, 0)",
		enabled, schedule, lastSyncedAt,
	); err != nil {
		t.Fatalf("insert twitter_syncs: %v", err)
	}
}

func TestRunSyncTwitterHook(t *testing.T) {
	tests := []struct {
		name         string
		insert       bool
		enabled      int64
		schedule     string
		lastSyncedAt any
		wantCalled   bool
	}{
		{"due", true, 1, "every_15_minutes", int64(1000), true},
		{"never synced", true, 1, "hourly", nil, true},
		{"daily due (synced before today's 08:00 slot)", true, 1, "daily", time.Date(2026, 8, 3, 7, 0, 0, 0, time.UTC).Unix(), true},
		{"daily not due (synced after today's 08:00 slot)", true, 1, "daily", time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC).Unix(), false},
		{"disabled", true, 0, "every_15_minutes", int64(1000), false},
		{"no row", false, 0, "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openDB(t)
			now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
			s := newTestScheduler(d, t.TempDir(), now)
			if tt.insert {
				insertTwitterSync(t, d, tt.enabled, tt.schedule, tt.lastSyncedAt)
			}
			called := false
			s.RegisterHook("sync_twitter", func(context.Context) error {
				called = true
				return nil
			})
			if err := s.runSyncTwitterHook(t.Context()); err != nil {
				t.Fatalf("runSyncTwitterHook: %v", err)
			}
			if called != tt.wantCalled {
				t.Errorf("hook called = %v, want %v", called, tt.wantCalled)
			}
		})
	}
}

func TestRunSyncTwitterHookDefaultNoOp(t *testing.T) {
	d := openDB(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	s := newTestScheduler(d, t.TempDir(), now)
	insertTwitterSync(t, d, 1, "every_15_minutes", int64(1000))
	if err := s.runSyncTwitterHook(t.Context()); err != nil {
		t.Fatalf("runSyncTwitterHook without hook: %v", err)
	}
}

// Scheduled tasks run with the Scheduler's ctx (the process signal ctx in
// main), so SIGTERM interrupts a running task instead of dragging the
// graceful shutdown out until SIGKILL.
func TestSchedulerRunUsesSchedulerContext(t *testing.T) {
	d := openDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	s := NewScheduler(ctx, d, t.TempDir())

	var got context.Context
	s.run(func(ctx context.Context) error {
		got = ctx
		return nil
	})
	if got == nil {
		t.Fatal("task did not run")
	}
	cancel()
	if err := got.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("task ctx.Err() = %v after cancel, want context.Canceled", err)
	}
}

// A panicking task must not escape: cron runs jobs on bare goroutines, so an
// unrecovered panic would crash the whole process (the worker converts
// handler panics to errors the same way).
func TestSchedulerRunRecoversPanic(t *testing.T) {
	d := openDB(t)
	s := NewScheduler(context.Background(), d, t.TempDir())
	s.run(func(context.Context) error { panic("boom") }) // must not panic
}

// Stop waits for a running task, but only up to stopTimeout: a task that
// ignores the cancelled ctx must not block shutdown indefinitely.
func TestSchedulerStopBoundsWait(t *testing.T) {
	d := openDB(t)
	s := NewScheduler(context.Background(), d, t.TempDir())
	s.stopTimeout = 50 * time.Millisecond

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	if _, err := s.cron.AddFunc("@every 1s", func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // stuck like a network fetch that ignores cancellation
	}); err != nil {
		t.Fatalf("AddFunc: %v", err)
	}
	s.Start()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("task did not start within 5s")
	}

	// No defer s.Stop(): this goroutine is the only Stop call, so a Stop
	// that never gives up on the stuck task fails here fast and explicitly
	// instead of deadlocking until the go test timeout. 2s against a 50ms
	// stopTimeout leaves ample scheduling headroom.
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Stop blocked on a stuck task for over 2s, want it to give up after stopTimeout (%s)", s.stopTimeout)
	}
}
