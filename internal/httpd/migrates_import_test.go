package httpd

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/service/transfer"
	"rables/internal/templates"
)

// newMigratesImportTestServer builds a Server with the migrates index,
// export and import routes.
func newMigratesImportTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	renderer, err := templates.New()
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := NewServer(database, config.Config{Addr: ":8080", DataDir: dataDir, HMACSecret: "x"}, logger, renderer)
	r := chi.NewRouter()
	RegisterMigratesAdminRoutes(r, s)
	RegisterMigratesImportRoutes(r, s)
	return s, r
}

// importUpload is one file field of a multipart form.
type importUpload struct {
	filename    string
	contentType string
	content     []byte
}

// postUpload sends a multipart form with the given file fields to path.
func postUpload(t *testing.T, h http.Handler, path string, fields map[string]importUpload, session *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, f := range fields {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, name, f.filename))
		hdr.Set("Content-Type", f.contentType)
		part, err := mw.CreatePart(hdr)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := part.Write(f.content); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func latestJob(t *testing.T, s *Server) (string, sql.NullString, error) {
	t.Helper()
	var kind string
	var payload sql.NullString
	err := s.DB.QueryRow(`SELECT kind, payload FROM job_runs ORDER BY id DESC LIMIT 1`).Scan(&kind, &payload)
	return kind, payload, err
}

func clearJobs(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.DB.Exec(`DELETE FROM job_runs`); err != nil {
		t.Fatal(err)
	}
}

func TestAdminMigratesImportAuth(t *testing.T) {
	_, h := newMigratesImportTestServer(t)
	for _, path := range []string{"/admin/migrates/import", "/admin/migrates/import_server", "/admin/migrates/import_markdown", "/admin/migrates/import_rss", "/admin/migrates/import_rss_confirm"} {
		rec := doRequest(t, h, http.MethodPost, path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("POST %s unauthenticated: status = %d location = %q, want 302 /session/new",
				path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestAdminMigratesImportFormActions(t *testing.T) {
	s, h := newMigratesImportTestServer(t)
	session := redirectsSessionCookie(t, s)
	rec := doRequest(t, h, http.MethodGet, "/admin/migrates?tab=import", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, want := range []string{`action="/admin/migrates/import"`, `action="/admin/migrates/import_markdown"`, `action="/admin/migrates/import_rss"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("import tab missing %q", want)
		}
	}
}

// TestAdminMigratesImportRSS covers the preview step: the feed is fetched and
// the import tab is re-rendered with the selectable entry list; no job is
// enqueued at this point.
func TestAdminMigratesImportRSS(t *testing.T) {
	s, h := newMigratesImportTestServer(t)
	session := redirectsSessionCookie(t, s)

	t.Run("preview lists the feed entries", func(t *testing.T) {
		clearJobs(t, s)
		s.RSSPreview = func(_ context.Context, feedURL string) ([]transfer.RSSPreviewItem, error) {
			if feedURL != "https://example.com/feed.xml" {
				t.Errorf("preview url = %q, want the submitted url", feedURL)
			}
			return []transfer.RSSPreviewItem{
				{Title: "First Post", Link: "https://blog.example/posts/first-post", Published: 1136214245},
				{Title: "", Link: "https://blog.example/posts/second-post", Published: 0},
			}, nil
		}
		defer func() { s.RSSPreview = nil }()
		rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_rss",
			url.Values{"url": {"https://example.com/feed.xml"}, "import_images": {"1"}}, session)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{
			`action="/admin/migrates/import_rss_confirm"`,
			`<input type="hidden" name="url" value="https://example.com/feed.xml">`,
			`<input type="hidden" name="import_images" value="1">`,
			`value="https://blog.example/posts/first-post"`,
			"First Post",
			"2006-01-02 15:04:05", // published timestamp in the fallback UTC zone
			"https://blog.example/posts/second-post",
			"(untitled)",
			`id="rss-select-all"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("preview missing %q", want)
			}
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected from the preview step, got kind %q", kind)
		}
	})

	t.Run("empty feed renders no selection form", func(t *testing.T) {
		s.RSSPreview = func(_ context.Context, _ string) ([]transfer.RSSPreviewItem, error) {
			return nil, nil
		}
		defer func() { s.RSSPreview = nil }()
		rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_rss",
			url.Values{"url": {"https://example.com/feed.xml"}}, session)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "No importable entries found in this feed.") {
			t.Error("empty feed: missing the no-entries message")
		}
		if strings.Contains(body, `action="/admin/migrates/import_rss_confirm"`) {
			t.Error("empty feed: the confirm form must not render")
		}
	})

	t.Run("fetch error redirects with an alert", func(t *testing.T) {
		s.RSSPreview = func(_ context.Context, _ string) ([]transfer.RSSPreviewItem, error) {
			return nil, errors.New("import rss: fetch: connection refused")
		}
		defer func() { s.RSSPreview = nil }()
		rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_rss",
			url.Values{"url": {"https://example.com/feed.xml"}}, session)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
			t.Fatalf("status = %d location = %q, want 302 /admin/migrates?tab=import", rec.Code, rec.Header().Get("Location"))
		}
	})

	for _, tt := range []struct {
		name string
		form url.Values
	}{
		{"blank url", url.Values{"url": {"  "}}},
		{"missing url", url.Values{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearJobs(t, s)
			rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_rss", tt.form, session)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
				t.Fatalf("status = %d location = %q, want 302 /admin/migrates?tab=import", rec.Code, rec.Header().Get("Location"))
			}
			if kind, _, err := latestJob(t, s); err == nil {
				t.Errorf("no job expected, got kind %q", kind)
			}
		})
	}
}

