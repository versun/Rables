package jobs

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"rables/internal/domain"
)

func insertScheduledArticle(t *testing.T, d *sql.DB, status domain.Status, scheduledAt *int64, platforms string, newsletter int64) int64 {
	t.Helper()
	res, err := d.Exec(
		`INSERT INTO articles (slug, status, scheduled_at, scheduled_crosspost_platforms, scheduled_send_newsletter, created_at, updated_at)
		 VALUES (lower(hex(randomblob(8))), ?, ?, ?, ?, 1000, 1000)`,
		int64(status), scheduledAt, platforms, newsletter,
	)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("article id: %v", err)
	}
	return id
}

func insertScheduledPage(t *testing.T, d *sql.DB, status domain.Status, scheduledAt *int64) int64 {
	t.Helper()
	res, err := d.Exec(
		`INSERT INTO pages (slug, status, scheduled_at, created_at, updated_at)
		 VALUES (lower(hex(randomblob(8))), ?, ?, 1000, 1000)`,
		int64(status), scheduledAt,
	)
	if err != nil {
		t.Fatalf("insert page: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("page id: %v", err)
	}
	return id
}

type articleRow struct {
	status      domain.Status
	scheduledAt sql.NullInt64
	platforms   string
	newsletter  int64
	createdAt   int64
}

func getArticle(t *testing.T, d *sql.DB, id int64) articleRow {
	t.Helper()
	var a articleRow
	err := d.QueryRow(
		`SELECT status, scheduled_at, scheduled_crosspost_platforms, scheduled_send_newsletter, created_at FROM articles WHERE id = ?`, id,
	).Scan(&a.status, &a.scheduledAt, &a.platforms, &a.newsletter, &a.createdAt)
	if err != nil {
		t.Fatalf("load article %d: %v", id, err)
	}
	return a
}

func getPageRow(t *testing.T, d *sql.DB, id int64) (status domain.Status, scheduledAt sql.NullInt64, createdAt int64) {
	t.Helper()
	err := d.QueryRow(`SELECT status, scheduled_at, created_at FROM pages WHERE id = ?`, id).
		Scan(&status, &scheduledAt, &createdAt)
	if err != nil {
		t.Fatalf("load page %d: %v", id, err)
	}
	return status, scheduledAt, createdAt
}

func countJobs(t *testing.T, d *sql.DB, kind string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM job_runs WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s jobs: %v", kind, err)
	}
	return n
}

