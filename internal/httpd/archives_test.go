package httpd

import (
	"archive/zip"
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/templates"
)

// newArchiveTestServer builds a Server with a real DataDir (archives live on
// disk) and completes the setup flow so an admin can log in.
func newArchiveTestServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	dataDir := t.TempDir()
	database, err := db.Open(t.TempDir())
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
	h := NewRouter(s)
	completeSetup(t, h)
	return s, h, dataDir
}

// buildArchiveZip packs name->body entries into an in-memory ZIP.
func buildArchiveZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// postMultipart serves one multipart POST through the handler.
func postMultipart(t *testing.T, h http.Handler, target string, fields map[string]string, fileField, fileName string, fileBody []byte, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %q: %v", k, err)
		}
	}
	if fileField != "" {
		fw, err := mw.CreateFormFile(fileField, fileName)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(fileBody); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sampleArchiveFields() map[string]string {
	return map[string]string{
		"title":        "Demo Site",
		"slug":         "demo-site",
		"content_type": "html_archive",
		"status":       "publish",
		"tag_list":     "blog",
	}
}

func sampleArchiveZip(t *testing.T) []byte {
	t.Helper()
	return buildArchiveZip(t, map[string]string{
		"index.html":     "<html><body><h1>demo</h1><script>console.log(1)</script></body></html>",
		"css/style.css":  "body { color: red }",
		"js/app.js":      "console.log('app')",
		"img/pic.svg":    "<svg xmlns='http://www.w3.org/2000/svg'></svg>",
		"data/blob.bin":  "\x00\x01\x02",
		"sub/index.html": "<html><body>sub</body></html>",
	})
}

// createArchiveArticle posts the archive form; the fresh database gives the
// first article id 1.
func createArchiveArticle(t *testing.T, h http.Handler, cookie *http.Cookie, fields map[string]string, zipBody []byte) {
	t.Helper()
	rec := postMultipart(t, h, "/admin/posts", fields, "archive_file", "site.zip", zipBody, cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("create archive article: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestArchiveArticleLifecycle(t *testing.T) {
	_, h, dataDir := newArchiveTestServer(t)
	cookie := login(t, h, "Admin", "secret-pw")
	createArchiveArticle(t, h, cookie, sampleArchiveFields(), sampleArchiveZip(t))

	// Extracted on disk.
	for _, rel := range []string{"index.html", "css/style.css", "js/app.js"} {
		if _, err := os.Stat(filepath.Join(dataDir, "archives", "article", "1", filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing extracted file %s: %v", rel, err)
		}
	}

	// The public page embeds the sandboxed iframe, not an empty content div.
	rec := doRequest(t, h, http.MethodGet, "/demo-site", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public article: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<iframe class="archive-frame" src="/archives/article/1/"`) {
		t.Error("public article does not embed the archive iframe")
	}
	if !strings.Contains(body, `sandbox="allow-scripts allow-popups"`) {
		t.Error("iframe is missing the sandbox attribute")
	}
	if strings.Contains(body, `class="article-content"`) {
		t.Error("archive article still renders the content div")
	}

	// index.html at the tree root, with the sandbox CSP on the response.
	rec = doRequest(t, h, http.MethodGet, "/archives/article/1/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive root: status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("archive root Content-Type = %q", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox allow-scripts allow-popups" {
		t.Errorf("archive root CSP = %q", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("archive root missing nosniff")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, no-cache" {
		t.Errorf("archive root Cache-Control = %q, want public, no-cache", cc)
	}
	if !strings.Contains(rec.Body.String(), "<h1>demo</h1>") {
		t.Error("archive root did not serve index.html")
	}

	// Assets resolve relative to the tree; a subdirectory serves its own
	// index.html; unknown types download as octet-stream. The CSP rides every
	// response, so a directly-opened SVG stays inert too.
	cases := []struct {
		path, wantType string
	}{
		{"/archives/article/1/css/style.css", "text/css; charset=utf-8"},
		{"/archives/article/1/js/app.js", "text/javascript; charset=utf-8"},
		{"/archives/article/1/img/pic.svg", "image/svg+xml"},
		{"/archives/article/1/data/blob.bin", "application/octet-stream"},
		{"/archives/article/1/sub/", "text/html; charset=utf-8"},
		{"/archives/article/1/index.html", "text/html; charset=utf-8"},
	}
	for _, tc := range cases {
		rec = doRequest(t, h, http.MethodGet, tc.path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", tc.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != tc.wantType {
			t.Errorf("GET %s: Content-Type = %q, want %q", tc.path, ct, tc.wantType)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox allow-scripts allow-popups" {
			t.Errorf("GET %s: CSP = %q", tc.path, csp)
		}
	}

	// The bare record URL redirects to its trailing-slash form.
	rec = doRequest(t, h, http.MethodGet, "/archives/article/1", nil)
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/archives/article/1/" {
		t.Errorf("bare archive URL: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}

	// Traversal attempts are 404, never a neighboring file.
	for _, evil := range []string{
		"/archives/article/1/../index.html",
		"/archives/article/1/css/../../page/1/index.html",
		"/archives/article/1/%2e%2e%2f%2e%2e%2frables.db",
		"/archives/article/1/css/..%5c..%5csecret",
	} {
		rec = doRequest(t, h, http.MethodGet, evil, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", evil, rec.Code)
		}
	}

	// Missing files and foreign records 404.
	for _, missing := range []string{
		"/archives/article/1/nope.css",
		"/archives/article/999/index.html",
		"/archives/files/1/index.html",
	} {
		rec = doRequest(t, h, http.MethodGet, missing, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", missing, rec.Code)
		}
	}
}

func TestArchiveCreateValidation(t *testing.T) {
	s, h, dataDir := newArchiveTestServer(t)
	cookie := login(t, h, "Admin", "secret-pw")

	// No ZIP at all.
	rec := postMultipart(t, h, "/admin/posts", sampleArchiveFields(), "archive_file", "", nil, cookie)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Archive file can&#39;t be blank") {
		t.Errorf("no zip: status = %d, want 422 with blank-archive error", rec.Code)
	}

	// ZIP without a root index.html.
	bad := buildArchiveZip(t, map[string]string{"other.html": "x"})
	rec = postMultipart(t, h, "/admin/posts", sampleArchiveFields(), "archive_file", "site.zip", bad, cookie)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "must contain an index.html") {
		t.Errorf("no index.html: status = %d, want 422 with index error", rec.Code)
	}

	// ZIP with a traversal entry.
	evil := buildArchiveZip(t, map[string]string{"index.html": "x", "../evil": "x"})
	rec = postMultipart(t, h, "/admin/posts", sampleArchiveFields(), "archive_file", "site.zip", evil, cookie)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "unsafe path") {
		t.Errorf("traversal: status = %d, want 422 with unsafe-path error", rec.Code)
	}

	// Not a ZIP.
	rec = postMultipart(t, h, "/admin/posts", sampleArchiveFields(), "archive_file", "site.zip", []byte("plain text"), cookie)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not a valid ZIP") {
		t.Errorf("not a zip: status = %d, want 422 with invalid-zip error", rec.Code)
	}

	// Nothing may have been created or extracted.
	var n int64
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM articles").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("articles = %d, want 0 after failed uploads", n)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dataDir, "archives", "*", "*"))
	if len(leftovers) > 0 {
		t.Errorf("archive files left behind: %v", leftovers)
	}
}

func TestArchiveDraftVisibility(t *testing.T) {
	_, h, _ := newArchiveTestServer(t)
	cookie := login(t, h, "Admin", "secret-pw")

	fields := sampleArchiveFields()
	fields["status"] = "draft"
	createArchiveArticle(t, h, cookie, fields, sampleArchiveZip(t))

	// Anonymous visitors get the 404 for both the page and the archive tree;
	// the authenticated admin can preview both.
	rec := doRequest(t, h, http.MethodGet, "/demo-site", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("draft page anonymous: status = %d, want 404", rec.Code)
	}
	rec = doRequest(t, h, http.MethodGet, "/archives/article/1/", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("draft archive anonymous: status = %d, want 404", rec.Code)
	}
	rec = doRequest(t, h, http.MethodGet, "/archives/article/1/", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Errorf("draft archive admin: status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("draft archive Cache-Control = %q, want private", cc)
	}
}

func TestArchiveReplaceAndTypeSwitch(t *testing.T) {
	_, h, dataDir := newArchiveTestServer(t)
	cookie := login(t, h, "Admin", "secret-pw")
	createArchiveArticle(t, h, cookie, sampleArchiveFields(), sampleArchiveZip(t))

	// Update keeping the type without a new ZIP: the tree is kept.
	rec := postMultipart(t, h, "/admin/posts/demo-site", sampleArchiveFields(), "archive_file", "", nil, cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("update without zip: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodGet, "/archives/article/1/", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<h1>demo</h1>") {
		t.Errorf("archive after keep-update: status = %d", rec.Code)
	}

	// Update with a new ZIP replaces the tree.
	fields := sampleArchiveFields()
	fields["title"] = "Demo Site v2"
	rec = postMultipart(t, h, "/admin/posts/demo-site", fields, "archive_file", "v2.zip",
		buildArchiveZip(t, map[string]string{"index.html": "<html><body>v2</body></html>"}), cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("update with zip: status = %d", rec.Code)
	}
	rec = doRequest(t, h, http.MethodGet, "/archives/article/1/", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "v2") {
		t.Errorf("archive after replace: status = %d body missing v2", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "archives", "article", "1", "css", "style.css")); !os.IsNotExist(err) {
		t.Error("old archive file survived the replacement")
	}

	// Switching the content type back to markdown removes the tree.
	rec = postMultipart(t, h, "/admin/posts/demo-site", map[string]string{
		"title":            "Demo Site",
		"slug":             "demo-site",
		"content_type":     "markdown",
		"markdown_content": "# Back to markdown",
		"status":           "publish",
		"tag_list":         "blog",
	}, "archive_file", "", nil, cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("switch to markdown: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "archives", "article", "1")); !os.IsNotExist(err) {
		t.Error("archive tree survived the content-type switch")
	}
	// The route 404s once the record is no longer an archive.
	rec = doRequest(t, h, http.MethodGet, "/archives/article/1/", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("archive after type switch: status = %d, want 404", rec.Code)
	}
}

// A ZIP made by compressing a folder (one shared top-level directory) is
// unwrapped at upload: index.html lands at the tree root and serves.
func TestArchiveWrappedZipUpload(t *testing.T) {
	_, h, _ := newArchiveTestServer(t)
	cookie := login(t, h, "Admin", "secret-pw")

	createArchiveArticle(t, h, cookie, sampleArchiveFields(), buildArchiveZip(t, map[string]string{
		"mysite/index.html":            "<html><body>wrapped</body></html>",
		"mysite/css/style.css":         "body{}",
		"__MACOSX/mysite/._index.html": "junk",
	}))

	rec := doRequest(t, h, http.MethodGet, "/archives/article/1/", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "wrapped") {
		t.Errorf("wrapped archive root: status = %d", rec.Code)
	}
	rec = doRequest(t, h, http.MethodGet, "/archives/article/1/css/style.css", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("wrapped archive asset: status = %d", rec.Code)
	}
}

func TestArchiveDestroyCleanup(t *testing.T) {
	_, h, dataDir := newArchiveTestServer(t)
	cookie := login(t, h, "Admin", "secret-pw")
	createArchiveArticle(t, h, cookie, sampleArchiveFields(), sampleArchiveZip(t))

	// Two-stage destroy: trash, then real delete.
	rec := doRequest(t, h, http.MethodPost, "/admin/posts/demo-site/destroy", nil, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("trash: status = %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "archives", "article", "1")); err != nil {
		t.Error("archive tree removed too early (on trash)")
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/posts/demo-site/destroy", nil, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("destroy: status = %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "archives", "article", "1")); !os.IsNotExist(err) {
		t.Error("archive tree survived the real delete")
	}
}

func TestArchivePageFlow(t *testing.T) {
	_, h, dataDir := newArchiveTestServer(t)
	cookie := login(t, h, "Admin", "secret-pw")

	rec := postMultipart(t, h, "/admin/pages", map[string]string{
		"title":        "Archive Page",
		"slug":         "archive-page",
		"content_type": "html_archive",
		"status":       "publish",
		"page_order":   "0",
	}, "archive_file", "site.zip", sampleArchiveZip(t), cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("create archive page: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "archives", "page", "1", "index.html")); err != nil {
		t.Fatalf("page archive not extracted: %v", err)
	}

	rec = doRequest(t, h, http.MethodGet, "/pages/archive-page", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `src="/archives/page/1/"`) {
		t.Fatalf("public page: status = %d, iframe missing", rec.Code)
	}
	rec = doRequest(t, h, http.MethodGet, "/archives/page/1/css/style.css", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Errorf("page archive asset: status = %d", rec.Code)
	}

	// Page destroy (batch does a real delete for a trashed page; here the
	// member destroy twice) removes the tree.
	doRequest(t, h, http.MethodPost, "/admin/pages/archive-page/destroy", nil, cookie)
	rec = doRequest(t, h, http.MethodPost, "/admin/pages/archive-page/destroy", nil, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("page destroy: status = %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "archives", "page", "1")); !os.IsNotExist(err) {
		t.Error("page archive tree survived the delete")
	}
}
