package httpd

import (
	"bytes"
	"database/sql"
	"encoding/json"
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

// postZipUpload sends a multipart form with a single file field.
func postZipUpload(t *testing.T, h http.Handler, fieldName, filename, contentType string, content []byte, session *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if content != nil {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, filename))
		hdr.Set("Content-Type", contentType)
		part, err := mw.CreatePart(hdr)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/migrates/import", &buf)
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

func TestAdminMigratesImportAuth(t *testing.T) {
	_, h := newMigratesImportTestServer(t)
	for _, path := range []string{"/admin/migrates/import", "/admin/migrates/import_rss"} {
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
	for _, want := range []string{`action="/admin/migrates/import"`, `action="/admin/migrates/import_rss"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("import tab missing %q", want)
		}
	}
}

func TestAdminMigratesImportRSS(t *testing.T) {
	s, h := newMigratesImportTestServer(t)
	session := redirectsSessionCookie(t, s)

	for _, tt := range []struct {
		name       string
		form       url.Values
		wantJob    bool
		wantImages bool
	}{
		{"with images", url.Values{"url": {"https://example.com/feed.xml"}, "import_images": {"1"}}, true, true},
		{"without images", url.Values{"url": {"https://example.com/feed.xml"}}, true, false},
		{"blank url", url.Values{"url": {"  "}}, false, false},
		{"missing url", url.Values{}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.DB.Exec(`DELETE FROM job_runs`); err != nil {
				t.Fatal(err)
			}
			rec := doRequest(t, h, http.MethodPost, "/admin/migrates/import_rss", tt.form, session)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
				t.Fatalf("status = %d location = %q, want 302 /admin/migrates?tab=import", rec.Code, rec.Header().Get("Location"))
			}
			kind, payload, err := latestJob(t, s)
			if !tt.wantJob {
				if err == nil {
					t.Errorf("no job expected, got kind %q", kind)
				}
				return
			}
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
			if p.URL != "https://example.com/feed.xml" || p.ImportImages != tt.wantImages {
				t.Errorf("payload = %+v, want url set and import_images=%v", p, tt.wantImages)
			}
		})
	}
}

func TestAdminMigratesImportZip(t *testing.T) {
	s, h := newMigratesImportTestServer(t)
	session := redirectsSessionCookie(t, s)
	zipBytes := []byte("PK\x03\x04fake-zip")

	t.Run("valid zip enqueued", func(t *testing.T) {
		rec := postZipUpload(t, h, "zip_file", "backup.zip", "application/zip", zipBytes, session)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/migrates?tab=import" {
			t.Fatalf("status = %d location = %q, want 302 tab=import", rec.Code, rec.Header().Get("Location"))
		}
		kind, payload, err := latestJob(t, s)
		if err != nil {
			t.Fatalf("expected queued job: %v", err)
		}
		if kind != "import_zip" {
			t.Errorf("kind = %q, want import_zip", kind)
		}
		var p transfer.ImportZipPayload
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

	t.Run("zip extension accepted with octet-stream type", func(t *testing.T) {
		if _, err := s.DB.Exec(`DELETE FROM job_runs`); err != nil {
			t.Fatal(err)
		}
		rec := postZipUpload(t, h, "zip_file", "backup.zip", "application/octet-stream", zipBytes, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if _, _, err := latestJob(t, s); err != nil {
			t.Errorf("expected queued job: %v", err)
		}
	})

	t.Run("non-zip rejected", func(t *testing.T) {
		if _, err := s.DB.Exec(`DELETE FROM job_runs`); err != nil {
			t.Fatal(err)
		}
		importsDir := filepath.Join(s.Cfg.DataDir, "imports")
		before, _ := os.ReadDir(importsDir)
		rec := postZipUpload(t, h, "zip_file", "notes.txt", "text/plain", []byte("hello"), session)
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
		if _, err := s.DB.Exec(`DELETE FROM job_runs`); err != nil {
			t.Fatal(err)
		}
		rec := postZipUpload(t, h, "zip_file", "", "", nil, session)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if kind, _, err := latestJob(t, s); err == nil {
			t.Errorf("no job expected, got kind %q", kind)
		}
	})
}