// enqueuedPayloads returns the decoded payloads of all jobs of kind, oldest
// first (excluding the publish job under test itself).
func enqueuedPayloads(t *testing.T, d *sql.DB, kind string) []json.RawMessage {
	t.Helper()
	rows, err := d.Query(`SELECT payload FROM job_runs WHERE kind = ? ORDER BY id`, kind)
	if err != nil {
		t.Fatalf("list %s jobs: %v", kind, err)
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var p sql.NullString
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		if p.Valid {
			out = append(out, json.RawMessage(p.String))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s jobs: %v", kind, err)
	}
	return out
}

// runPublishJob enqueues a due job of kind with payload and runs the worker once.
func runPublishJob(t *testing.T, d *sql.DB, w *Worker, now time.Time, kind string, payload any) {
	t.Helper()
	if _, err := NewEnqueuer(d).Enqueue(t.Context(), kind, payload, now.Add(-time.Minute)); err != nil {
		t.Fatalf("enqueue %s: %v", kind, err)
	}
	ran, err := w.RunOnce(t.Context())
	if err != nil || !ran {
		t.Fatalf("RunOnce = (%v, %v), want (true, nil)", ran, err)
	}
}

func TestPublishArticleDueFlipsAndConsumesSnapshot(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	w := newTestWorker(d, now)
	RegisterPublishHandlers(w, d, NewEnqueuer(d))

	scheduledAt := now.Add(-time.Hour).Unix()
	id := insertScheduledArticle(t, d, domain.StatusSchedule, &scheduledAt, `["mastodon","bluesky"]`, 1)

	runPublishJob(t, d, w, now, KindPublishArticle, publishArticlePayload{ArticleID: id})

	a := getArticle(t, d, id)
	if a.status != domain.StatusPublish {
		t.Errorf("status = %v, want publish", a.status)
	}
	if a.createdAt != scheduledAt {
		t.Errorf("created_at = %d, want backfilled scheduled_at %d", a.createdAt, scheduledAt)
	}
	if a.scheduledAt.Valid {
		t.Errorf("scheduled_at = %v, want NULL", a.scheduledAt)
	}
	if a.platforms != "[]" {
		t.Errorf("scheduled_crosspost_platforms = %q, want []", a.platforms)
	}
	if a.newsletter != 0 {
		t.Errorf("scheduled_send_newsletter = %d, want 0", a.newsletter)
	}

	crossposts := enqueuedPayloads(t, d, KindCrosspost)
	if len(crossposts) != 1 {
		t.Fatalf("crosspost jobs = %d, want 1", len(crossposts))
	}
	var cp publishCrosspostPayload
	if err := json.Unmarshal(crossposts[0], &cp); err != nil {
		t.Fatalf("decode crosspost payload: %v", err)
	}
	if cp.ArticleID != id || len(cp.Platforms) != 2 || cp.Platforms[0] != "mastodon" || cp.Platforms[1] != "bluesky" {
		t.Errorf("crosspost payload = %s, want article_id=%d platforms=[mastodon bluesky]", crossposts[0], id)
	}

	newsletters := enqueuedPayloads(t, d, KindSendNewsletter)
	if len(newsletters) != 1 {
		t.Fatalf("newsletter jobs = %d, want 1", len(newsletters))
	}
	var np publishArticlePayload
	if err := json.Unmarshal(newsletters[0], &np); err != nil {
		t.Fatalf("decode newsletter payload: %v", err)
	}
	if np.ArticleID != id {
		t.Errorf("newsletter payload = %s, want article_id=%d", newsletters[0], id)
	}
}

func TestPublishArticleWithoutSnapshotEnqueuesNothing(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	w := newTestWorker(d, now)
	RegisterPublishHandlers(w, d, NewEnqueuer(d))

	scheduledAt := now.Add(-time.Hour).Unix()
	id := insertScheduledArticle(t, d, domain.StatusSchedule, &scheduledAt, "[]", 0)

	runPublishJob(t, d, w, now, KindPublishArticle, publishArticlePayload{ArticleID: id})

	if a := getArticle(t, d, id); a.status != domain.StatusPublish {
		t.Errorf("status = %v, want publish", a.status)
	}
	if n := countJobs(t, d, KindCrosspost); n != 0 {
		t.Errorf("crosspost jobs = %d, want 0", n)
	}
	if n := countJobs(t, d, KindSendNewsletter); n != 0 {
		t.Errorf("newsletter jobs = %d, want 0", n)
	}
}

func TestPublishArticleSkipsNonScheduleStates(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	w := newTestWorker(d, now)
	RegisterPublishHandlers(w, d, NewEnqueuer(d))

	scheduledAt := now.Add(-time.Hour).Unix()
	tests := []struct {
		name        string
		status      domain.Status
		scheduledAt *int64
	}{
		{"draft", domain.StatusDraft, &scheduledAt},
		{"already published", domain.StatusPublish, &scheduledAt},
		{"trash", domain.StatusTrash, &scheduledAt},
		{"schedule without scheduled_at", domain.StatusSchedule, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := insertScheduledArticle(t, d, tt.status, tt.scheduledAt, `["mastodon"]`, 1)
			runPublishJob(t, d, w, now, KindPublishArticle, publishArticlePayload{ArticleID: id})

			a := getArticle(t, d, id)
			if a.status != tt.status {
				t.Errorf("status = %v, want unchanged %v", a.status, tt.status)
			}
			if a.createdAt != 1000 {
				t.Errorf("created_at = %d, want unchanged 1000", a.createdAt)
			}
			if n := countJobs(t, d, KindCrosspost); n != 0 {
				t.Errorf("crosspost jobs = %d, want 0", n)
			}
			if n := countJobs(t, d, KindSendNewsletter); n != 0 {
				t.Errorf("newsletter jobs = %d, want 0", n)
			}
		})
	}
}

