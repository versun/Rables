package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"rables/internal/kv"
)

func newTestScheduler(d *sql.DB, dataDir string, now time.Time) *Scheduler {
	s := NewScheduler(d, dataDir)
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
		{"imports", "import_new.zip", newTime, false},
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
		{"not due", true, 1, "daily", time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC).Unix(), false},
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
