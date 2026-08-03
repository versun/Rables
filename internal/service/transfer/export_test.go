package transfer

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"rables/internal/db"
	"rables/internal/jobs"
)

func newTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database, dataDir
}

// seedExportData inserts one row into every table the ZIP export reads, plus
// an on-disk blob for the files row.
func seedExportData(t *testing.T, database *sql.DB, dataDir string) {
	t.Helper()
	now := time.Now().Unix()
	stmts := []string{
		`INSERT INTO articles (id, title, slug, content_html, content_type, description, status, comment, scheduled_at, scheduled_crosspost_platforms, scheduled_send_newsletter, source_author, source_url, source_content, meta_title, meta_description, meta_image, excerpt, created_at, updated_at)
		 VALUES (1, 'Hello', 'hello', '<p>hi</p>', 'rich_text', 'desc', 1, 1, NULL, '[]', 0, 'author', 'https://x.example/1', 'src content', NULL, NULL, NULL, NULL, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO pages (id, title, slug, content_html, content_type, redirect_url, page_order, status, comment, scheduled_at, created_at, updated_at)
		 VALUES (1, 'About', 'about', '<p>about</p>', 'html', NULL, 2, 1, 0, NULL, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO tags (id, name, slug, created_at, updated_at) VALUES (1, 'Go', 'go', ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO article_tags (id, article_id, tag_id, created_at, updated_at) VALUES (1, 1, 1, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO comments (id, commentable_type, commentable_id, article_id, parent_id, author_name, author_email, author_url, author_username, author_avatar_url, content, status, platform, external_id, url, published_at, created_at, updated_at)
		 VALUES (1, 'Article', 1, 1, NULL, 'Ann', 'ann@example.com', NULL, NULL, NULL, 'nice', 1, NULL, NULL, NULL, NULL, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO settings (id, title, time_zone, setup_completed, created_at, updated_at) VALUES (1, 'Site', 'UTC', 1, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO crossposts (id, platform, enabled, api_key, api_key_secret, access_token, access_token_secret, client_id, client_key, client_secret, app_password, refresh_token, token_expires_at, server_url, username, max_characters, auto_fetch_comments, comment_fetch_schedule, settings, created_at, updated_at)
		 VALUES (1, 'twitter', 1, 'ak', 'aks', 'at', 'ats', 'cid', 'ck', 'cs', NULL, NULL, NULL, NULL, 'bob', NULL, 0, NULL, NULL, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO listmonks (id, url, username, api_key, list_id, template_id, enabled, created_at, updated_at)
		 VALUES (1, 'https://lm.example', 'admin', 'lmkey', 1, 2, 1, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO social_media_posts (id, article_id, platform, url, created_at, updated_at)
		 VALUES (1, 1, 'mastodon', 'https://m.example/1', ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO redirects (id, regex, replacement, enabled, permanent, created_at, updated_at)
		 VALUES (1, '^/old', '/new', 1, 1, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO newsletter_settings (id, enabled, provider, from_email, smtp_address, smtp_port, smtp_user_name, smtp_password, smtp_domain, smtp_authentication, smtp_enable_starttls, created_at, updated_at)
		 VALUES (1, 1, 'native', 'news@example.com', 'smtp.example.com', 587, 'smtpuser', 'smtppass', NULL, 'plain', 1, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO subscribers (id, email, confirmation_token, unsubscribe_token, confirmed_at, unsubscribed_at, created_at, updated_at)
		 VALUES (1, 'sub@example.com', 'ctok', 'utok', ` + itoa(now) + `, NULL, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO subscriber_tags (id, subscriber_id, tag_id, created_at, updated_at) VALUES (1, 1, 1, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO files (id, key, filename, content_type, byte_size, checksum, variant_of, created_at)
		 VALUES (1, 'abcd1234abcd1234abcd1234abcd1234', 'pic.png', 'image/png', 3, NULL, NULL, ` + itoa(now) + `)`,
		`INSERT INTO attachments (id, file_id, record_type, record_id, name, created_at)
		 VALUES (1, 1, 'article', 1, 'content', ` + itoa(now) + `)`,
		`INSERT INTO static_files (id, filename, description, file_id, created_at, updated_at)
		 VALUES (1, 'robots.txt', 'bots', 1, ` + itoa(now) + `, ` + itoa(now) + `)`,
	}
	for _, stmt := range stmts {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
	blobPath := filepath.Join(dataDir, "files", "ab", "cd", "abcd1234abcd1234abcd1234abcd1234")
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatalf("seed blob dir: %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("png"), 0o644); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// readZipCSV returns the parsed rows of one CSV entry in the zip.
func readZipCSV(t *testing.T, zr *zip.ReadCloser, name string) [][]string {
	t.Helper()
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open zip entry %s: %v", name, err)
			}
			defer rc.Close()
			rows, err := csv.NewReader(rc).ReadAll()
			if err != nil {
				t.Fatalf("parse zip entry %s: %v", name, err)
			}
			return rows
		}
	}
	t.Fatalf("zip entry %s missing", name)
	return nil
}

