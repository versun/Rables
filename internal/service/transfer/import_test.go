package transfer

import (
	"archive/zip"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rables/internal/jobs"
)

// buildImportZip writes a zip with the given name->content entries.
func buildImportZip(t *testing.T, dir, zipName string, entries map[string]string) string {
	t.Helper()
	zipPath := filepath.Join(dir, zipName)
	out, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(out)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create entry %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return zipPath
}

func tableCount(t *testing.T, database *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// importTables are the 16 tables the ZIP export/import round-trip covers.
var importTables = []string{
	"articles", "pages", "tags", "article_tags", "comments", "settings",
	"crossposts", "listmonks", "social_media_posts", "static_files",
	"redirects", "newsletter_settings", "subscribers", "subscriber_tags",
	"files", "attachments",
}

// TestZipImportRoundTrip exports the seeded database, imports the bundle into
// a fresh one, and expects identical table counts plus a restored blob.
func TestZipImportRoundTrip(t *testing.T) {
	srcDB, srcDir := newTestDB(t)
	seedExportData(t, srcDB, srcDir)
	zipPath, err := (&ZipExporter{DB: srcDB, DataDir: srcDir}).Generate(context.Background())
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	dstDB, dstDir := newTestDB(t)
	result, err := (&ZipImporter{DB: dstDB, DataDir: dstDir}).Import(context.Background(), zipPath)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	for _, table := range importTables {
		got := tableCount(t, dstDB, table)
		want := tableCount(t, srcDB, table)
		if got != want {
			t.Errorf("%s: imported rows = %d, want %d", table, got, want)
		}
		if result.Imported[table]+result.Updated[table] != want {
			t.Errorf("%s: result imported+updated = %d, want %d (result %+v)", table, result.Imported[table]+result.Updated[table], want, result)
		}
	}

	// The blob content lands in the media layout with the exported key.
	blob, err := os.ReadFile(filepath.Join(dstDir, "files", "ab", "cd", "abcd1234abcd1234abcd1234abcd1234"))
	if err != nil {
		t.Fatalf("restored blob: %v", err)
	}
	if string(blob) != "png" {
		t.Errorf("blob content = %q, want png", blob)
	}

	// Redacted credentials import as NULL on a fresh database, and a
	// crosspost row enabled without its credentials imports disabled.
	var apiKey sql.NullString
	var enabled int64
	if err := dstDB.QueryRow(`SELECT api_key, enabled FROM crossposts WHERE platform = 'twitter'`).Scan(&apiKey, &enabled); err != nil {
		t.Fatalf("query crosspost: %v", err)
	}
	if apiKey.Valid {
		t.Errorf("api_key = %v, want NULL (redacted)", apiKey)
	}
	if enabled != 0 {
		t.Errorf("enabled = %d, want 0 (disabled without credentials)", enabled)
	}
	var lmKey sql.NullString
	if err := dstDB.QueryRow(`SELECT api_key FROM listmonks WHERE id = 1`).Scan(&lmKey); err != nil {
		t.Fatalf("query listmonk: %v", err)
	}
	if lmKey.Valid {
		t.Errorf("listmonk api_key = %v, want NULL (redacted)", lmKey)
	}

	// Imported subscribers get fresh tokens (exports never carry them).
	var confirmLen, unsubLen int
	if err := dstDB.QueryRow(`SELECT LENGTH(confirmation_token), LENGTH(unsubscribe_token) FROM subscribers`).Scan(&confirmLen, &unsubLen); err != nil {
		t.Fatalf("query subscriber tokens: %v", err)
	}
	if confirmLen != 43 || unsubLen != 43 {
		t.Errorf("token lengths = %d/%d, want 43/43", confirmLen, unsubLen)
	}

	// Re-importing the same bundle is idempotent: stored credentials survive
	// the redacted placeholders and every dedupe rule skips.
	if _, err := dstDB.Exec(`UPDATE crossposts SET api_key = 'real-ak', api_key_secret = 'real-aks', access_token = 'real-at', access_token_secret = 'real-ats' WHERE platform = 'twitter'`); err != nil {
		t.Fatalf("set real credentials: %v", err)
	}
	before := map[string]int{}
	for _, table := range importTables {
		before[table] = tableCount(t, dstDB, table)
	}
	second, err := (&ZipImporter{DB: dstDB, DataDir: dstDir}).Import(context.Background(), zipPath)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	for _, table := range importTables {
		if got := tableCount(t, dstDB, table); got != before[table] {
			t.Errorf("%s: rows after re-import = %d, want %d", table, got, before[table])
		}
	}
	for _, table := range []string{"articles", "pages", "tags", "comments", "subscribers", "redirects", "files", "static_files", "social_media_posts", "article_tags", "subscriber_tags", "attachments"} {
		if second.Imported[table] != 0 {
			t.Errorf("%s: re-import inserted %d rows, want 0", table, second.Imported[table])
		}
	}
	var keptKey string
	if err := dstDB.QueryRow(`SELECT api_key FROM crossposts WHERE platform = 'twitter'`).Scan(&keptKey); err != nil {
		t.Fatalf("query crosspost after re-import: %v", err)
	}
	if keptKey != "real-ak" {
		t.Errorf("api_key = %q, want real-ak (redacted placeholder must not overwrite)", keptKey)
	}
	// With credentials present, the CSV's enabled=1 now applies.
	if err := dstDB.QueryRow(`SELECT enabled FROM crossposts WHERE platform = 'twitter'`).Scan(&enabled); err != nil {
		t.Fatalf("query enabled: %v", err)
	}
	if enabled != 1 {
		t.Errorf("enabled = %d, want 1 once credentials are present", enabled)
	}
}

// TestZipImportDedupeSkips covers the per-row skip rules: pre-existing rows
// matched by slug/email/regex/(platform,external_id) stay untouched.
func TestZipImportDedupeSkips(t *testing.T) {
	srcDB, srcDir := newTestDB(t)
	seedExportData(t, srcDB, srcDir)
	zipPath, err := (&ZipExporter{DB: srcDB, DataDir: srcDir, KeepCredentials: true}).Generate(context.Background())
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	dstDB, dstDir := newTestDB(t)
	now := time.Now().Unix()
	preseed := []string{
		`INSERT INTO tags (id, name, slug, created_at, updated_at) VALUES (1, 'OldName', 'go', ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO articles (id, title, slug, content_html, content_type, status, comment, scheduled_crosspost_platforms, created_at, updated_at)
		 VALUES (1, 'OldTitle', 'hello', '<p>old</p>', 'rich_text', 1, 0, '[]', ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO subscribers (id, email, created_at, updated_at) VALUES (1, 'sub@example.com', ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO redirects (id, regex, replacement, enabled, permanent, created_at, updated_at) VALUES (1, '^/old', '/older', 1, 0, ` + itoa(now) + `, ` + itoa(now) + `)`,
	}
	for _, stmt := range preseed {
		if _, err := dstDB.Exec(stmt); err != nil {
			t.Fatalf("preseed: %v\n%s", err, stmt)
		}
	}

	result, err := (&ZipImporter{DB: dstDB, DataDir: dstDir}).Import(context.Background(), zipPath)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// Natural-key duplicates are skipped, leaving the existing rows alone.
	for _, tt := range []struct{ table, column, want string }{
		{"tags", "name", "OldName"},
		{"articles", "title", "OldTitle"},
		{"redirects", "replacement", "/older"},
	} {
		var got string
		if err := dstDB.QueryRow("SELECT " + tt.column + " FROM " + tt.table).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", tt.table, err)
		}
		if got != tt.want {
			t.Errorf("%s.%s = %q, want %q (not overwritten)", tt.table, tt.column, got, tt.want)
		}
	}
	for _, table := range []string{"tags", "articles", "subscribers", "redirects"} {
		if got := tableCount(t, dstDB, table); got != 1 {
			t.Errorf("%s: rows = %d, want 1 (dupe skipped)", table, got)
		}
		if result.Skipped[table] != 1 {
			t.Errorf("%s: skipped = %d, want 1", table, result.Skipped[table])
		}
	}
	// Associations onto the pre-existing article still import.
	if got := tableCount(t, dstDB, "article_tags"); got != 1 {
		t.Errorf("article_tags: rows = %d, want 1 (attached to existing article)", got)
	}
	if got := tableCount(t, dstDB, "comments"); got != 1 {
		t.Errorf("comments: rows = %d, want 1 (attached to existing article)", got)
	}
}

