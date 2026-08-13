package httpd

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestStaticFilesServePercentEncoded: when the client's escaping differs from
// Go's canonical form (encoding "&", or lowercase hex for non-ASCII bytes),
// net/url keeps the original bytes in URL.RawPath and chi routes on it
// without decoding — serveStaticFile must unescape the wildcard param or the
// DB lookup misses a filename that exists (same fix as slugParam).
func TestStaticFilesServePercentEncoded(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)

	for _, tt := range []struct {
		filename string
		path     string
	}{
		{"a&b.txt", "/static/a%26b.txt"},
		{"报告.txt", "/static/%e6%8a%a5%e5%91%8a.txt"},
	} {
		if rec := uploadStaticFile(t, h, session, tt.filename, "data", ""); rec.Code != http.StatusFound {
			t.Fatalf("upload %s: status = %d", tt.filename, rec.Code)
		}
		rec := doRequest(t, h, http.MethodGet, tt.path, nil)
		if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/files/") {
			t.Errorf("GET %s: status = %d location = %q, want 302 /files/...", tt.path, rec.Code, rec.Header().Get("Location"))
		}
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
		{"", "Filename can't be blank"},
		{"  ", "Filename can't be blank"},
		{"a/b.txt", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{"a\\b.txt", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{"a b.txt", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{"a#b.txt", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{"a?b.txt", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{"a%20b.txt", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{"a%b.txt", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{"a\x00b", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{"a\x1fb", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{"a\x7fb", "Filename must not contain slashes, spaces, ?, #, % or control characters"},
		{".", "Filename must not be only dots"},
		{"..", "Filename must not be only dots"},
		{"...", "Filename must not be only dots"},
		{"a.txt", ""},
		{".hidden", ""},
	}
	for _, tt := range tests {
		if got := staticFilenameError(tt.filename); got != tt.want {
			t.Errorf("staticFilenameError(%q) = %q, want %q", tt.filename, got, tt.want)
		}
	}
}

// TestStaticFilesPurgeOnCreateFailure: when the static_files row cannot be
// created after the blob was stored, the freshly stored files row and blob
// are purged instead of staying publicly reachable at /files/{key} with
// nothing referencing them.
func TestStaticFilesPurgeOnCreateFailure(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)

	if _, err := s.DB.Exec(`CREATE TRIGGER reject_static_file_inserts BEFORE INSERT ON static_files
		BEGIN SELECT RAISE(ABORT, 'insert blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	rec := uploadStaticFile(t, h, session, "doomed.txt", "doomed content", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("upload: status = %d, want 500", rec.Code)
	}
	var rows int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if rows != 0 {
		t.Errorf("files rows = %d, want 0 (orphan row not purged)", rows)
	}
	blobs := 0
	walkErr := filepath.Walk(filepath.Join(s.Cfg.DataDir, "files"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			blobs++
		}
		return err
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		t.Fatalf("walk files dir: %v", walkErr)
	}
	if blobs != 0 {
		t.Errorf("blobs on disk = %d, want 0 (orphan blob not purged)", blobs)
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

// TestStaticFilesPurgeSharedFile: when a database import has merged a static
// file onto a files row that an attachment also references, destroying the
// static file removes only the static_files row — the shared files row and
// its blob stay, so the attachment keeps serving.
func TestStaticFilesPurgeSharedFile(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	rec := uploadStaticFile(t, h, session, "shared.txt", "shared content", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("upload: status = %d", rec.Code)
	}
	staticFile, err := s.Q.GetStaticFileByFilename(ctx, "shared.txt")
	if err != nil {
		t.Fatalf("get static file: %v", err)
	}
	file, err := s.Q.GetFileForStaticFilename(ctx, "shared.txt")
	if err != nil {
		t.Fatalf("get file row: %v", err)
	}
	// Simulate the import merge: an article attachment on the same files row.
	if _, err := s.DB.Exec(`INSERT INTO attachments (file_id, record_type, record_id, name, created_at) VALUES (?, 'Article', 1, 'image', ?)`, file.ID, time.Now().Unix()); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/static_files/"+itoa(staticFile.ID)+"/destroy", nil, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("destroy: status = %d", rec.Code)
	}
	rec = doRequest(t, h, http.MethodGet, "/static/shared.txt", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("show after destroy: status = %d, want 404", rec.Code)
	}

	// The shared files row and its blob survive; the attachment URL serves.
	if _, err := s.Q.GetFileByKey(ctx, file.Key); err != nil {
		t.Errorf("shared files row purged despite the attachment: %v", err)
	}
	if _, err := os.Stat(s.Media().PathFor(file.Key)); err != nil {
		t.Errorf("shared blob removed from disk despite the attachment: %v", err)
	}
	rec = doRequest(t, h, http.MethodGet, "/files/"+file.Key, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "shared content" {
		t.Errorf("attachment blob serve: status = %d body = %q, want 200 shared content", rec.Code, rec.Body.String())
	}
}

// TestStaticFilesOverwriteCASMismatch: when the compare-and-swap update
// matches no row (a concurrent overwrite moved the row to another blob
// between this request's read and its update), the loser purges its own
// freshly stored blob and re-renders with an alert instead of leaking it.
// The RAISE(IGNORE) trigger abandons the update without an error, simulating
// the lost race deterministically.
func TestStaticFilesOverwriteCASMismatch(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	rec := uploadStaticFile(t, h, session, "stale.txt", "v1", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("seed upload: status = %d", rec.Code)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER abandon_static_file_updates BEFORE UPDATE ON static_files
		BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	rec = uploadStaticFile(t, h, session, "stale.txt", "v2", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "与另一个上传冲突，请重试") {
		t.Fatalf("lost overwrite: status = %d, want 200 with the conflict alert", rec.Code)
	}

	// The row still points at the original blob, which is still served.
	file, err := s.Q.GetFileForStaticFilename(ctx, "stale.txt")
	if err != nil {
		t.Fatalf("get file row: %v", err)
	}
	rec = doRequest(t, h, http.MethodGet, "/files/"+file.Key, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "v1" {
		t.Errorf("served content = %q (status %d), want v1", rec.Body.String(), rec.Code)
	}

	// The loser's files row and blob were purged.
	if rows := countFileRows(t, s); rows != 1 {
		t.Errorf("files rows = %d, want 1 (loser's row not purged)", rows)
	}
	if blobs := countStoredBlobs(t, s); blobs != 1 {
		t.Errorf("blobs on disk = %d, want 1 (loser's blob not purged)", blobs)
	}
}

// TestStaticFilesConcurrentOverwrite: concurrent uploads over the same
// filename race to replace the row; the compare-and-swap update makes each
// loser purge its own blob, so no orphan files row or disk blob survives
// (it would stay reachable at /files/{key} with nothing referencing it).
func TestStaticFilesConcurrentOverwrite(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	rec := uploadStaticFile(t, h, session, "race.txt", "v0", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("seed upload: status = %d", rec.Code)
	}

	const uploads = 8
	recs := make([]*httptest.ResponseRecorder, uploads)
	var wg sync.WaitGroup
	for i := range uploads {
		body, contentType := staticFileUploadBody(t, "race.txt", fmt.Sprintf("v%d", i+1), "")
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/admin/static_files", body)
			req.Header.Set("Content-Type", contentType)
			req.AddCookie(session)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			recs[i] = rec
		}()
	}
	wg.Wait()

	for _, rec := range recs {
		// Winners redirect; losers re-render the index with the alert.
		if rec.Code != http.StatusFound && rec.Code != http.StatusOK {
			t.Errorf("concurrent upload: unexpected status = %d", rec.Code)
		}
	}

	rows, err := s.Q.ListStaticFiles(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("static_files rows = %d err = %v, want exactly 1", len(rows), err)
	}
	if n := countFileRows(t, s); n != 1 {
		t.Errorf("files rows = %d, want 1 (orphan row leaked)", n)
	}
	if blobs := countStoredBlobs(t, s); blobs != 1 {
		t.Errorf("blobs on disk = %d, want 1 (orphan blob leaked)", blobs)
	}

	// The surviving row serves a winning upload's content, never v0's.
	rec = doRequest(t, h, http.MethodGet, "/static/race.txt", nil)
	rec = doRequest(t, h, http.MethodGet, rec.Header().Get("Location"), nil)
	if body := rec.Body.String(); rec.Code != http.StatusOK || body == "v0" || !strings.HasPrefix(body, "v") {
		t.Errorf("surviving content = %q (status %d), want one of the concurrent uploads", body, rec.Code)
	}
}

// TestStaticFilesConcurrentCreate: concurrent uploads of the same brand-new
// filename race past the existence check; the UNIQUE constraint makes every
// loser fail its insert, and each loser must purge its own stored blob and
// re-render with the conflict alert instead of erroring out with a bare 500.
func TestStaticFilesConcurrentCreate(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	const uploads = 8
	recs := make([]*httptest.ResponseRecorder, uploads)
	var wg sync.WaitGroup
	for i := range uploads {
		body, contentType := staticFileUploadBody(t, "create-race.txt", fmt.Sprintf("v%d", i+1), "")
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/admin/static_files", body)
			req.Header.Set("Content-Type", contentType)
			req.AddCookie(session)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			recs[i] = rec
		}()
	}
	wg.Wait()

	redirects := 0
	for _, rec := range recs {
		switch rec.Code {
		case http.StatusFound:
			redirects++
		case http.StatusOK:
			if !strings.Contains(rec.Body.String(), "与另一个上传冲突，请重试") {
				t.Errorf("losing upload: status = 200 without the conflict alert")
			}
		default:
			t.Errorf("concurrent upload: unexpected status = %d", rec.Code)
		}
	}
	if redirects == 0 {
		t.Error("no upload won the race, want at least one redirect")
	}

	rows, err := s.Q.ListStaticFiles(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("static_files rows = %d err = %v, want exactly 1", len(rows), err)
	}
	if n := countFileRows(t, s); n != 1 {
		t.Errorf("files rows = %d, want 1 (orphan row leaked)", n)
	}
	if blobs := countStoredBlobs(t, s); blobs != 1 {
		t.Errorf("blobs on disk = %d, want 1 (orphan blob leaked)", blobs)
	}

	// The surviving row serves one of the uploaded contents.
	rec := doRequest(t, h, http.MethodGet, "/static/create-race.txt", nil)
	rec = doRequest(t, h, http.MethodGet, rec.Header().Get("Location"), nil)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.HasPrefix(body, "v") {
		t.Errorf("surviving content = %q (status %d), want one of the concurrent uploads", body, rec.Code)
	}
}

// TestStaticFilesConcurrentDestroyOverwrite: a destroy racing an overwrite of
// the same filename must not leak the blob the overwrite swapped in — the
// delete purges the file the row pointed at when it was actually removed.
// Afterwards each filename is either fully gone or fully consistent, with no
// orphan files row or disk blob.
func TestStaticFilesConcurrentDestroyOverwrite(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	const rounds = 12
	for i := range rounds {
		filename := fmt.Sprintf("destroy-race-%d.txt", i)
		rec := uploadStaticFile(t, h, session, filename, "v1", "")
		if rec.Code != http.StatusFound {
			t.Fatalf("seed upload %s: status = %d", filename, rec.Code)
		}
		staticFile, err := s.Q.GetStaticFileByFilename(ctx, filename)
		if err != nil {
			t.Fatalf("get %s: %v", filename, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var destroyRec, overwriteRec *httptest.ResponseRecorder
		go func() {
			defer wg.Done()
			destroyRec = doRequest(t, h, http.MethodPost, "/admin/static_files/"+itoa(staticFile.ID)+"/destroy", nil, session)
		}()
		go func() {
			defer wg.Done()
			overwriteRec = uploadStaticFile(t, h, session, filename, "v2", "")
		}()
		wg.Wait()

		if destroyRec.Code != http.StatusFound {
			t.Errorf("destroy %s: status = %d, want 302", filename, destroyRec.Code)
		}
		if overwriteRec.Code != http.StatusFound && overwriteRec.Code != http.StatusOK {
			t.Errorf("overwrite %s: unexpected status = %d", filename, overwriteRec.Code)
		}
	}

	// A filename survives only when the overwrite re-created the row after the
	// delete landed; it then serves the overwrite's content.
	rows, err := s.Q.ListStaticFiles(ctx)
	if err != nil {
		t.Fatalf("list static files: %v", err)
	}
	for _, row := range rows {
		rec := doRequest(t, h, http.MethodGet, "/static/"+row.Filename, nil)
		rec = doRequest(t, h, http.MethodGet, rec.Header().Get("Location"), nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "v2" {
			t.Errorf("%s: served content = %q (status %d), want v2", row.Filename, rec.Body.String(), rec.Code)
		}
	}
	if n := countFileRows(t, s); n != len(rows) {
		t.Errorf("files rows = %d, want %d (one per surviving static file; orphan leaked)", n, len(rows))
	}
	if blobs := countStoredBlobs(t, s); blobs != len(rows) {
		t.Errorf("blobs on disk = %d, want %d (one per surviving static file; orphan leaked)", blobs, len(rows))
	}
}

// TestStaticFilesPurgeKeepsBlobWhenRowDeleteFails: when the files-row delete
// fails (e.g. a foreign-key constraint after an import merged a fresh
// reference between the reference check and the delete), the blob must stay —
// the surviving row still points at it. The trigger makes the delete fail
// deterministically.
func TestStaticFilesPurgeKeepsBlobWhenRowDeleteFails(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	rec := uploadStaticFile(t, h, session, "pinned.txt", "pinned", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("upload: status = %d", rec.Code)
	}
	staticFile, err := s.Q.GetStaticFileByFilename(ctx, "pinned.txt")
	if err != nil {
		t.Fatalf("get static file: %v", err)
	}
	file, err := s.Q.GetFileForStaticFilename(ctx, "pinned.txt")
	if err != nil {
		t.Fatalf("get file row: %v", err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER reject_file_deletes BEFORE DELETE ON files
		BEGIN SELECT RAISE(ABORT, 'delete blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/static_files/"+itoa(staticFile.ID)+"/destroy", nil, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("destroy: status = %d, want 302", rec.Code)
	}
	if _, err := s.Q.GetFileByKey(ctx, file.Key); err != nil {
		t.Errorf("files row gone although its delete failed: %v", err)
	}
	if _, err := os.Stat(s.Media().PathFor(file.Key)); err != nil {
		t.Errorf("blob removed although the files row survives: %v", err)
	}
}

// TestStaticFilesPurgeAbortsWhenVariantsListFails: when listing the file's
// variants errors out, the purge must abort instead of deleting the original
// anyway — the unknown variant family could pin the original. Renaming
// variant_of makes the variant query fail deterministically.
func TestStaticFilesPurgeAbortsWhenVariantsListFails(t *testing.T) {
	s, h := newStaticFilesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	rec := uploadStaticFile(t, h, session, "family.txt", "family", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("upload: status = %d", rec.Code)
	}
	staticFile, err := s.Q.GetStaticFileByFilename(ctx, "family.txt")
	if err != nil {
		t.Fatalf("get static file: %v", err)
	}
	file, err := s.Q.GetFileForStaticFilename(ctx, "family.txt")
	if err != nil {
		t.Fatalf("get file row: %v", err)
	}
	if _, err := s.DB.Exec(`ALTER TABLE files RENAME COLUMN variant_of TO variant_of_broken`); err != nil {
		t.Fatalf("rename column: %v", err)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/static_files/"+itoa(staticFile.ID)+"/destroy", nil, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("destroy: status = %d, want 302", rec.Code)
	}
	// Raw COUNT(*): the generated queries name variant_of explicitly and
	// would fail for the same reason as the variant listing.
	var rows int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE id = ?`, file.ID).Scan(&rows); err != nil {
		t.Fatalf("count files row: %v", err)
	}
	if rows != 1 {
		t.Errorf("files row purged although the variant listing failed")
	}
	if _, err := os.Stat(s.Media().PathFor(file.Key)); err != nil {
		t.Errorf("blob removed although the purge aborted: %v", err)
	}
}

// staticFileUploadBody builds the multipart upload form like
// uploadStaticFile but without issuing the request, so concurrent tests can
// build each request body before starting their goroutines.
func staticFileUploadBody(t *testing.T, filename, content, description string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if description != "" {
		if err := mw.WriteField("description", description); err != nil {
			t.Fatalf("write description: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// countFileRows returns the number of rows in the files table.
func countFileRows(t *testing.T, s *Server) int {
	t.Helper()
	var rows int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatalf("count files: %v", err)
	}
	return rows
}

// countStoredBlobs returns the number of blob files under the media dir.
func countStoredBlobs(t *testing.T, s *Server) int {
	t.Helper()
	blobs := 0
	err := filepath.Walk(filepath.Join(s.Cfg.DataDir, "files"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			blobs++
		}
		return err
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk files dir: %v", err)
	}
	return blobs
}