func zipEntryNames(zr *zip.ReadCloser) map[string]bool {
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	return names
}

func TestZipExportStructure(t *testing.T) {
	database, dataDir := newTestDB(t)
	seedExportData(t, database, dataDir)

	zipPath, err := (&ZipExporter{DB: database, DataDir: dataDir}).Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if filepath.Dir(zipPath) != filepath.Join(dataDir, "exports") {
		t.Errorf("zip not under data/exports: %s", zipPath)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	names := zipEntryNames(zr)
	for _, csvName := range []string{
		"articles.csv", "pages.csv", "tags.csv", "comments.csv", "settings.csv",
		"crossposts.csv", "listmonks.csv", "social_media_posts.csv",
		"static_files.csv", "redirects.csv", "newsletter_settings.csv",
		"subscribers.csv", "article_tags.csv", "subscriber_tags.csv",
		"files.csv", "attachments.csv",
	} {
		if !names[csvName] {
			t.Errorf("zip missing %s", csvName)
		}
	}
	if !names["attachments/files/1_pic.png"] {
		t.Errorf("zip missing attachment content, got %v", names)
	}

	// Fixed column order, spot-checked against the package doc.
	wantHeaders := map[string]string{
		"articles.csv":    "id,title,slug,content_html,content_type,description,excerpt,meta_description,meta_title,meta_image,source_author,source_url,source_content,status,comment,scheduled_at,scheduled_crosspost_platforms,scheduled_send_newsletter,created_at,updated_at",
		"comments.csv":    "id,commentable_type,commentable_id,article_id,article_slug,parent_id,author_name,author_email,author_url,author_username,author_avatar_url,content,status,platform,external_id,url,published_at,created_at,updated_at",
		"subscribers.csv": "id,email,confirmed_at,unsubscribed_at,created_at,updated_at",
		"files.csv":       "id,key,filename,content_type,byte_size,checksum,variant_of,created_at",
	}
	for name, want := range wantHeaders {
		rows := readZipCSV(t, zr, name)
		if got := strings.Join(rows[0], ","); got != want {
			t.Errorf("%s header = %q, want %q", name, got, want)
		}
		if len(rows) != 2 {
			t.Errorf("%s rows = %d, want header + 1 data row", name, len(rows))
		}
	}

	// Join helper columns and raw values.
	articleTags := readZipCSV(t, zr, "article_tags.csv")
	if got := articleTags[1][2]; got != "hello" {
		t.Errorf("article_tags article_slug = %q, want hello", got)
	}
	subscriberTags := readZipCSV(t, zr, "subscriber_tags.csv")
	if got := subscriberTags[1][2]; got != "sub@example.com" {
		t.Errorf("subscriber_tags subscriber_email = %q, want sub@example.com", got)
	}
	// Subscriber tokens are never exported.
	if rows := readZipCSV(t, zr, "subscribers.csv"); len(rows[1]) != 6 {
		t.Errorf("subscribers row has %d columns, want 6 (no tokens)", len(rows[1]))
	}

	// Attachment content round-trips.
	for _, f := range zr.File {
		if f.Name == "attachments/files/1_pic.png" {
			rc, _ := f.Open()
			data, _ := io.ReadAll(rc)
			rc.Close()
			if string(data) != "png" {
				t.Errorf("attachment content = %q, want png", data)
			}
		}
	}
}

func TestZipExportRedaction(t *testing.T) {
	for _, tt := range []struct {
		name            string
		keepCredentials bool
		wantAPIKey      string
		wantSMTPPass    string
		wantListmonkKey string
	}{
		{"default redacts", false, RedactedValue, RedactedValue, RedactedValue},
		{"keep credentials", true, "ak", "smtppass", "lmkey"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			database, dataDir := newTestDB(t)
			seedExportData(t, database, dataDir)

			zipPath, err := (&ZipExporter{DB: database, DataDir: dataDir, KeepCredentials: tt.keepCredentials}).Generate(context.Background())
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			zr, err := zip.OpenReader(zipPath)
			if err != nil {
				t.Fatalf("open zip: %v", err)
			}
			defer zr.Close()

			crossposts := readZipCSV(t, zr, "crossposts.csv")
			header := crossposts[0]
			row := crossposts[1]
			col := func(name string) string {
				for i, h := range header {
					if h == name {
						return row[i]
					}
				}
				t.Fatalf("crossposts.csv missing column %s", name)
				return ""
			}
			if got := col("api_key"); got != tt.wantAPIKey {
				t.Errorf("crossposts api_key = %q, want %q", got, tt.wantAPIKey)
			}
			// Every secret column: redacted by default, verbatim when kept.
			secrets := map[string]string{
				"api_key_secret": "aks", "access_token": "at", "access_token_secret": "ats",
				"client_key": "ck", "client_secret": "cs",
			}
			for secretCol, plain := range secrets {
				want := RedactedValue
				if tt.keepCredentials {
					want = plain
				}
				if got := col(secretCol); got != want {
					t.Errorf("crossposts %s = %q, want %q", secretCol, got, want)
				}
			}
			if got := col("client_id"); got != "cid" {
				t.Errorf("client_id = %q, want cid (not a secret)", got)
			}
			if got := col("username"); got != "bob" {
				t.Errorf("username = %q, want bob (not a secret)", got)
			}

			newsletter := readZipCSV(t, zr, "newsletter_settings.csv")
			var smtpPass string
			for i, h := range newsletter[0] {
				if h == "smtp_password" {
					smtpPass = newsletter[1][i]
				}
			}
			if smtpPass != tt.wantSMTPPass {
				t.Errorf("newsletter smtp_password = %q, want %q", smtpPass, tt.wantSMTPPass)
			}

			listmonks := readZipCSV(t, zr, "listmonks.csv")
			var lmKey string
			for i, h := range listmonks[0] {
				if h == "api_key" {
					lmKey = listmonks[1][i]
				}
			}
			if lmKey != tt.wantListmonkKey {
				t.Errorf("listmonks api_key = %q, want %q", lmKey, tt.wantListmonkKey)
			}
		})
	}
}

