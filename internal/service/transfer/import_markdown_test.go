package transfer

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rables/internal/jobs"
)

// markdownTestArticle is one export-format article document.
const markdownTestArticle = `---
type: article
id: 24
title: 第99期
slug: '99'
status: publish
created_at: '2024-11-23T00:00:54+08:00'
updated_at: '2026-01-08T09:24:44+08:00'
tags: [weekly, 周刊]
---

Hello **world**.

![pic](https://example.com/pic.jpg)

<script>alert(1)</script>
`

// markdownZip writes one ZIP of markdown documents into dir and returns its
// path. Entries map archive names to file contents.
func markdownZip(t *testing.T, dir string, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, "import_test.zip")
	out, err := os.Create(path)
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
	return path
}

// TestMarkdownImportZip imports a ZIP carrying an article and two pages in
// the markdown-export layout and checks the stored rows field by field.
func TestMarkdownImportZip(t *testing.T) {
	database, dataDir := newTestDB(t)
	path := markdownZip(t, dataDir, map[string]string{
		"articles/99.md":    markdownTestArticle,
		"pages/rss.md":      "---\ntype: page\ntitle: RSS\nslug: rss\nstatus: publish\nredirect_url: https://54321.versun.me/rss\npage_order: 1\ncreated_at: '2025-11-20T21:05:02+08:00'\nupdated_at: '2026-01-08T09:24:45+08:00'\n---\n\nrss\n",
		"pages/suspend.md":  "---\ntype: page\ntitle: 已暂停更新\nslug: suspended\nstatus: publish\nredirect_url: https://versun.me/blog/suspended\npage_order: 0\n---\n\n",
		"attachments/x.txt": "not a markdown file",
	})

	res, err := (&MarkdownImporter{DB: database}).Import(context.Background(), path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 3 || res.Failed != 0 {
		t.Fatalf("result = imported %d failed %d, want 3/0", res.Imported, res.Failed)
	}

	var title, slug, contentType, contentMarkdown, contentHTML string
	var status, createdAt, updatedAt int64
	err = database.QueryRow(`SELECT title, slug, content_type, content_markdown, content_html, status, created_at, updated_at
		FROM articles WHERE slug = '99'`).Scan(&title, &slug, &contentType, &contentMarkdown, &contentHTML, &status, &createdAt, &updatedAt)
	if err != nil {
		t.Fatalf("query article: %v", err)
	}
	if title != "第99期" || slug != "99" {
		t.Errorf("article title/slug = %q/%q", title, slug)
	}
	if contentType != "markdown" {
		t.Errorf("content_type = %q, want markdown", contentType)
	}
	if !strings.Contains(contentMarkdown, "Hello **world**.") {
		t.Errorf("content_markdown lost the source: %q", contentMarkdown)
	}
	if !strings.Contains(contentHTML, "<strong>world</strong>") {
		t.Errorf("content_html missing the rendered markdown: %q", contentHTML)
	}
	if !strings.Contains(contentHTML, `loading="lazy"`) {
		t.Errorf("content_html missing lazy loading on the image: %q", contentHTML)
	}
	if strings.Contains(contentHTML, "<script") {
		t.Errorf("content_html kept the script tag: %q", contentHTML)
	}
	if status != 1 {
		t.Errorf("status = %d, want publish(1)", status)
	}
	wantCreated, _ := time.Parse(time.RFC3339, "2024-11-23T00:00:54+08:00")
	wantUpdated, _ := time.Parse(time.RFC3339, "2026-01-08T09:24:44+08:00")
	if createdAt != wantCreated.Unix() || updatedAt != wantUpdated.Unix() {
		t.Errorf("timestamps = %d/%d, want %d/%d", createdAt, updatedAt, wantCreated.Unix(), wantUpdated.Unix())
	}

	var tagNames []string
	rows, err := database.Query(`SELECT t.name FROM tags t JOIN article_tags at ON at.tag_id = t.id
		JOIN articles a ON a.id = at.article_id WHERE a.slug = '99' ORDER BY t.name`)
	if err != nil {
		t.Fatalf("query tags: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan tag: %v", err)
		}
		tagNames = append(tagNames, name)
	}
	if strings.Join(tagNames, ",") != "weekly,周刊" {
		t.Errorf("article tags = %v, want [weekly 周刊]", tagNames)
	}

	var redirect string
	var pageOrder, pageStatus int64
	err = database.QueryRow(`SELECT redirect_url, page_order, status FROM pages WHERE slug = 'rss'`).
		Scan(&redirect, &pageOrder, &pageStatus)
	if err != nil {
		t.Fatalf("query rss page: %v", err)
	}
	if redirect != "https://54321.versun.me/rss" || pageOrder != 1 || pageStatus != 1 {
		t.Errorf("rss page = redirect %q order %d status %d", redirect, pageOrder, pageStatus)
	}
	// Redirect pages may carry a blank body.
	var content string
	if err := database.QueryRow(`SELECT content_html FROM pages WHERE slug = 'suspended'`).Scan(&content); err != nil {
		t.Fatalf("query suspended page: %v", err)
	}
	if content != "" {
		t.Errorf("blank-body page content_html = %q, want empty", content)
	}
}

