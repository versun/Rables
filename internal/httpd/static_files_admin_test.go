package httpd

import (
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/templates"
)

// newStaticFilesTestServer builds a Server with the static-file routes (admin
// plus public) and the media route that serves the resolved blob.
func newStaticFilesTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterStaticFilesRoutes(r, s)
	RegisterMediaRoutes(r, s)
	return s, r
}

// uploadStaticFile posts the multipart upload form. An empty filename sends
// no file field at all.
func uploadStaticFile(t *testing.T, h http.Handler, session *http.Cookie, filename, content, description string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if description != "" {
		if err := mw.WriteField("description", description); err != nil {
			t.Fatalf("write description: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/static_files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if session != nil {
		req.AddCookie(session)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAdminStaticFilesAuth: the admin routes sit behind RequireAuth; the
// public /static/* route does not.
func TestAdminStaticFilesAuth(t *testing.T) {
	_, h := newStaticFilesTestServer(t)
	for _, tt := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/static_files"},
		{http.MethodPost, "/admin/static_files"},
		{http.MethodPost, "/admin/static_files/1/destroy"},
	} {
		rec := doRequest(t, h, tt.method, tt.path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("%s %s unauthenticated: status = %d location = %q, want 302 /session/new",
				tt.method, tt.path, rec.Code, rec.Header().Get("Location"))
		}
	}
	// Public route: no session needed (404 for the unknown filename).
	rec := doRequest(t, h, http.MethodGet, "/static/nope.txt", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /static/nope.txt: status = %d, want 404", rec.Code)
	}
}

// TestStaticFilesUploadDownloadDelete walks Admin::StaticFilesController and
// the public StaticFilesController#show.
func TestStaticFilesUploadDownloadDelete(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	// Upload.
	rec := uploadStaticFile(t, h, session, "hello.txt", "hello world", "greeting")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/static_files" {
		t.Fatalf("upload: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	staticFile, err := s.Q.GetStaticFileByFilename(ctx, "hello.txt")
	if err != nil {
		t.Fatalf("get static file: %v", err)
	}
	if staticFile.Description.String != "greeting" {
		t.Errorf("description = %q, want greeting", staticFile.Description.String)
	}

	// Index lists it.
	rec = doRequest(t, h, http.MethodGet, "/admin/static_files", nil, session)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "hello.txt") || !strings.Contains(body, "/static/hello.txt") {
		t.Errorf("index does not list the upload: status = %d", rec.Code)
	}

	// Public route redirects to the blob URL, which serves the content.
	rec = doRequest(t, h, http.MethodGet, "/static/hello.txt", nil)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/files/") {
		t.Fatalf("show: status = %d location = %q, want 302 /files/...", rec.Code, rec.Header().Get("Location"))
	}
	rec = doRequest(t, h, http.MethodGet, rec.Header().Get("Location"), nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello world" {
		t.Errorf("blob serve: status = %d body = %q", rec.Code, rec.Body.String())
	}

	// Overwrite keeps one row and replaces the content.
	rec = uploadStaticFile(t, h, session, "hello.txt", "hello v2", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("overwrite: status = %d", rec.Code)
	}
	if flash := findCookie(rec, flashCookieName); flash == nil {
		t.Error("overwrite sets no flash cookie")
	}
	rows, err := s.Q.ListStaticFiles(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("after overwrite: rows = %d err = %v, want exactly 1", len(rows), err)
	}
	rec = doRequest(t, h, http.MethodGet, "/static/hello.txt", nil)
	rec = doRequest(t, h, http.MethodGet, rec.Header().Get("Location"), nil)
	if rec.Body.String() != "hello v2" {
		t.Errorf("overwritten content = %q, want hello v2", rec.Body.String())
	}

	// Delete removes the row and the blob.
	rec = doRequest(t, h, http.MethodPost, "/admin/static_files/"+itoa(staticFile.ID)+"/destroy", nil, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/static_files" {
		t.Fatalf("destroy: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := s.Q.GetStaticFileByFilename(ctx, "hello.txt"); err == nil {
		t.Error("static file row still present after destroy")
	}
	rec = doRequest(t, h, http.MethodGet, "/static/hello.txt", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("show after destroy: status = %d, want 404", rec.Code)
	}
}

// TestStaticFilesUploadValidation mirrors the Rails failure branches.
func TestStaticFilesUploadValidation(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	// No file field: index re-renders with the alert.
	rec := uploadStaticFile(t, h, session, "", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "请选择要上传的文件") {
		t.Errorf("missing file: status = %d, want 200 with the choose-file alert", rec.Code)
	}

	// Invalid filenames never reach the handler intact (mime/multipart's
	// Part.FileName applies filepath.Base, and control chars break the header
	// parser), so the validation mapping is covered by TestStaticFilenameError.

	// Nothing was stored.
	if rows, err := s.Q.ListStaticFiles(ctx); err != nil || len(rows) != 0 {
		t.Errorf("rows = %d err = %v, want no stored files", len(rows), err)
	}
}

// TestStaticFilenameError covers the StaticFile filename validations.
func TestStaticFilenameError(t *testing.T) {
	tests := []struct {
		filename string
		want     string
	}{
		{"ok.txt", ""},
		{"a b.txt", ""},
		{"", "Filename can't be blank"},
		{"  ", "Filename can't be blank"},
		{"a/b.txt", "Filename must not contain slashes or control characters"},
		{"a\x00b", "Filename must not contain slashes or control characters"},
		{"a\x1fb", "Filename must not contain slashes or control characters"},
		{"a\x7fb", "Filename must not contain slashes or control characters"},
	}
	for _, tt := range tests {
		if got := staticFilenameError(tt.filename); got != tt.want {
			t.Errorf("staticFilenameError(%q) = %q, want %q", tt.filename, got, tt.want)
		}
	}
}

// TestStaticFilesPurgeOnDelete: destroy removes the files row and the blob
// from disk, like ActiveStorage's dependent purge.
func TestStaticFilesPurgeOnDelete(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	rec := uploadStaticFile(t, h, session, "bye.txt", "bye", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("upload: status = %d", rec.Code)
	}
	staticFile, err := s.Q.GetStaticFileByFilename(ctx, "bye.txt")
	if err != nil {
		t.Fatalf("get static file: %v", err)
	}
	file, err := s.Q.GetFileForStaticFilename(ctx, "bye.txt")
	if err != nil {
		t.Fatalf("get file row: %v", err)
	}
	blobPath := s.Media().PathFor(file.Key)
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("blob missing on disk: %v", err)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/static_files/"+itoa(staticFile.ID)+"/destroy", nil, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("destroy: status = %d", rec.Code)
	}
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Errorf("blob still on disk after destroy: stat err = %v", err)
	}
	if _, err := s.Q.GetFileByKey(ctx, file.Key); err == nil {
		t.Error("files row still present after destroy")
	}
}