func TestMarkdownExport(t *testing.T) {
	database, dataDir := newTestDB(t)
	now := time.Now().Unix()
	stmts := []string{
		`INSERT INTO articles (id, title, slug, content_html, content_type, status, comment, scheduled_crosspost_platforms, created_at, updated_at)
		 VALUES (1, 'Hello <World>', 'hello!', '<h1>Title</h1><p>body <strong>bold</strong></p>', 'rich_text', 1, 0, '[]', ` + itoa(now) + `, ` + itoa(now) + `)`,
		// Slug normalizes to the same basename as article 1 -> dedupe.
		`INSERT INTO articles (id, title, slug, content_html, content_type, status, comment, scheduled_crosspost_platforms, source_author, source_url, source_content, created_at, updated_at)
		 VALUES (2, 'Second', 'hello?', '<p>two</p>', 'rich_text', 1, 0, '[]', 'someone', 'https://x.example/post', 'quoted<br>text', ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO tags (id, name, slug, created_at, updated_at) VALUES (1, 'Go', 'go', ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO article_tags (id, article_id, tag_id, created_at, updated_at) VALUES (1, 1, 1, ` + itoa(now) + `, ` + itoa(now) + `)`,
		`INSERT INTO pages (id, title, slug, content_html, content_type, page_order, status, comment, created_at, updated_at)
		 VALUES (1, 'About', 'about', '<p>about</p>', 'html', 0, 1, 0, ` + itoa(now) + `, ` + itoa(now) + `)`,
	}
	for _, stmt := range stmts {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	zipPath, err := (&MarkdownExporter{DB: database, DataDir: dataDir}).Generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	contents := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", f.Name, err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		contents[f.Name] = string(data)
	}

	for _, want := range []string{"articles/hello.md", "articles/hello-hello?.md", "pages/about.md"} {
		if _, ok := contents[want]; !ok {
			t.Errorf("missing %s, got %v", want, contents)
		}
	}

	// Front matter parses as YAML and carries the expected fields.
	body := contents["articles/hello.md"]
	if !strings.HasPrefix(body, "---\n") {
		t.Fatalf("front matter missing: %q", body)
	}
	parts := strings.SplitN(body[4:], "---\n", 2)
	if len(parts) != 2 {
		t.Fatalf("front matter not closed: %q", body)
	}
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(parts[0]), &fm); err != nil {
		t.Fatalf("front matter not valid YAML: %v", err)
	}
	if fm["type"] != "article" || fm["slug"] != "hello!" || fm["status"] != "publish" {
		t.Errorf("front matter = %v", fm)
	}
	if fm["title"] != "Hello <World>" {
		t.Errorf("front matter title = %v", fm["title"])
	}
	tags, ok := fm["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "Go" {
		t.Errorf("front matter tags = %v", fm["tags"])
	}
	if !strings.Contains(parts[1], "# Title") || !strings.Contains(parts[1], "**bold**") {
		t.Errorf("markdown body = %q", parts[1])
	}

	// Reference block for sourced articles (source_content <br> -> newline).
	second := contents["articles/hello-hello?.md"]
	if !strings.Contains(second, "Reference:") || !strings.Contains(second, "Source: someone") ||
		!strings.Contains(second, "> quoted\n> text") || !strings.Contains(second, "> Original: https://x.example/post") {
		t.Errorf("reference block = %q", second)
	}

	page := contents["pages/about.md"]
	if !strings.Contains(page, "type: page") || !strings.Contains(page, "page_order: 0") {
		t.Errorf("page front matter = %q", page)
	}
}