// TestMarkdownImportBareFile imports one bare .md path (no ZIP wrapper).
func TestMarkdownImportBareFile(t *testing.T) {
	database, dataDir := newTestDB(t)
	path := filepath.Join(dataDir, "note.md")
	if err := os.WriteFile(path, []byte(markdownTestArticle), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (&MarkdownImporter{DB: database}).Import(context.Background(), path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 1 || res.Failed != 0 {
		t.Fatalf("result = imported %d failed %d, want 1/0", res.Imported, res.Failed)
	}
	if n := tableCount(t, database, "articles"); n != 1 {
		t.Errorf("articles = %d, want 1", n)
	}
}

// TestMarkdownImportSkips covers the per-document failure modes: each bad
// document counts as failed without aborting the batch.
func TestMarkdownImportSkips(t *testing.T) {
	database, dataDir := newTestDB(t)
	if _, err := database.Exec(`INSERT INTO articles (title, slug, content_html, status, created_at, updated_at)
		VALUES ('taken', 'taken', '<p>x</p>', 1, 1700000000, 1700000000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO pages (title, slug, content_html, page_order, status, created_at, updated_at)
		VALUES ('taken', 'taken-page', '<p>x</p>', 0, 1, 1700000000, 1700000000)`); err != nil {
		t.Fatal(err)
	}

	bad := map[string]string{
		"no_frontmatter.md":    "just a body, no header\n",
		"empty_frontmatter.md": "---\n---\n\nbody\n",
		"unterminated.md":      "---\ntype: article\ntitle: x\n",
		"unknown_type.md":      "---\ntype: comment\ntitle: x\nslug: x\n---\n\nbody\n",
		"blank_content.md":     "---\ntype: article\ntitle: x\nslug: blank\n---\n\n",
		"reserved_slug.md":     "---\ntype: article\ntitle: x\nslug: admin\n---\n\nbody\n",
		"taken_slug.md":        "---\ntype: article\ntitle: x\nslug: taken\n---\n\nbody\n",
		"page_no_title.md":     "---\ntype: page\nslug: x\n---\n\nbody\n",
		"page_no_slug.md":      "---\ntype: page\ntitle: x\n---\n\nbody\n",
		"page_bad_redirect.md": "---\ntype: page\ntitle: x\nslug: redir\nredirect_url: javascript:alert(1)\n---\n\n",
		"page_taken_slug.md":   "---\ntype: page\ntitle: x\nslug: taken-page\n---\n\n",
		"good.md":              "---\ntype: article\ntitle: ok\nslug: ok\n---\n\nbody\n",
	}
	res, err := (&MarkdownImporter{DB: database}).Import(context.Background(), markdownZip(t, dataDir, bad))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 1 || res.Failed != len(bad)-1 {
		t.Fatalf("result = imported %d failed %d, want 1/%d", res.Imported, res.Failed, len(bad)-1)
	}
	if n := tableCount(t, database, "articles"); n != 2 {
		t.Errorf("articles = %d, want the seeded one plus the good import", n)
	}
	if n := tableCount(t, database, "pages"); n != 1 {
		t.Errorf("pages = %d, want only the seeded one", n)
	}
}

// TestMarkdownImportStatusMapping covers the front-matter status mapping:
// publish and a missing status import published, everything else drafts.
func TestMarkdownImportStatusMapping(t *testing.T) {
	database, dataDir := newTestDB(t)
	doc := func(slug, statusLine string) string {
		return "---\ntype: article\ntitle: " + slug + "\nslug: " + slug + "\n" + statusLine + "---\n\nbody\n"
	}
	res, err := (&MarkdownImporter{DB: database}).Import(context.Background(), markdownZip(t, dataDir, map[string]string{
		"pub.md":      doc("pub", "status: publish\n"),
		"draft.md":    doc("draft", "status: draft\n"),
		"trash.md":    doc("trash", "status: trash\n"),
		"schedule.md": doc("schedule", "status: schedule\n"),
		"missing.md":  doc("missing", ""),
	}))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 5 {
		t.Fatalf("imported = %d, want 5", res.Imported)
	}
	for slug, want := range map[string]int64{"pub": 1, "missing": 1, "draft": 0, "trash": 0, "schedule": 0} {
		var status int64
		if err := database.QueryRow(`SELECT status FROM articles WHERE slug = ?`, slug).Scan(&status); err != nil {
			t.Fatalf("query %s: %v", slug, err)
		}
		if status != want {
			t.Errorf("slug %s status = %d, want %d", slug, status, want)
		}
	}
}

// TestMarkdownImportCRLF imports a document with Windows line endings.
func TestMarkdownImportCRLF(t *testing.T) {
	database, dataDir := newTestDB(t)
	doc := "---\r\ntype: article\r\ntitle: crlf\r\nslug: crlf\r\n---\r\n\r\nbody\r\n"
	res, err := (&MarkdownImporter{DB: database}).Import(context.Background(), markdownZip(t, dataDir, map[string]string{"crlf.md": doc}))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 1 || res.Failed != 0 {
		t.Fatalf("result = imported %d failed %d, want 1/0", res.Imported, res.Failed)
	}
}

// TestMarkdownImportJobEndToEnd drives a markdown import through job_runs:
// the staged upload is imported and removed, and the activity rows mirror the
// other import jobs.
func TestMarkdownImportJobEndToEnd(t *testing.T) {
	database, dataDir := newTestDB(t)
	importsDir := filepath.Join(dataDir, "imports")
	if err := os.MkdirAll(importsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := markdownZip(t, importsDir, map[string]string{"articles/99.md": markdownTestArticle})
	staged := filepath.Join(importsDir, "import_1_test.zip")
	if err := os.Rename(path, staged); err != nil {
		t.Fatal(err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandlers(worker, database, dataDir, nil)
	enqueuer := jobs.NewEnqueuer(database)
	if _, err := enqueuer.Enqueue(context.Background(), jobs.KindImportMarkdown, ImportMarkdownPayload{Path: staged}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if !claimed {
		t.Fatal("no job claimed")
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM job_runs`).Scan(&status); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if status != "done" {
		t.Errorf("job status = %q, want done", status)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged upload still on disk: %v", err)
	}
	if n := tableCount(t, database, "articles"); n != 1 {
		t.Errorf("articles = %d, want 1", n)
	}
	for _, action := range []string{"started", "completed"} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM activity_logs WHERE target = 'import' AND action = ?`, action).Scan(&count); err != nil {
			t.Fatalf("query activity: %v", err)
		}
		if count != 1 {
			t.Errorf("activity %q rows = %d, want 1", action, count)
		}
	}
	var description string
	if err := database.QueryRow(`SELECT description FROM activity_logs WHERE target = 'import' AND action = 'completed'`).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(description, `source="markdown"`) || !strings.Contains(description, "imported_count=1") {
		t.Errorf("completed description = %q, want source=markdown imported_count=1", description)
	}
}