const importTestArticlesCSV = `id,title,slug,content_html,content_type,description,excerpt,meta_description,meta_title,meta_image,source_author,source_url,source_content,status,comment,scheduled_at,scheduled_crosspost_platforms,scheduled_send_newsletter,created_at,updated_at
10,Post,post,<p>x</p>,rich_text,,,,,,,,,1,0,,[],0,1000,1000
`

const importTestCommentsCSV = `id,commentable_type,commentable_id,article_id,article_slug,parent_id,author_name,author_email,author_url,author_username,author_avatar_url,content,status,platform,external_id,url,published_at,created_at,updated_at
100,Article,10,10,post,,Ann,,,,,root,1,,,,,1000,1000
101,Article,10,10,post,100,Bob,,,,,child,0,,,,,1001,1001
102,Article,10,10,post,101,Eve,,,,,grandchild,1,mastodon,e1,https://m.example/1,1002,1002,1002
`

// TestZipImportCommentParentBackfill covers the two-pass comment import:
// parent_id is NULL in the first pass and rewired through the old->new id
// map in the second.
func TestZipImportCommentParentBackfill(t *testing.T) {
	database, dataDir := newTestDB(t)
	zipPath := buildImportZip(t, dataDir, "comments.zip", map[string]string{
		"articles.csv": importTestArticlesCSV,
		"comments.csv": importTestCommentsCSV,
	})

	result, err := (&ZipImporter{DB: database, DataDir: dataDir}).Import(context.Background(), zipPath)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported["comments"] != 3 {
		t.Fatalf("imported comments = %d, want 3", result.Imported["comments"])
	}

	rows, err := database.Query(`SELECT content, parent_id FROM comments ORDER BY id`)
	if err != nil {
		t.Fatalf("query comments: %v", err)
	}
	defer rows.Close()
	parents := map[string]sql.NullInt64{}
	for rows.Next() {
		var content string
		var parent sql.NullInt64
		if err := rows.Scan(&content, &parent); err != nil {
			t.Fatalf("scan: %v", err)
		}
		parents[content] = parent
	}
	var rootID, childID int64
	if err := database.QueryRow(`SELECT id FROM comments WHERE content = 'root'`).Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT id FROM comments WHERE content = 'child'`).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	if parents["root"].Valid {
		t.Errorf("root parent_id = %v, want NULL", parents["root"])
	}
	if !parents["child"].Valid || parents["child"].Int64 != rootID {
		t.Errorf("child parent_id = %v, want %d", parents["child"], rootID)
	}
	if !parents["grandchild"].Valid || parents["grandchild"].Int64 != childID {
		t.Errorf("grandchild parent_id = %v, want %d", parents["grandchild"], childID)
	}
}

// TestZipImportOverdueSchedule covers the snapshot clearing: schedule
// articles whose time has passed import with empty crosspost platforms and
// no newsletter flag; future ones keep both.
func TestZipImportOverdueSchedule(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour).Unix()
	future := now.Add(time.Hour).Unix()

	var sb strings.Builder
	sb.WriteString("id,title,slug,content_html,content_type,description,excerpt,meta_description,meta_title,meta_image,source_author,source_url,source_content,status,comment,scheduled_at,scheduled_crosspost_platforms,scheduled_send_newsletter,created_at,updated_at\n")
	sb.WriteString("1,Overdue,overdue,<p>a</p>,rich_text,,,,,,,,,2,0," + itoa(past) + ",\"[\"\"twitter\"\"]\",1,1000,1000\n")
	sb.WriteString("2,Future,future,<p>b</p>,rich_text,,,,,,,,,2,0," + itoa(future) + ",\"[\"\"twitter\"\"]\",1,1000,1000\n")
	sb.WriteString("3,Published,published,<p>c</p>,rich_text,,,,,,,,,1,0,,\"[\"\"twitter\"\"]\",1,1000,1000\n")

	database, dataDir := newTestDB(t)
	zipPath := buildImportZip(t, dataDir, "schedule.zip", map[string]string{"articles.csv": sb.String()})
	imp := &ZipImporter{DB: database, DataDir: dataDir, Now: func() time.Time { return now }}
	if _, err := imp.Import(context.Background(), zipPath); err != nil {
		t.Fatalf("import: %v", err)
	}

	for _, tt := range []struct {
		slug           string
		wantPlatforms  string
		wantNewsletter int64
	}{
		{"overdue", "[]", 0},
		{"future", `["twitter"]`, 1},
		// Only schedule rows are touched; other statuses keep their values.
		{"published", `["twitter"]`, 1},
	} {
		var platforms string
		var newsletter int64
		if err := database.QueryRow(`SELECT scheduled_crosspost_platforms, scheduled_send_newsletter FROM articles WHERE slug = ?`, tt.slug).Scan(&platforms, &newsletter); err != nil {
			t.Fatalf("query %s: %v", tt.slug, err)
		}
		if platforms != tt.wantPlatforms || newsletter != tt.wantNewsletter {
			t.Errorf("%s: snapshot = %q/%d, want %q/%d", tt.slug, platforms, newsletter, tt.wantPlatforms, tt.wantNewsletter)
		}
	}
}

// TestZipImportPathTraversal rejects zip entries escaping the staging dir.
func TestZipImportPathTraversal(t *testing.T) {
	for _, entry := range []string{"../evil.txt", "/abs/evil.txt", "sub/../../evil.txt"} {
		t.Run(entry, func(t *testing.T) {
			database, dataDir := newTestDB(t)
			zipPath := buildImportZip(t, dataDir, "traversal.zip", map[string]string{
				entry:      "evil",
				"tags.csv": "id,name,slug,created_at,updated_at\n1,Go,go,1000,1000\n",
			})
			_, err := (&ZipImporter{DB: database, DataDir: dataDir}).Import(context.Background(), zipPath)
			if err == nil || !strings.Contains(err.Error(), "unsafe path") {
				t.Fatalf("error = %v, want unsafe path rejection", err)
			}
			if got := tableCount(t, database, "tags"); got != 0 {
				t.Errorf("tags = %d, want 0 (import must abort)", got)
			}
			if _, statErr := os.Stat(filepath.Join(dataDir, "evil.txt")); !os.IsNotExist(statErr) {
				t.Errorf("evil.txt escaped the staging dir")
			}
		})
	}
}

// TestZipImportRollback aborts mid-import (invalid comment status) and
// expects the rows from earlier passes to be rolled back too.
func TestZipImportRollback(t *testing.T) {
	database, dataDir := newTestDB(t)
	zipPath := buildImportZip(t, dataDir, "rollback.zip", map[string]string{
		"tags.csv":     "id,name,slug,created_at,updated_at\n1,Go,go,1000,1000\n",
		"articles.csv": importTestArticlesCSV,
		"comments.csv": strings.Replace(importTestCommentsCSV, ",1,,,,,1000,1000\n", ",9,,,,,1000,1000\n", 1),
	})

	_, err := (&ZipImporter{DB: database, DataDir: dataDir}).Import(context.Background(), zipPath)
	if err == nil || !strings.Contains(err.Error(), "invalid comment status") {
		t.Fatalf("error = %v, want invalid comment status", err)
	}
	for _, table := range []string{"tags", "articles", "comments"} {
		if got := tableCount(t, database, table); got != 0 {
			t.Errorf("%s = %d, want 0 (transaction rolled back)", table, got)
		}
	}
}

// TestZipImportJobEndToEnd drives the import through job_runs: the uploaded
// zip is consumed and deleted, and the activity rows mirror the Rails job.
func TestZipImportJobEndToEnd(t *testing.T) {
	database, dataDir := newTestDB(t)

	importsDir := filepath.Join(dataDir, "imports")
	if err := os.MkdirAll(importsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	good := buildImportZip(t, importsDir, "import_good.zip", map[string]string{"tags.csv": "id,name,slug,created_at,updated_at\n1,Go,go,1000,1000\n"})
	bad := buildImportZip(t, importsDir, "import_bad.zip", map[string]string{"tags.csv": "id,name,slug,created_at,updated_at\n1,Go,go,1000,1000\n2,Go,go2,1000,1000\n"})

	worker := jobs.NewWorker(database)
	RegisterImportHandlers(worker, database, dataDir)
	enqueuer := jobs.NewEnqueuer(database)

	for _, tt := range []struct {
		name       string
		path       string
		wantTags   int
		wantAction string
	}{
		{"success", good, 1, "completed"},
		{"duplicate tag name aborts", bad, 1, "failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := enqueuer.Enqueue(context.Background(), jobs.KindImportZip, ImportZipPayload{Path: tt.path}, time.Now()); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			claimed, err := worker.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("run once: %v", err)
			}
			if !claimed {
				t.Fatal("no job claimed")
			}
			// A failed import is logged, not retried: the job still completes.
			var status string
			if err := database.QueryRow(`SELECT status FROM job_runs ORDER BY id DESC LIMIT 1`).Scan(&status); err != nil {
				t.Fatalf("query job: %v", err)
			}
			if status != "done" {
				t.Errorf("job status = %q, want done", status)
			}
			if _, err := os.Stat(tt.path); !os.IsNotExist(err) {
				t.Errorf("uploaded zip not cleaned up: %s", tt.path)
			}
			var count int
			if err := database.QueryRow(`SELECT COUNT(*) FROM activity_logs WHERE target = 'import' AND action = ?`, tt.wantAction).Scan(&count); err != nil {
				t.Fatalf("query activity: %v", err)
			}
			if count == 0 {
				t.Errorf("no %q activity row", tt.wantAction)
			}
			if got := tableCount(t, database, "tags"); got != tt.wantTags {
				t.Errorf("tags = %d, want %d", got, tt.wantTags)
			}
		})
	}
}

// TestZipImportStaticFileBlobRequired mirrors import_static_files: a row
// whose blob is neither in the bundle nor on disk is skipped.
func TestZipImportStaticFileBlobRequired(t *testing.T) {
	database, dataDir := newTestDB(t)
	zipPath := buildImportZip(t, dataDir, "static.zip", map[string]string{
		"files.csv": "id,key,filename,content_type,byte_size,checksum,variant_of,created_at\n" +
			"5,abcd1234abcd1234abcd1234abcd1234,robots.txt,text/plain,3,,,1000\n",
		"static_files.csv": "id,filename,file_id,blob_filename,description,created_at,updated_at\n" +
			"1,robots.txt,5,robots.txt,,1000,1000\n",
		// No attachments/files/5_robots.txt entry: the blob is missing.
	})

	result, err := (&ZipImporter{DB: database, DataDir: dataDir}).Import(context.Background(), zipPath)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := tableCount(t, database, "files"); got != 1 {
		t.Errorf("files = %d, want 1 (row imports without blob)", got)
	}
	if got := tableCount(t, database, "static_files"); got != 0 {
		t.Errorf("static_files = %d, want 0 (skipped, blob missing)", got)
	}
	if result.Skipped["static_files"] != 1 {
		t.Errorf("static_files skipped = %d, want 1", result.Skipped["static_files"])
	}

	// Same bundle plus the blob: the row imports and the content lands on disk.
	zipPath2 := buildImportZip(t, dataDir, "static2.zip", map[string]string{
		"files.csv": "id,key,filename,content_type,byte_size,checksum,variant_of,created_at\n" +
			"6,bbcd1234abcd1234abcd1234abcd1234,humans.txt,text/plain,2,,,1000\n",
		"static_files.csv": "id,filename,file_id,blob_filename,description,created_at,updated_at\n" +
			"2,humans.txt,6,humans.txt,,1000,1000\n",
		"attachments/files/6_humans.txt": "hi",
	})
	if _, err := (&ZipImporter{DB: database, DataDir: dataDir}).Import(context.Background(), zipPath2); err != nil {
		t.Fatalf("import with blob: %v", err)
	}
	if got := tableCount(t, database, "static_files"); got != 1 {
		t.Fatalf("static_files = %d, want 1", got)
	}
	content, err := os.ReadFile(filepath.Join(dataDir, "files", "bb", "cd", "bbcd1234abcd1234abcd1234abcd1234"))
	if err != nil {
		t.Fatalf("read restored blob: %v", err)
	}
	if string(content) != "hi" {
		t.Errorf("blob = %q, want hi", content)
	}
	// static_files.file_id points at the newly imported files row.
	var fileKey string
	if err := database.QueryRow(`SELECT files.key FROM static_files JOIN files ON files.id = static_files.file_id WHERE static_files.filename = 'humans.txt'`).Scan(&fileKey); err != nil {
		t.Fatalf("query static file join: %v", err)
	}
	if fileKey != "bbcd1234abcd1234abcd1234abcd1234" {
		t.Errorf("file key = %q, want the imported key", fileKey)
	}
}

// TestZipImportAttachmentsRemap checks the old->new id remapping of
// attachment rows and the unique-tuple dedupe.
func TestZipImportAttachmentsRemap(t *testing.T) {
	database, dataDir := newTestDB(t)
	zipPath := buildImportZip(t, dataDir, "comments.zip", map[string]string{
		"articles.csv": importTestArticlesCSV,
		"files.csv": "id,key,filename,content_type,byte_size,checksum,variant_of,created_at\n" +
			"7,abcd1234abcd1234abcd1234abcd1234,pic.png,image/png,3,,,1000\n",
		"attachments.csv": "id,file_id,record_type,record_id,name,created_at\n" +
			"1,7,Article,10,embeds,1000\n" +
			"2,7,ActionText::RichText,99,content,1000\n" +
			"3,99,Article,10,embeds,1000\n",
	})

	result, err := (&ZipImporter{DB: database, DataDir: dataDir}).Import(context.Background(), zipPath)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported["attachments"] != 2 {
		t.Errorf("attachments imported = %d, want 2 (one unresolved file skipped)", result.Imported["attachments"])
	}
	var recordID, fileID int64
	if err := database.QueryRow(`SELECT record_id, file_id FROM attachments WHERE record_type = 'Article'`).Scan(&recordID, &fileID); err != nil {
		t.Fatalf("query attachment: %v", err)
	}
	var newArticleID, newFileID int64
	if err := database.QueryRow(`SELECT id FROM articles WHERE slug = 'post'`).Scan(&newArticleID); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT id FROM files WHERE key = 'abcd1234abcd1234abcd1234abcd1234'`).Scan(&newFileID); err != nil {
		t.Fatal(err)
	}
	if recordID != newArticleID || fileID != newFileID {
		t.Errorf("attachment = (record %d, file %d), want (%d, %d)", recordID, fileID, newArticleID, newFileID)
	}
	// Non-remappable record types keep their record_id verbatim (provenance).
	var legacy int64
	if err := database.QueryRow(`SELECT record_id FROM attachments WHERE record_type = 'ActionText::RichText'`).Scan(&legacy); err != nil {
		t.Fatalf("query legacy attachment: %v", err)
	}
	if legacy != 99 {
		t.Errorf("legacy record_id = %d, want 99 (verbatim)", legacy)
	}

	// Re-import dedupes on the full tuple.
	second, err := (&ZipImporter{DB: database, DataDir: dataDir}).Import(context.Background(), zipPath)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if second.Imported["attachments"] != 0 {
		t.Errorf("re-import inserted %d attachments, want 0", second.Imported["attachments"])
	}
}
