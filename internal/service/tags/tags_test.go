package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/db/query"
)

func newTestDB(t *testing.T) (*sql.DB, *query.Queries) {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database, query.New(database)
}

// TestFindOrCreateByNamesMixedCase: "Go"/"go"/"GO" resolve to one tag, ids
// keep first-occurrence order, and blanks/dupes are dropped — mirroring
// Tag.find_or_create_by_names.
func TestFindOrCreateByNamesMixedCase(t *testing.T) {
	_, q := newTestDB(t)
	ctx := t.Context()

	ids, err := FindOrCreateByNames(ctx, q, []string{"Go", "go", "GO"})
	if err != nil {
		t.Fatalf("find or create: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want exactly one tag id", ids)
	}

	ids, err = FindOrCreateByNames(ctx, q, []string{"ruby", " GO ", "", "Ruby", "rails"})
	if err != nil {
		t.Fatalf("find or create: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("ids = %v, want 3 ids (ruby, GO, rails)", ids)
	}
	if ids[1] == ids[0] || ids[1] == ids[2] {
		t.Errorf("mixed-case name did not resolve to the existing tag: %v", ids)
	}

	tag, err := q.GetTagByLowerName(ctx, "go")
	if err != nil {
		t.Fatalf("get tag: %v", err)
	}
	if tag.Name != "Go" {
		t.Errorf("stored name = %q, want the first-seen %q", tag.Name, "Go")
	}
	if ids[1] != tag.ID {
		t.Errorf("GO id = %d, want existing tag id %d", ids[1], tag.ID)
	}
	if got := countTags(t, q); got != 3 {
		t.Errorf("tag rows = %d, want 3", got)
	}
}

// TestFindOrCreateByNamesConcurrent: concurrent creators of the same
// (mixed-case) name all succeed and exactly one row survives.
func TestFindOrCreateByNamesConcurrent(t *testing.T) {
	_, q := newTestDB(t)
	ctx := t.Context()

	const workers = 16
	names := []string{"Go", "go", "GO", "gO"}
	ids := make([][]int64, workers)
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := FindOrCreateByNames(ctx, q, []string{names[i%len(names)]})
			if err != nil {
				errs <- err
				return
			}
			ids[i] = got
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent find or create: %v", err)
	}
	for i, got := range ids {
		if len(got) != 1 || got[0] != ids[0][0] {
			t.Errorf("worker %d ids = %v, want [%d]", i, got, ids[0][0])
		}
	}
	if got := countTags(t, q); got != 1 {
		t.Errorf("tag rows = %d, want 1", got)
	}
}

// TestCreateSlug covers Tag#generate_slug: the squished name is the slug
// (Chinese kept as-is), collisions take "-1", "-2", ... suffixes.
func TestCreateSlug(t *testing.T) {
	_, q := newTestDB(t)
	ctx := t.Context()

	tests := []struct {
		name     string
		wantSlug string
	}{
		{name: "Go", wantSlug: "Go"},
		{name: "Hello World", wantSlug: "Hello World"},
		{name: "Hello  World", wantSlug: "Hello World-1"}, // squish collides with the above
		{name: "编程 语言", wantSlug: "编程 语言"},                // non-ASCII kept, not parameterized
		{name: "  padded  ", wantSlug: "padded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tag, err := Create(ctx, q, tt.name)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if tag.Slug != tt.wantSlug {
				t.Errorf("slug = %q, want %q", tag.Slug, tt.wantSlug)
			}
		})
	}
}

// TestCreateValidation mirrors the model validations.
func TestCreateValidation(t *testing.T) {
	_, q := newTestDB(t)
	ctx := t.Context()

	if _, err := Create(ctx, q, "go"); err != nil {
		t.Fatalf("create: %v", err)
	}
	tests := []struct {
		name string
		in   string
		want error
	}{
		{name: "blank", in: "   ", want: ErrNameBlank},
		{name: "empty", in: "", want: ErrNameBlank},
		{name: "duplicate same case", in: "go", want: ErrNameTaken},
		{name: "duplicate other case", in: "GO", want: ErrNameTaken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Create(ctx, q, tt.in); !errors.Is(err, tt.want) {
				t.Errorf("create(%q) error = %v, want %v", tt.in, err, tt.want)
			}
		})
	}
}

