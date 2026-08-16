package httpd

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/templates"
)

// newDownloadsTestServer mounts only the admin download routes, with a real
// DataDir so the handler can serve files from exports/.
func newDownloadsTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterDownloadsRoutes(r, s)
	return s, r
}

// deadlineRecorder wraps httptest.ResponseRecorder with the SetReadDeadline /
// SetWriteDeadline methods http.ResponseController looks for on the
// underlying connection writer, so a test can observe which deadline a
// handler clears. A nil field means the handler never called the setter.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	readDeadline  *time.Time
	writeDeadline *time.Time
}

func (r *deadlineRecorder) SetReadDeadline(t time.Time) error {
	r.readDeadline = &t
	return nil
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	r.writeDeadline = &t
	return nil
}

// postFileDeadline posts a one-file multipart form through a deadlineRecorder
// and returns the recorder for deadline assertions.
func postFileDeadline(t *testing.T, h http.Handler, path, field, filename, contentType string, content []byte, session *http.Cookie) *deadlineRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, filename))
	hdr.Set("Content-Type", contentType)
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(session)
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(rec, req)
	return rec
}

// TestAdminDownloadShowClearsWriteDeadline: the served export zip must not
// carry the server-wide WriteTimeout, or a slow client loses a large
// download mid-stream. The request runs through statusRecorder to prove the
// ResponseController walk reaches the connection writer in production.
func TestAdminDownloadShowClearsWriteDeadline(t *testing.T) {
	s, h := newDownloadsTestServer(t)
	session := redirectsSessionCookie(t, s)
	exportsDir := filepath.Join(s.Cfg.DataDir, "exports")
	if err := os.MkdirAll(exportsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exportsDir, "export_1.zip"), []byte("zip-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/downloads/export_1.zip", nil)
	req.AddCookie(session)
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(&statusRecorder{ResponseWriter: rec, status: http.StatusOK}, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "zip-bytes" {
		t.Fatalf("status = %d body = %q, want 200 zip-bytes", rec.Code, rec.Body.String())
	}
	if rec.writeDeadline == nil {
		t.Fatal("SetWriteDeadline never called; the server-wide WriteTimeout still applies")
	}
	if !rec.writeDeadline.IsZero() {
		t.Errorf("write deadline = %v, want zero (cleared)", *rec.writeDeadline)
	}
	if rec.readDeadline != nil {
		t.Errorf("read deadline = %v, want untouched (nil)", *rec.readDeadline)
	}
}

// TestAdminDownloadShowPercentFilename: a filename containing a literal "%"
// arrives already decoded by chi (the client encoding 50%25off.zip matches
// Go's canonical escaping, so URL.RawPath stays empty); a second
// PathUnescape must not run or the "%of" trips a 400 and the file can never
// be downloaded.
func TestAdminDownloadShowPercentFilename(t *testing.T) {
	s, h := newDownloadsTestServer(t)
	session := redirectsSessionCookie(t, s)
	exportsDir := filepath.Join(s.Cfg.DataDir, "exports")
	if err := os.MkdirAll(exportsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exportsDir, "50%off.zip"), []byte("zip-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/downloads/50%25off.zip", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "zip-bytes" {
		t.Fatalf("status = %d body = %q, want 200 zip-bytes", rec.Code, rec.Body.String())
	}
}

// TestLargeUploadsClearRequestDeadlines: the upload endpoints with multi-
// hundred-MB bodies clear the server-wide 30s ReadTimeout and 60s
// WriteTimeout — an upload outliving the write deadline gets its final
// redirect rejected with i/o timeout after the file was already stored, so a
// retry would duplicate the upload/import. The MaxBytesReader cap stays as
// the only size guard.
func TestLargeUploadsClearRequestDeadlines(t *testing.T) {
	zipBytes := []byte("PK\x03\x04fake-zip")

	checkDeadlines := func(t *testing.T, rec *deadlineRecorder) {
		t.Helper()
		if rec.readDeadline == nil || !rec.readDeadline.IsZero() {
			t.Errorf("read deadline = %v, want zero (cleared)", rec.readDeadline)
		}
		if rec.writeDeadline == nil || !rec.writeDeadline.IsZero() {
			t.Errorf("write deadline = %v, want zero (cleared)", rec.writeDeadline)
		}
	}

	t.Run("migrates db import", func(t *testing.T) {
		s, h := newMigratesImportTestServer(t)
		session := redirectsSessionCookie(t, s)
		rec := postFileDeadline(t, h, "/admin/migrates/import", "backup_file", "backup.zip", "application/zip", zipBytes, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		checkDeadlines(t, rec)
	})

	t.Run("migrates rails import", func(t *testing.T) {
		s, h := newMigratesImportTestServer(t)
		session := redirectsSessionCookie(t, s)
		rec := postFileDeadline(t, h, "/admin/migrates/import_rails", "db_file", "production.sqlite3", "application/octet-stream", []byte("sqlite"), session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		checkDeadlines(t, rec)
	})

	t.Run("media upload", func(t *testing.T) {
		s, h, _ := newMediaServer(t)
		session := mediaSession(t, s)
		rec := postFileDeadline(t, h, "/admin/uploads", "file", "notes.txt", "text/plain", []byte("hello"), session)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
		checkDeadlines(t, rec)
	})
}
