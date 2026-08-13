package twitterarchive

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"rables/internal/db/query"
	"rables/internal/jobs"
)

func insertImport(t *testing.T, database *sql.DB, status string, activeSlot any, updatedAt time.Time) int64 {
	t.Helper()
	now := updatedAt.Unix()
	res, err := database.Exec(
		`INSERT INTO twitter_archive_imports (status, source_filename, queued_at, active_slot, created_at, updated_at)
		 VALUES (?, 'archive.zip', ?, ?, ?, ?)`,
		status, now, activeSlot, now, now,
	)
	if err != nil {
		t.Fatalf("insert import: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("import id: %v", err)
	}
	return id
}

func getImport(t *testing.T, database *sql.DB, id int64) (status string, activeSlot sql.NullInt64, errorMessage sql.NullString) {
	t.Helper()
	err := database.QueryRow(
		`SELECT status, active_slot, error_message FROM twitter_archive_imports WHERE id = ?`, id,
	).Scan(&status, &activeSlot, &errorMessage)
	if err != nil {
		t.Fatalf("load import %d: %v", id, err)
	}
	return status, activeSlot, errorMessage
}

func TestRecoverStaleImports(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-5 * time.Minute)

	t.Run("stale active import is failed and the slot released", func(t *testing.T) {
		database := newTestDB(t)
		stale := insertImport(t, database, "running", 1, now.Add(-time.Hour))
		completed := insertImport(t, database, "completed", nil, now.Add(-time.Hour))

		n, err := RecoverStaleImports(t.Context(), query.New(database), cutoff)
		if err != nil {
			t.Fatalf("RecoverStaleImports: %v", err)
		}
		if n != 1 {
			t.Errorf("recovered = %d, want 1", n)
		}
		status, slot, msg := getImport(t, database, stale)
		if status != "failed" {
			t.Errorf("stale import status = %q, want failed", status)
		}
		if slot.Valid {
			t.Errorf("stale import active_slot = %v, want NULL (slot released)", slot)
		}
		if !msg.Valid || msg.String == "" {
			t.Errorf("stale import error_message = %+v, want set", msg)
		}
		if status, _, _ := getImport(t, database, completed); status != "completed" {
			t.Errorf("completed import status = %q, want untouched", status)
		}

		// The released slot must be reusable by the next submission.
		if _, err := database.Exec(
			`INSERT INTO twitter_archive_imports (status, source_filename, queued_at, active_slot, created_at, updated_at)
			 VALUES ('queued', 'next.zip', 0, 1, 0, 0)`,
		); err != nil {
			t.Errorf("insert next active import after recovery: %v", err)
		}
	})

	t.Run("fresh active import is left alone", func(t *testing.T) {
		database := newTestDB(t)
		fresh := insertImport(t, database, "queued", 1, now.Add(-time.Minute))

		n, err := RecoverStaleImports(t.Context(), query.New(database), cutoff)
		if err != nil {
			t.Fatalf("RecoverStaleImports: %v", err)
		}
		if n != 0 {
			t.Errorf("recovered = %d, want 0", n)
		}
		status, slot, _ := getImport(t, database, fresh)
		if status != "queued" || !slot.Valid {
			t.Errorf("fresh import = (%q, slot %+v), want queued with slot", status, slot)
		}
	})
}

// A job re-executed after its import already completed (a crash between the
// import-complete and job-complete writes leaves the job row running, and the
// startup reaper requeues it) must not resurrect the row: the source file is
// gone, so a re-run could only clobber the history back to failed. The
// mark-running guard skips completed imports and the job completes.
func TestImportJobSkipsCompletedImport(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	id := insertImport(t, database, "completed", nil, time.Now())
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": id}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, t.TempDir())
	claimed, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if !claimed {
		t.Fatalf("job was not claimed")
	}

	status, slot, _ := getImport(t, database, id)
	if status != "completed" || slot.Valid {
		t.Errorf("import = (%q, slot %+v), want completed with released slot", status, slot)
	}
	var jobStatus string
	if err := database.QueryRow(`SELECT status FROM job_runs WHERE kind = ?`, jobs.KindTwitterArchiveImport).Scan(&jobStatus); err != nil {
		t.Fatalf("load job run: %v", err)
	}
	if jobStatus != "done" {
		t.Errorf("job status = %q, want done", jobStatus)
	}
	// No spurious started/failed activity for the skipped re-run.
	var logs int
	if err := database.QueryRow(`SELECT COUNT(*) FROM activity_logs WHERE target = 'twitter_archive'`).Scan(&logs); err != nil {
		t.Fatalf("count activity logs: %v", err)
	}
	if logs != 0 {
		t.Errorf("activity logs = %d, want 0", logs)
	}
}