func TestExportJobEndToEnd(t *testing.T) {
	database, dataDir := newTestDB(t)
	seedExportData(t, database, dataDir)

	worker := jobs.NewWorker(database)
	RegisterExportHandlers(worker, database, dataDir)
	enqueuer := jobs.NewEnqueuer(database)

	if _, err := enqueuer.Enqueue(context.Background(), jobs.KindExport, ExportPayload{Format: "markdown"}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if !claimed {
		t.Fatal("no job claimed")
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "exports"))
	if err != nil {
		t.Fatalf("read exports dir: %v", err)
	}
	var zips []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".zip") {
			zips = append(zips, entry.Name())
		}
	}
	if len(zips) != 1 || !strings.HasPrefix(zips[0], "markdown_export_") {
		t.Errorf("exports dir = %v, want one markdown_export_*.zip", zips)
	}

	// The completed activity mirrors ExportDataJob.
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM activity_logs WHERE target = 'export' AND action = 'completed'`).Scan(&count); err != nil {
		t.Fatalf("query activity: %v", err)
	}
	if count != 1 {
		t.Errorf("completed activity rows = %d, want 1", count)
	}
}

func TestSafeBasename(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"hello-world", "hello-world"},
		{"hello world", "hello_world"},
		{"a/b\\c:d*e?f\"g<h>i|j", "a_b_c_d_e_f_g_h_i_j"},
		{"..", ""},
		{"__x__", "x"},
		{"trailing.", "trailing"},
		{"中文 slug", "中文_slug"},
	} {
		got := safeBasename(tt.in)
		if tt.want == "" {
			if got == "" {
				t.Errorf("safeBasename(%q) = empty, want random fallback", tt.in)
			}
			continue
		}
		if got != tt.want {
			t.Errorf("safeBasename(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