// TestRenameBumpsArticles: a rename bumps every tagged article's updated_at
// in the same transaction (Tag#touch_articles); a no-change save does not.
// The slug stays put because generate_slug only fills blank slugs.
func TestRenameBumpsArticles(t *testing.T) {
	database, q := newTestDB(t)
	ctx := t.Context()
	const old = int64(1000)

	tag, err := Create(ctx, q, "Go")
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	other, err := Create(ctx, q, "Other")
	if err != nil {
		t.Fatalf("create other tag: %v", err)
	}
	tagged := insertArticle(t, database, "tagged", old)
	untouched := insertArticle(t, database, "untouched", old)
	linkTag(t, database, tagged, tag.ID)
	linkTag(t, database, untouched, other.ID)

	if err := Rename(ctx, database, tag.ID, "Golang"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := articleUpdatedAt(t, database, tagged); got <= old {
		t.Errorf("tagged article updated_at = %d, want bumped past %d", got, old)
	}
	if got := articleUpdatedAt(t, database, untouched); got != old {
		t.Errorf("unrelated article updated_at = %d, want unchanged %d", got, old)
	}
	renamed, err := q.GetTagByID(ctx, tag.ID)
	if err != nil {
		t.Fatalf("get tag: %v", err)
	}
	if renamed.Name != "Golang" || renamed.Slug != "Go" {
		t.Errorf("renamed tag = (%q, %q), want (Golang, Go) — slug kept", renamed.Name, renamed.Slug)
	}

	// Saving without changing the name leaves the articles alone.
	resetArticleUpdatedAt(t, database, tagged, old)
	if err := Rename(ctx, database, tag.ID, "Golang"); err != nil {
		t.Fatalf("no-change rename: %v", err)
	}
	if got := articleUpdatedAt(t, database, tagged); got != old {
		t.Errorf("no-change rename bumped article updated_at to %d, want %d", got, old)
	}
}

// TestRenameValidation mirrors the update path's validations and lookup.
func TestRenameValidation(t *testing.T) {
	database, q := newTestDB(t)
	ctx := t.Context()

	tag, err := Create(ctx, q, "Go")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Create(ctx, q, "Ruby"); err != nil {
		t.Fatalf("create: %v", err)
	}
	tests := []struct {
		name string
		id   int64
		in   string
		want error
	}{
		{name: "blank", id: tag.ID, in: " ", want: ErrNameBlank},
		{name: "taken", id: tag.ID, in: "ruby", want: ErrNameTaken},
		{name: "missing", id: 999, in: "x", want: sql.ErrNoRows},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Rename(ctx, database, tt.id, tt.in); !errors.Is(err, tt.want) {
				t.Errorf("rename error = %v, want %v", err, tt.want)
			}
		})
	}
	// Renaming to the same name with different case is allowed (uniqueness
	// excludes the record itself).
	if err := Rename(ctx, database, tag.ID, "gO"); err != nil {
		t.Errorf("same-tag case change: %v", err)
	}
}

// TestDestroy covers tag.destroy with dependent: :destroy on the join
// tables: join rows go, the tag goes, the article stays.
func TestDestroy(t *testing.T) {
	database, q := newTestDB(t)
	ctx := t.Context()

	tag, err := Create(ctx, q, "Go")
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	article := insertArticle(t, database, "a", 1000)
	linkTag(t, database, article, tag.ID)
	subscriber := insertSubscriber(t, database, "a@example.com")
	linkSubscriberTag(t, database, subscriber, tag.ID)

	if err := Destroy(ctx, database, tag.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := q.GetTagByID(ctx, tag.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("tag still present: %v", err)
	}
	for table, want := range map[string]int64{"article_tags": 0, "subscriber_tags": 0} {
		var got int64
		if err := database.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE tag_id = ?", table), tag.ID).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Errorf("%s rows = %d, want %d", table, got, want)
		}
	}
	if _, err := q.GetArticleByID(ctx, article); err != nil {
		t.Errorf("article was destroyed with the tag: %v", err)
	}
	if err := Destroy(ctx, database, tag.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("second destroy error = %v, want sql.ErrNoRows", err)
	}
}

func countTags(t *testing.T, q *query.Queries) int64 {
	t.Helper()
	rows, err := q.ListTagsWithArticleCount(t.Context())
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	return int64(len(rows))
}

func insertArticle(t *testing.T, database *sql.DB, slug string, updatedAt int64) int64 {
	t.Helper()
	res, err := database.ExecContext(t.Context(),
		`INSERT INTO articles (title, slug, created_at, updated_at) VALUES (?, ?, ?, ?)`, slug, slug, updatedAt, updatedAt)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("article id: %v", err)
	}
	return id
}

func linkTag(t *testing.T, database *sql.DB, articleID, tagID int64) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO article_tags (article_id, tag_id, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		articleID, tagID, now, now); err != nil {
		t.Fatalf("link tag: %v", err)
	}
}

func insertSubscriber(t *testing.T, database *sql.DB, email string) int64 {
	t.Helper()
	now := time.Now().Unix()
	res, err := database.ExecContext(t.Context(),
		`INSERT INTO subscribers (email, created_at, updated_at) VALUES (?, ?, ?)`, email, now, now)
	if err != nil {
		t.Fatalf("insert subscriber: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("subscriber id: %v", err)
	}
	return id
}

func linkSubscriberTag(t *testing.T, database *sql.DB, subscriberID, tagID int64) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := database.ExecContext(t.Context(),
		`INSERT INTO subscriber_tags (subscriber_id, tag_id, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		subscriberID, tagID, now, now); err != nil {
		t.Fatalf("link subscriber tag: %v", err)
	}
}

func articleUpdatedAt(t *testing.T, database *sql.DB, id int64) int64 {
	t.Helper()
	var got int64
	if err := database.QueryRowContext(t.Context(), `SELECT updated_at FROM articles WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("article updated_at: %v", err)
	}
	return got
}

func resetArticleUpdatedAt(t *testing.T, database *sql.DB, id, updatedAt int64) {
	t.Helper()
	if _, err := database.ExecContext(t.Context(), `UPDATE articles SET updated_at = ? WHERE id = ?`, updatedAt, id); err != nil {
		t.Fatalf("reset article updated_at: %v", err)
	}
}