// TestAdminMigratesImportRSSConfirm covers the selection submit: the
// import_rss job is enqueued with exactly the selected entry links.
func TestAdminMigratesImportRSSConfirm(t *testing.T) {
	s, h := newMigratesImportTestServer(t)
	session := redirectsSessionCookie(t, s)

	t.Run("selected links enqueued", func(t *testing.T) {
		clearJobs(t, s)
		rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_rss_confirm", url.Values{
			"url":           {"https://example.com/feed.xml"},
			"import_images": {"1"},
			"links":         {"https://blog.example/posts/first-post", "https://blog.example/posts/second-post"},
		}, session)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
			t.Fatalf("status = %d location = %q, want 302 /admin/migrates?tab=import", rec.Code, rec.Header().Get("Location"))
		}
		kind, payload, err := latestJob(t, s)
		if err != nil {
			t.Fatalf("expected queued job: %v", err)
		}
		if kind != "import_rss" {
			t.Errorf("kind = %q, want import_rss", kind)
		}
		var p transfer.ImportRSSPayload
		if err := json.Unmarshal([]byte(payload.String), &p); err != nil {
			t.Fatalf("payload not JSON: %v", err)
		}
		if p.URL != "https://example.com/feed.xml" || !p.ImportImages {
			t.Errorf("payload = %+v, want url set and import_images=true", p)
		}
		wantLinks := []string{"https://blog.example/posts/first-post", "https://blog.example/posts/second-post"}
		if !slices.Equal(p.Links, wantLinks) {
			t.Errorf("links = %v, want %v", p.Links, wantLinks)
		}
	})

	for _, tt := range []struct {
		name string
		form url.Values
	}{
		{"no selection", url.Values{"url": {"https://example.com/feed.xml"}}},
		{"blank url", url.Values{"url": {" "}, "links": {"https://blog.example/posts/first-post"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearJobs(t, s)
			rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_rss_confirm", tt.form, session)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
				t.Fatalf("status = %d location = %q, want 302 /admin/migrates?tab=import", rec.Code, rec.Header().Get("Location"))
			}
			if kind, _, err := latestJob(t, s); err == nil {
				t.Errorf("no job expected, got kind %q", kind)
			}
		})
	}
}