func TestPublishArticleNotYetDueSkips(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	w := newTestWorker(d, now)
	RegisterPublishHandlers(w, d, NewEnqueuer(d))

	future := now.Add(time.Hour).Unix()
	id := insertScheduledArticle(t, d, domain.StatusSchedule, &future, `["mastodon"]`, 1)

	runPublishJob(t, d, w, now, KindPublishArticle, publishArticlePayload{ArticleID: id})

	a := getArticle(t, d, id)
	if a.status != domain.StatusSchedule {
		t.Errorf("status = %v, want still schedule", a.status)
	}
	if !a.scheduledAt.Valid || a.scheduledAt.Int64 != future {
		t.Errorf("scheduled_at = %v, want %d", a.scheduledAt, future)
	}
	if n := countJobs(t, d, KindCrosspost); n != 0 {
		t.Errorf("crosspost jobs = %d, want 0", n)
	}
}

func TestPublishArticleRerunDoesNotReenqueue(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	w := newTestWorker(d, now)
	RegisterPublishHandlers(w, d, NewEnqueuer(d))

	scheduledAt := now.Add(-time.Hour).Unix()
	id := insertScheduledArticle(t, d, domain.StatusSchedule, &scheduledAt, `["mastodon"]`, 1)

	runPublishJob(t, d, w, now, KindPublishArticle, publishArticlePayload{ArticleID: id})
	// A duplicate job (e.g. an old schedule that was never cancelled) must be a no-op.
	runPublishJob(t, d, w, now, KindPublishArticle, publishArticlePayload{ArticleID: id})

	if n := countJobs(t, d, KindCrosspost); n != 1 {
		t.Errorf("crosspost jobs = %d, want 1", n)
	}
	if n := countJobs(t, d, KindSendNewsletter); n != 1 {
		t.Errorf("newsletter jobs = %d, want 1", n)
	}
}

func TestPublishArticleMissingRowIsDone(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	w := newTestWorker(d, now)
	RegisterPublishHandlers(w, d, NewEnqueuer(d))

	// No article with id 99999; the handler must succeed so the job completes.
	runPublishJob(t, d, w, now, KindPublishArticle, publishArticlePayload{ArticleID: 99999})
}

func TestPublishPageDueFlipsWithoutCreatedAtBackfill(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	w := newTestWorker(d, now)
	RegisterPublishHandlers(w, d, NewEnqueuer(d))

	scheduledAt := now.Add(-time.Hour).Unix()
	id := insertScheduledPage(t, d, domain.StatusSchedule, &scheduledAt)

	runPublishJob(t, d, w, now, KindPublishPage, publishPagePayload{PageID: id})

	status, sched, createdAt := getPageRow(t, d, id)
	if status != domain.StatusPublish {
		t.Errorf("status = %v, want publish", status)
	}
	if sched.Valid {
		t.Errorf("scheduled_at = %v, want NULL", sched)
	}
	if createdAt != 1000 {
		t.Errorf("created_at = %d, want unchanged 1000 (Rails does not backfill pages)", createdAt)
	}
}

func TestPublishPageSkips(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	w := newTestWorker(d, now)
	RegisterPublishHandlers(w, d, NewEnqueuer(d))

	past := now.Add(-time.Hour).Unix()
	future := now.Add(time.Hour).Unix()
	tests := []struct {
		name        string
		status      domain.Status
		scheduledAt *int64
	}{
		{"already published", domain.StatusPublish, &past},
		{"schedule without scheduled_at", domain.StatusSchedule, nil},
		{"not yet due", domain.StatusSchedule, &future},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := insertScheduledPage(t, d, tt.status, tt.scheduledAt)
			runPublishJob(t, d, w, now, KindPublishPage, publishPagePayload{PageID: id})

			status, sched, _ := getPageRow(t, d, id)
			if status != tt.status {
				t.Errorf("status = %v, want unchanged %v", status, tt.status)
			}
			if (sched.Valid) != (tt.scheduledAt != nil) {
				t.Errorf("scheduled_at valid = %v, want %v", sched.Valid, tt.scheduledAt != nil)
			}
		})
	}
}
