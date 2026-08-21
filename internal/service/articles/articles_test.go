package articles

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
)

func TestParseStatus(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"draft", 0, true},
		{"publish", 1, true},
		{"schedule", 2, true},
		{"trash", 3, true},
		{"shared", 4, true},
		{"", 0, false},
		{"bogus", 0, false},
	}
	for _, tt := range tests {
		got, ok := ParseStatus(tt.in)
		if ok != tt.ok || int(got) != tt.want {
			t.Errorf("ParseStatus(%q) = %d, %v; want %d, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseScheduledPlatforms(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"json array", `["twitter","mastodon"]`, []string{"mastodon", "twitter"}},
		{"empty json", `[]`, nil},
		{"unknown dropped", `["mastodon","myspace"]`, []string{"mastodon"}},
		{"duplicates collapse", `["bluesky","bluesky"]`, []string{"bluesky"}},
		{"comma fallback", "twitter, bluesky", []string{"twitter", "bluesky"}},
		{"garbage", "not-json", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseScheduledPlatforms(tt.raw); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseScheduledPlatforms(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSplitTagList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"go", []string{"go"}},
		{"go, web ,x", []string{"go", " web ", "x"}}, // trimming happens downstream
	}
	for _, tt := range tests {
		if got := SplitTagList(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitTagList(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSelectedPlatforms(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]bool
		want []string
	}{
		{"nil map", nil, []string{}},
		{"order normalized", map[string]bool{"bluesky": true, "mastodon": true}, []string{"mastodon", "bluesky"}},
		{"false dropped", map[string]bool{"mastodon": true, "twitter": false}, []string{"mastodon"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SelectedPlatforms(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SelectedPlatforms(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestSaveContentTypeSanitize covers the write-time content_html handling per
// content type: raw html is stored verbatim (skip sanitize), rich_text is
// sanitized and lazy-loaded, and markdown is rendered then sanitized.
func TestSaveContentTypeSanitize(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := t.Context()

	save := func(t *testing.T, p SaveParams) string {
		t.Helper()
		article, errs, err := Save(ctx, database, nil, p)
		if err != nil || len(errs) > 0 {
			t.Fatalf("Save: errs = %v, err = %v", errs, err)
		}
		return article.ContentHtml.String
	}

	t.Run("html stored verbatim", func(t *testing.T) {
		raw := `<div><script>alert(1)</script><img src="/x.png"></div>`
		stored := save(t, SaveParams{
			Title:       "HTML Post",
			ContentType: string(domain.ContentTypeHTML),
			ContentHTML: raw,
			Status:      domain.StatusDraft,
		})
		if stored != raw {
			t.Errorf("content_html = %q, want the raw html untouched", stored)
		}
	})

	t.Run("legacy rich_text sanitized, stored as html", func(t *testing.T) {
		article, errs, err := Save(ctx, database, nil, SaveParams{
			Title:       "Rich Post",
			ContentType: string(domain.ContentTypeRichText),
			ContentHTML: `<p>Hi</p><script>alert(1)</script><p><img src="/x.png"></p>`,
			Status:      domain.StatusDraft,
		})
		if err != nil || len(errs) > 0 {
			t.Fatalf("Save: errs = %v, err = %v", errs, err)
		}
		stored := article.ContentHtml.String
		if article.ContentType != string(domain.ContentTypeHTML) {
			t.Errorf("content_type = %q, want html (rich_text coerced)", article.ContentType)
		}
		if strings.Contains(stored, "script") {
			t.Errorf("content_html still contains script: %q", stored)
		}
		if !strings.Contains(stored, `loading="lazy"`) {
			t.Errorf("img missing loading=lazy: %q", stored)
		}
	})

	t.Run("markdown rendered and sanitized", func(t *testing.T) {
		stored := save(t, SaveParams{
			Title:       "MD Post",
			ContentType: string(domain.ContentTypeMarkdown),
			ContentHTML: "# Hi\n\n<script>alert(1)</script>\n",
			Status:      domain.StatusDraft,
		})
		if !strings.Contains(stored, "<h1>Hi</h1>") {
			t.Errorf("content_html is not rendered markdown: %q", stored)
		}
		if strings.Contains(stored, "script") {
			t.Errorf("content_html kept the script: %q", stored)
		}
	})
}

// TestSaveRejectsBlankSlug: a handwritten slug of only dots strips to "" in
// GenerateSlug; without the validation it would be stored as a NULL slug that
// no public route or admin action can reach.
func TestSaveRejectsBlankSlug(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	_, errs, err := Save(t.Context(), database, nil, SaveParams{
		Title:       "Post",
		Slug:        ".",
		ContentType: string(domain.ContentTypeHTML),
		ContentHTML: "<p>x</p>",
		Status:      domain.StatusDraft,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(errs) != 1 || errs[0] != "Slug can't be blank" {
		t.Errorf("Save errs = %v, want [\"Slug can't be blank\"]", errs)
	}
}

// TestSaveCleansURLUnsafeSlug: a handwritten slug with URL path reserved
// chars is stripped down to a reachable slug instead of being stored as-is,
// where no route could match it.
func TestSaveCleansURLUnsafeSlug(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	article, errs, err := Save(t.Context(), database, nil, SaveParams{
		Title:       "Post",
		Slug:        "a/b?c#d",
		ContentType: string(domain.ContentTypeHTML),
		ContentHTML: "<p>x</p>",
		Status:      domain.StatusDraft,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("Save errs = %v, want none", errs)
	}
	if got := article.Slug.String; got != "abcd" {
		t.Errorf("slug = %q, want %q", got, "abcd")
	}
}

// TestSaveRejectsAdminBatchSlug: an article slugged like an admin batch
// action still opens its edit form (GET /admin/posts/batch_destroy/edit falls
// through to {id}/edit), but the update — POST /admin/posts/batch_destroy —
// is a static chi route swallowed by the batch handler, so Save rejects the
// slug instead of silently losing edits.
func TestSaveRejectsAdminBatchSlug(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	for _, slug := range domain.AdminArticleBatchSlugs {
		_, errs, err := Save(t.Context(), database, nil, SaveParams{
			Title:       "Post",
			Slug:        slug,
			ContentType: string(domain.ContentTypeHTML),
			ContentHTML: "<p>x</p>",
			Status:      domain.StatusDraft,
		})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if len(errs) != 1 || errs[0] != "Slug is reserved" {
			t.Errorf("Save slug %q errs = %v, want [\"Slug is reserved\"]", slug, errs)
		}
	}
}

// TestDestroyPurgesAttachments covers the has_rich_text dependent purge:
// destroying an article removes its attachment rows, the files rows left
// without references (image variants included) and their disk blobs — while a
// file still attached to another article keeps its row and blob.
func TestDestroyPurgesAttachments(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := t.Context()
	q := query.New(database)

	save := func(t *testing.T, title string) int64 {
		t.Helper()
		article, errs, err := Save(ctx, database, nil, SaveParams{
			Title:       title,
			ContentType: string(domain.ContentTypeHTML),
			ContentHTML: "<p>x</p>",
			Status:      domain.StatusDraft,
		})
		if err != nil || len(errs) > 0 {
			t.Fatalf("Save: errs = %v, err = %v", errs, err)
		}
		return article.ID
	}
	writeBlob := func(t *testing.T, key string) string {
		t.Helper()
		path := filepath.Join(dataDir, "files", key[0:2], key[2:4], key)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir blob: %v", err)
		}
		if err := os.WriteFile(path, []byte("blob"), 0o644); err != nil {
			t.Fatalf("write blob: %v", err)
		}
		return path
	}
	addFile := func(t *testing.T, key string, variantOf int64) int64 {
		t.Helper()
		vo := sql.NullInt64{}
		if variantOf > 0 {
			vo = sql.NullInt64{Int64: variantOf, Valid: true}
		}
		f, err := q.CreateFile(ctx, query.CreateFileParams{
			Key:       key,
			Filename:  key + ".jpg",
			ByteSize:  4,
			VariantOf: vo,
			CreatedAt: 1,
		})
		if err != nil {
			t.Fatalf("create file: %v", err)
		}
		return f.ID
	}
	attach := func(t *testing.T, fileID, articleID int64) {
		t.Helper()
		if err := q.CreateAttachment(ctx, query.CreateAttachmentParams{
			FileID: fileID, RecordType: "Article", RecordID: articleID, Name: "embeds", CreatedAt: 1,
		}); err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	countRows := func(t *testing.T, sqlText string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := database.QueryRowContext(ctx, sqlText, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	articleA := save(t, "Post A")
	articleB := save(t, "Post B")

	// A file shared by both articles.
	sharedKey := "aaaa1111bbbb2222"
	sharedID := addFile(t, sharedKey, 0)
	sharedBlob := writeBlob(t, sharedKey)
	attach(t, sharedID, articleA)
	attach(t, sharedID, articleB)

	// A file attached only to A, with an image variant.
	ownKey := "cccc3333dddd4444"
	variantKey := "eeee5555ffff6666"
	ownID := addFile(t, ownKey, 0)
	ownBlob := writeBlob(t, ownKey)
	variantID := addFile(t, variantKey, ownID)
	variantBlob := writeBlob(t, variantKey)
	attach(t, ownID, articleA)

	if err := Destroy(ctx, database, articleA, dataDir); err != nil {
		t.Fatalf("destroy A: %v", err)
	}

	if _, err := q.GetAdminArticleByID(ctx, articleA); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("article A still present: %v", err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM attachments WHERE record_type = 'Article' AND record_id = ?`, articleA); n != 0 {
		t.Errorf("article A attachments = %d, want 0", n)
	}
	for _, id := range []int64{ownID, variantID} {
		if n := countRows(t, `SELECT COUNT(*) FROM files WHERE id = ?`, id); n != 0 {
			t.Errorf("files row %d = %d, want 0 (purged with the article)", id, n)
		}
	}
	for _, blob := range []string{ownBlob, variantBlob} {
		if _, err := os.Stat(blob); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("blob %s still on disk: %v", blob, err)
		}
	}
	// The shared file keeps its row, its blob and its attachment to B.
	if n := countRows(t, `SELECT COUNT(*) FROM files WHERE id = ?`, sharedID); n != 1 {
		t.Errorf("shared files row = %d, want 1 (still attached to B)", n)
	}
	if _, err := os.Stat(sharedBlob); err != nil {
		t.Errorf("shared blob removed: %v", err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM attachments WHERE record_type = 'Article' AND record_id = ?`, articleB); n != 1 {
		t.Errorf("article B attachments = %d, want 1", n)
	}

	// Destroying B drops the last reference: the shared file goes too.
	if err := Destroy(ctx, database, articleB, dataDir); err != nil {
		t.Fatalf("destroy B: %v", err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM files WHERE id = ?`, sharedID); n != 0 {
		t.Errorf("shared files row after B destroy = %d, want 0", n)
	}
	if _, err := os.Stat(sharedBlob); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("shared blob after B destroy still on disk: %v", err)
	}
}

// TestDestroyKeepsStaticFileReferencedFile: a database import can merge a
// static_files entry onto a files row that is also attached to an article.
// Destroying that article must keep the file row and its blob —
// static_files.file_id references files(id) under foreign_keys enforcement,
// so deleting it would fail the whole destroy on the FK violation.
func TestDestroyKeepsStaticFileReferencedFile(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := t.Context()
	q := query.New(database)

	article, errs, err := Save(ctx, database, nil, SaveParams{
		Title:       "Post",
		ContentType: string(domain.ContentTypeHTML),
		ContentHTML: "<p>x</p>",
		Status:      domain.StatusDraft,
	})
	if err != nil || len(errs) > 0 {
		t.Fatalf("Save: errs = %v, err = %v", errs, err)
	}

	key := "aaaa1111bbbb2222"
	file, err := q.CreateFile(ctx, query.CreateFileParams{
		Key: key, Filename: "doc.pdf", ByteSize: 4, CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	blob := filepath.Join(dataDir, "files", key[0:2], key[2:4], key)
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatalf("mkdir blob: %v", err)
	}
	if err := os.WriteFile(blob, []byte("blob"), 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := q.CreateAttachment(ctx, query.CreateAttachmentParams{
		FileID: file.ID, RecordType: "Article", RecordID: article.ID, Name: "embeds", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := q.CreateStaticFile(ctx, query.CreateStaticFileParams{
		Filename: "doc.pdf", FileID: file.ID, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("create static file: %v", err)
	}

	if err := Destroy(ctx, database, article.ID, dataDir); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if _, err := q.GetAdminArticleByID(ctx, article.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("article still present: %v", err)
	}
	var files int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files WHERE id = ?`, file.ID).Scan(&files); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if files != 1 {
		t.Errorf("files row = %d, want 1 (still referenced by the static file)", files)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Errorf("blob removed while the static file still references it: %v", err)
	}
	var staticFiles int
	if err := database.QueryRow(`SELECT COUNT(*) FROM static_files WHERE file_id = ?`, file.ID).Scan(&staticFiles); err != nil {
		t.Fatalf("count static files: %v", err)
	}
	if staticFiles != 1 {
		t.Errorf("static_files row = %d, want 1 (untouched by the destroy)", staticFiles)
	}
}

// TestDestroyKeepsContentReferencedFile: a file attached to the destroyed
// article whose /files/<key> URL was hand-reused in another article's body
// must survive the destroy — the attachment sweep would otherwise delete the
// row and blob and leave the other article's embed 404.
func TestDestroyKeepsContentReferencedFile(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := t.Context()
	q := query.New(database)

	key := "aaaa1111bbbb2222"
	articleA, errs, err := Save(ctx, database, nil, SaveParams{
		Title:       "Post A",
		ContentType: string(domain.ContentTypeHTML),
		ContentHTML: `<p><img src="/files/` + key + `"></p>`,
		Status:      domain.StatusDraft,
	})
	if err != nil || len(errs) > 0 {
		t.Fatalf("Save A: errs = %v, err = %v", errs, err)
	}
	if _, errs, err := Save(ctx, database, nil, SaveParams{
		Title:       "Post B",
		ContentType: string(domain.ContentTypeHTML),
		ContentHTML: `<p><img src="/files/` + key + `"></p>`,
		Status:      domain.StatusDraft,
	}); err != nil || len(errs) > 0 {
		t.Fatalf("Save B: errs = %v, err = %v", errs, err)
	}

	file, err := q.CreateFile(ctx, query.CreateFileParams{
		Key: key, Filename: "photo.jpg", ByteSize: 4, CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	blob := filepath.Join(dataDir, "files", key[0:2], key[2:4], key)
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatalf("mkdir blob: %v", err)
	}
	if err := os.WriteFile(blob, []byte("blob"), 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := q.CreateAttachment(ctx, query.CreateAttachmentParams{
		FileID: file.ID, RecordType: "Article", RecordID: articleA.ID, Name: "embeds", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	if err := Destroy(ctx, database, articleA.ID, dataDir); err != nil {
		t.Fatalf("destroy A: %v", err)
	}

	if _, err := q.GetAdminArticleByID(ctx, articleA.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("article A still present: %v", err)
	}
	var files int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files WHERE id = ?`, file.ID).Scan(&files); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if files != 1 {
		t.Errorf("files row = %d, want 1 (still referenced by article B content)", files)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Errorf("blob removed while article B content still references it: %v", err)
	}
}

// TestEnqueuePublishEffectsSkipsRecordedURL: a selected platform whose post
// URL is already recorded enqueues no crosspost job — a twitter-sync archive
// carries its tweet URL and a successful crosspost records its post URL, so
// saving with the box checked must not publish a duplicate. Clearing the URL
// re-arms crossposting.
func TestEnqueuePublishEffectsSkipsRecordedURL(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := t.Context()
	q := query.New(database)
	now := time.Now()

	article, errs, err := Save(ctx, database, nil, SaveParams{
		Title:       "Post",
		ContentType: string(domain.ContentTypeMarkdown),
		ContentHTML: "body",
		Status:      domain.StatusDraft,
	})
	if err != nil || len(errs) > 0 {
		t.Fatalf("Save: errs = %v, err = %v", errs, err)
	}
	if _, err := database.Exec(`INSERT INTO crossposts (platform, enabled, created_at, updated_at) VALUES ('twitter', 1, 1, 1)`); err != nil {
		t.Fatalf("enable twitter crosspost: %v", err)
	}

	countJobs := func() int {
		runs, err := q.ListQueuedJobRunsByKind(ctx, jobs.KindCrosspost)
		if err != nil {
			t.Fatalf("list crosspost jobs: %v", err)
		}
		return len(runs)
	}
	selected := map[string]bool{"twitter": true}

	if err := EnqueuePublishEffects(ctx, q, article.ID, selected, false, now); err != nil {
		t.Fatalf("enqueue without recorded url: %v", err)
	}
	if n := countJobs(); n != 1 {
		t.Fatalf("crosspost jobs = %d, want 1 without a recorded url", n)
	}

	// The twitter-sync / crosspost write path: a recorded post URL.
	if err := q.UpsertSocialMediaPost(ctx, query.UpsertSocialMediaPostParams{
		ArticleID: article.ID, Platform: "twitter", Url: "https://x.com/me/status/1",
		CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("record social post: %v", err)
	}
	if err := EnqueuePublishEffects(ctx, q, article.ID, selected, false, now); err != nil {
		t.Fatalf("enqueue with recorded url: %v", err)
	}
	if n := countJobs(); n != 1 {
		t.Errorf("crosspost jobs = %d, want 1 (recorded url skips the re-post)", n)
	}

	if err := q.DeleteSocialMediaPost(ctx, query.DeleteSocialMediaPostParams{ArticleID: article.ID, Platform: "twitter"}); err != nil {
		t.Fatalf("delete social post: %v", err)
	}
	if err := EnqueuePublishEffects(ctx, q, article.ID, selected, false, now); err != nil {
		t.Fatalf("enqueue after clearing url: %v", err)
	}
	if n := countJobs(); n != 2 {
		t.Errorf("crosspost jobs = %d, want 2 (cleared url re-arms crossposting)", n)
	}
}