func TestAdminMigratesImportDB(t *testing.T) {
	s, h := newMigratesImportTestServer(t)
	session := redirectsSessionCookie(t, s)
	zipBytes := []byte("PK\x03\x04fake-zip")

	t.Run("valid zip enqueued", func(t *testing.T) {
		clearJobs(t, s)
		rec := postUpload(t, h, "/admin/migrates/import", map[string]importUpload{
			"backup_file": {"backup.zip", "application/zip", zipBytes},
		}, session)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
			t.Fatalf("status = %d location = %q, want 302 tab=import", rec.Code, rec.Header().Get("Location"))
		}
		kind, payload, err := latestJob(t, s)
		if err != nil {
			t.Fatalf("expected queued job: %v", err)
		}
		if kind != "import_db" {
			t.Errorf("kind = %q, want import_db", kind)
		}
		var p transfer.ImportDBPayload
		if err := json.Unmarshal([]byte(payload.String), &p); err != nil {
			t.Fatalf("payload not JSON: %v", err)
		}
		// The upload is stored under data/imports with a generated name.
		base := filepath.Base(p.Path)
		if filepath.Dir(p.Path) != filepath.Join(s.Cfg.DataDir, "imports") ||
			!strings.HasPrefix(base, "import_") || !strings.HasSuffix(base, ".zip") {
			t.Errorf("stored path = %q, want data/imports/import_*.zip", p.Path)
		}
		stored, err := os.ReadFile(p.Path)
		if err != nil {
			t.Fatalf("stored upload: %v", err)
		}
		if !bytes.Equal(stored, zipBytes) {
			t.Errorf("stored content = %q, want the uploaded bytes", stored)
		}
	})

	t.Run("bare database accepted", func(t *testing.T) {
		clearJobs(t, s)
		rec := postUpload(t, h, "/admin/migrates/import", map[string]importUpload{
			"backup_file": {"rables.sqlite3", "application/octet-stream", []byte("sqlite")},
		}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		kind, payload, err := latestJob(t, s)
		if err != nil {
			t.Fatalf("expected queued job: %v", err)
		}
		if kind != "import_db" {
			t.Errorf("kind = %q, want import_db", kind)
		}
		var p transfer.ImportDBPayload
		if err := json.Unmarshal([]byte(payload.String), &p); err != nil {
			t.Fatalf("payload not JSON: %v", err)
		}
		if !strings.HasSuffix(p.Path, ".sqlite3") {
			t.Errorf("stored path = %q, want .sqlite3 suffix", p.Path)
		}
	})

	t.Run("uppercase .ZIP extension accepted", func(t *testing.T) {
		clearJobs(t, s)
		rec := postUpload(t, h, "/admin/migrates/import", map[string]importUpload{
			"backup_file": {"backup.ZIP", "application/x-zip-compressed", zipBytes},
		}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if _, _, err := latestJob(t, s); err != nil {
			t.Errorf("expected queued job: %v", err)
		}
	})

	t.Run("wrong type rejected", func(t *testing.T) {
		clearJobs(t, s)
		importsDir := filepath.Join(s.Cfg.DataDir, "imports")
		before, _ := os.ReadDir(importsDir)
		rec := postUpload(t, h, "/admin/migrates/import", map[string]importUpload{
			"backup_file": {"notes.txt", "text/plain", []byte("hello")},
		}, session)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
			t.Fatalf("status = %d, want 302 tab=import", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
		// The rejected upload must not stay behind on disk.
		after, _ := os.ReadDir(importsDir)
		if len(after) != len(before) {
			t.Errorf("rejected upload left a file behind: before %d, after %d", len(before), len(after))
		}
	})

	t.Run("missing file field", func(t *testing.T) {
		clearJobs(t, s)
		rec := postUpload(t, h, "/admin/migrates/import", map[string]importUpload{}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
	})
}

func TestAdminMigratesImportMarkdown(t *testing.T) {
	s, h := newMigratesImportTestServer(t)
	session := redirectsSessionCookie(t, s)
	article := []byte("---\ntype: article\ntitle: x\nslug: x\n---\n\nbody\n")

	// postMarkdownFiles uploads names under the shared markdown_files field.
	postMarkdownFiles := func(t *testing.T, names []string) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, name := range names {
			hdr := textproto.MIMEHeader{}
			hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="markdown_files"; filename="%s"`, name))
			hdr.Set("Content-Type", "text/markdown")
			part, err := mw.CreatePart(hdr)
			if err != nil {
				t.Fatalf("create part: %v", err)
			}
			if _, err := part.Write(article); err != nil {
				t.Fatalf("write part: %v", err)
			}
		}
		if err := mw.Close(); err != nil {
			t.Fatalf("close multipart: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/admin/migrates/import_markdown", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.AddCookie(session)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	latestMarkdownPath := func(t *testing.T) string {
		t.Helper()
		kind, payload, err := latestJob(t, s)
		if err != nil {
			t.Fatalf("expected queued job: %v", err)
		}
		if kind != "import_markdown" {
			t.Fatalf("kind = %q, want import_markdown", kind)
		}
		var p transfer.ImportMarkdownPayload
		if err := json.Unmarshal([]byte(payload.String), &p); err != nil {
			t.Fatalf("payload not JSON: %v", err)
		}
		return p.Path
	}

	t.Run("multiple markdown files packed into one staged zip", func(t *testing.T) {
		clearJobs(t, s)
		rec := postMarkdownFiles(t, []string{"1.md", "2.md"})
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
			t.Fatalf("status = %d location = %q, want 302 tab=import", rec.Code, rec.Header().Get("Location"))
		}
		path := latestMarkdownPath(t)
		base := filepath.Base(path)
		if filepath.Dir(path) != filepath.Join(s.Cfg.DataDir, "imports") ||
			!strings.HasPrefix(base, "import_") || !strings.HasSuffix(base, ".zip") {
			t.Errorf("stored path = %q, want data/imports/import_*.zip", path)
		}
		zr, err := zip.OpenReader(path)
		if err != nil {
			t.Fatalf("staged upload is not a zip: %v", err)
		}
		defer zr.Close()
		if len(zr.File) != 2 {
			t.Errorf("staged zip entries = %d, want 2", len(zr.File))
		}
	})

	t.Run("single zip passes through", func(t *testing.T) {
		clearJobs(t, s)
		rec := postMarkdownFiles(t, []string{"export.zip"})
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		path := latestMarkdownPath(t)
		stored, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("stored upload: %v", err)
		}
		if !bytes.Equal(stored, article) {
			t.Errorf("stored content = %q, want the uploaded bytes", stored)
		}
	})

	t.Run("zip among multiple files rejected", func(t *testing.T) {
		clearJobs(t, s)
		rec := postMarkdownFiles(t, []string{"export.zip", "1.md"})
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
	})

	t.Run("wrong type rejected", func(t *testing.T) {
		clearJobs(t, s)
		importsDir := filepath.Join(s.Cfg.DataDir, "imports")
		before, _ := os.ReadDir(importsDir)
		rec := postMarkdownFiles(t, []string{"notes.txt"})
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
		after, _ := os.ReadDir(importsDir)
		if len(after) != len(before) {
			t.Errorf("rejected upload left a file behind: before %d, after %d", len(before), len(after))
		}
	})

	t.Run("missing files", func(t *testing.T) {
		clearJobs(t, s)
		rec := postUpload(t, h, "/admin/migrates/import_markdown", map[string]importUpload{}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
	})
}

func TestAdminMigratesImportServerFile(t *testing.T) {
	s, h := newMigratesImportTestServer(t)
	session := redirectsSessionCookie(t, s)
	importsDir := filepath.Join(s.Cfg.DataDir, "imports")
	if err := os.MkdirAll(importsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	zipBytes := []byte("PK\x03\x04fake-zip")
	if err := os.WriteFile(filepath.Join(importsDir, "export_2026.zip"), zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	// A web upload already owned by an enqueued job must not be listed.
	if err := os.WriteFile(filepath.Join(importsDir, "import_1700000000_a1b2c3d4.zip"), zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("listed on the import tab", func(t *testing.T) {
		rec := doRequest(t, h, http.MethodGet, "/admin/migrates?tab=import", nil, session)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		for _, want := range []string{`action="/admin/migrates/import_server"`, "export_2026.zip"} {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("import tab missing %q", want)
			}
		}
		if strings.Contains(rec.Body.String(), "import_1700000000_a1b2c3d4.zip") {
			t.Error("import tab listed a pending upload owned by a job")
		}
	})

	t.Run("existing file enqueued and renamed out of the list", func(t *testing.T) {
		clearJobs(t, s)
		rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_server", url.Values{"filename": {"export_2026.zip"}}, session)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
			t.Fatalf("status = %d location = %q, want 302 tab=import", rec.Code, rec.Header().Get("Location"))
		}
		kind, payload, err := latestJob(t, s)
		if err != nil {
			t.Fatalf("expected queued job: %v", err)
		}
		if kind != "import_db" {
			t.Errorf("kind = %q, want import_db", kind)
		}
		var p transfer.ImportDBPayload
		if err := json.Unmarshal([]byte(payload.String), &p); err != nil {
			t.Fatalf("payload not JSON: %v", err)
		}
		queuedPath := filepath.Join(importsDir, "export_2026.zip.queued")
		if p.Path != queuedPath {
			t.Errorf("payload path = %q, want the .queued file in data/imports", p.Path)
		}
		if !p.KeepOnFailure {
			t.Error("KeepOnFailure = false, want true so a failed import leaves the server file for a retry")
		}
		// The rename moves the file aside so it cannot be enqueued twice.
		if _, err := os.Stat(filepath.Join(importsDir, "export_2026.zip")); !os.IsNotExist(err) {
			t.Errorf("original file still present after enqueue: %v", err)
		}
		stored, err := os.ReadFile(p.Path)
		if err != nil {
			t.Fatalf("queued file missing after enqueue: %v", err)
		}
		if !bytes.Equal(stored, zipBytes) {
			t.Errorf("stored content = %q, want the original bytes", stored)
		}
		// The queued file drops out of the import tab list.
		rec = doRequest(t, h, http.MethodGet, "/admin/migrates?tab=import", nil, session)
		if strings.Contains(rec.Body.String(), "export_2026.zip") {
			t.Error("import tab still listed the enqueued file")
		}
		// Re-submitting the original name now reports the file as missing.
		clearJobs(t, s)
		rec = doRequest(t, h, http.MethodPost, "/admin/migrates/import_server", url.Values{"filename": {"export_2026.zip"}}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("resubmit: status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected for a resubmitted file, got kind %q", kind)
		}
		// And the .queued name itself is rejected as job-owned.
		rec = doRequest(t, h, http.MethodPost, "/admin/migrates/import_server", url.Values{"filename": {"export_2026.zip.queued"}}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("queued name: status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected for a .queued name, got kind %q", kind)
		}
	})

	t.Run("existing .queued rejected without overwrite", func(t *testing.T) {
		clearJobs(t, s)
		// A leftover .queued is either a failed import kept for a retry or a
		// file still owned by a pending job; the rename must not clobber it.
		path := filepath.Join(importsDir, "export_2027.zip")
		queuedPath := path + ".queued"
		if err := os.WriteFile(path, zipBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		queuedBytes := []byte("PK\x03\x04previous-run")
		if err := os.WriteFile(queuedPath, queuedBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_server", url.Values{"filename": {"export_2027.zip"}}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
		// Both files stay untouched: the backup keeps its bytes and the
		// original stays listed for another attempt.
		stored, err := os.ReadFile(queuedPath)
		if err != nil || !bytes.Equal(stored, queuedBytes) {
			t.Errorf("queued file = %q, %v; want the untouched backup bytes", stored, err)
		}
		stored, err = os.ReadFile(path)
		if err != nil || !bytes.Equal(stored, zipBytes) {
			t.Errorf("original file = %q, %v; want the untouched original bytes", stored, err)
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		clearJobs(t, s)
		rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_server", url.Values{"filename": {"../rables.db"}}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
	})

	t.Run("wrong type rejected", func(t *testing.T) {
		clearJobs(t, s)
		if err := os.WriteFile(filepath.Join(importsDir, "notes.txt"), []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_server", url.Values{"filename": {"notes.txt"}}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
	})

	t.Run("missing file rejected", func(t *testing.T) {
		clearJobs(t, s)
		for _, form := range []url.Values{{"filename": {"nope.zip"}}, {}} {
			rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_server", form, session)
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", rec.Code)
			}
			if kind, _, err := latestJob(t, s); err == nil {
				t.Errorf("no job expected, got kind %q", kind)
			}
		}
	})

	t.Run("job-owned upload rejected", func(t *testing.T) {
		clearJobs(t, s)
		// The file exists but belongs to an enqueued job (a web upload);
		// importing it again would race the owner.
		rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_server", url.Values{"filename": {"import_1700000000_a1b2c3d4.zip"}}, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
	})
}

// peekFile is a multipart.File that runs see once on the first Read, then
// serves content, so a test can observe data/imports mid-copy.
type peekFile struct {
	r    *strings.Reader
	see  func()
	seen bool
}

func (f *peekFile) Read(p []byte) (int, error) {
	if !f.seen {
		f.seen = true
		f.see()
	}
	return f.r.Read(p)
}

func (f *peekFile) ReadAt(p []byte, off int64) (int, error) { return f.r.ReadAt(p, off) }
func (f *peekFile) Seek(off int64, whence int) (int64, error) {
	return f.r.Seek(off, whence)
}
func (f *peekFile) Close() error { return nil }

// errFile is a multipart.File whose Read always fails.
type errFile struct{}

func (errFile) Read([]byte) (int, error)          { return 0, errors.New("read boom") }
func (errFile) ReadAt([]byte, int64) (int, error) { return 0, errors.New("read boom") }
func (errFile) Seek(int64, int) (int64, error)    { return 0, nil }
func (errFile) Close() error                      { return nil }

func TestSaveImportUploadTempName(t *testing.T) {
	s, _ := newMigratesImportTestServer(t)
	importsDir := filepath.Join(s.Cfg.DataDir, "imports")
	content := "upload-bytes"

	t.Run("final name only appears fully written", func(t *testing.T) {
		// Mid-copy only the .part temp name may exist in data/imports: the
		// startup sweep (jobs.CleanupOrphanImportFiles) and the import tab
		// must never see a half-written import_* file.
		f := &peekFile{r: strings.NewReader(content), see: func() {
			entries, err := os.ReadDir(importsDir)
			if err != nil {
				t.Errorf("read imports dir mid-write: %v", err)
				return
			}
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".part") {
					t.Errorf("mid-write entry %q does not carry the .part temp suffix", e.Name())
				}
			}
		}}
		path, err := s.saveImportUpload(f, ".zip")
		if err != nil {
			t.Fatalf("saveImportUpload: %v", err)
		}
		if !f.seen {
			t.Fatal("mid-write check never ran")
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "import_") || !strings.HasSuffix(base, ".zip") {
			t.Errorf("final path = %q, want import_*.zip", base)
		}
		stored, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read stored upload: %v", err)
		}
		if string(stored) != content {
			t.Errorf("stored content = %q, want %q", stored, content)
		}
		// The rename leaves no temp file behind.
		entries, err := os.ReadDir(importsDir)
		if err != nil {
			t.Fatalf("read imports dir: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != base {
			t.Errorf("imports dir entries = %v, want only %s", entries, base)
		}
	})

	t.Run("failed copy removes the temp file", func(t *testing.T) {
		if _, err := s.saveImportUpload(errFile{}, ".zip"); err == nil {
			t.Fatal("saveImportUpload: want an error")
		}
		entries, err := os.ReadDir(importsDir)
		if err != nil {
			t.Fatalf("read imports dir: %v", err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".part") {
				t.Errorf("temp file %s left behind after a failed copy", e.Name())
			}
		}
	})
}
