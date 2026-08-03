package httpd

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// newMigratesTestServer builds a Server with the migrates and downloads
// routes.
func newMigratesTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterDownloadsRoutes(r, s)
	return s, r
}

func TestAdminMigratesAuth(t *testing.T) {
	_, h := newMigratesTestServer(t)
	for _, tt := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/migrates"},
		{http.MethodPost, "/admin/migrates/export"},
		{http.MethodGet, "/admin/downloads/x.zip"},
	} {
		rec := doRequest(t, h, tt.method, tt.path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("%s %s unauthenticated: status = %d location = %q, want 302 /session/new",
				tt.method, tt.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestAdminMigratesIndex(t *testing.T) {
	s, h := newMigratesTestServer(t)
	session := redirectsSessionCookie(t, s)

	// An export zip shows up with a download link.
	exportsDir := filepath.Join(s.Cfg.DataDir, "exports")
	if err := os.MkdirAll(exportsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exportsDir, "export_20260803_120000_1_abcd.zip"), []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		tab      string
		contains []string
	}{
		{"export", []string{`action="/admin/migrates/export"`, "export_20260803_120000_1_abcd.zip", "/admin/downloads/export_20260803_120000_1_abcd.zip"}},
		{"import", []string{`action="/admin/migrates/import"`}},
		{"bogus", []string{`action="/admin/migrates/export"`}}, // falls back to export
	} {
		rec := doRequest(t, h, http.MethodGet, "/admin/migrates?tab="+tt.tab, nil, session)
		if rec.Code != http.StatusOK {
			t.Fatalf("tab %s: status = %d, want 200", tt.tab, rec.Code)
		}
		for _, want := range tt.contains {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("tab %s: body missing %q", tt.tab, want)
			}
		}
	}
}

func TestAdminMigratesExport(t *testing.T) {
	s, h := newMigratesTestServer(t)
	session := redirectsSessionCookie(t, s)

	for _, tt := range []struct {
		name            string
		form            url.Values
		wantLocation    string
		wantFormat      string
		wantKeepSecrets bool
		wantJob         bool
	}{
		{"default", url.Values{"export_type": {"default"}}, "/admin/migrates?tab=export", "default", false, true},
		{"markdown with credentials", url.Values{"export_type": {"markdown"}, "keep_credentials": {"1"}}, "/admin/migrates?tab=export", "markdown", true, true},
		{"unsupported type", url.Values{"export_type": {"xml"}}, "/admin/migrates?tab=export", "", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.DB.Exec(`DELETE FROM job_runs`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.Exec(`DELETE FROM activity_logs`); err != nil {
				t.Fatal(err)
			}
			rec := doRequest(t, h, http.MethodPost, "/admin/migrates/export", tt.form, session)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != tt.wantLocation {
				t.Fatalf("status = %d location = %q, want 302 %q", rec.Code, rec.Header().Get("Location"), tt.wantLocation)
			}

			var payload sql.NullString
			var kind string
			row := s.DB.QueryRow(`SELECT kind, payload FROM job_runs ORDER BY id DESC LIMIT 1`)
			scanErr := row.Scan(&kind, &payload)
			if !tt.wantJob {
				if scanErr == nil {
					t.Errorf("no job expected, got kind %s", kind)
				}
				return
			}
			if scanErr != nil {
				t.Fatalf("expected queued job: %v", scanErr)
			}
			if kind != "export" {
				t.Errorf("kind = %q, want export", kind)
			}
			var p transfer.ExportPayload
			if err := json.Unmarshal([]byte(payload.String), &p); err != nil {
				t.Fatalf("payload not JSON: %v", err)
			}
			if p.Format != tt.wantFormat || p.KeepCredentials != tt.wantKeepSecrets {
				t.Errorf("payload = %+v, want format=%s keep=%v", p, tt.wantFormat, tt.wantKeepSecrets)
			}

			// Queued activity mirrors handle_export's ActivityLog.log!.
			var count int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM activity_logs WHERE target = 'export' AND action = 'queued'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Errorf("queued activity rows = %d, want 1", count)
			}
		})
	}
}

func TestAdminDownloads(t *testing.T) {
	s, h := newMigratesTestServer(t)
	session := redirectsSessionCookie(t, s)

	exportsDir := filepath.Join(s.Cfg.DataDir, "exports")
	importsDir := filepath.Join(s.Cfg.DataDir, "imports")
	for _, dir := range []string{exportsDir, importsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(exportsDir, "export.zip"), []byte("export-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(importsDir, "import.zip"), []byte("import-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file outside the served directories must not be reachable.
	if err := os.WriteFile(filepath.Join(s.Cfg.DataDir, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink inside exports pointing outside must not be served.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(exportsDir, "link.zip")); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"export zip", "/admin/downloads/export.zip", http.StatusOK, "export-bytes"},
		{"import zip", "/admin/downloads/import.zip", http.StatusOK, "import-bytes"},
		{"missing file", "/admin/downloads/nope.zip", http.StatusNotFound, ""},
		{"outside served dirs", "/admin/downloads/secret.txt", http.StatusNotFound, ""},
		{"dotfile rejected", "/admin/downloads/.hidden", http.StatusBadRequest, ""},
		{"dotdot rejected", "/admin/downloads/..", http.StatusBadRequest, ""},
		{"encoded traversal", "/admin/downloads/..%2f..%2fsecret.txt", http.StatusBadRequest, ""},
		{"encoded dotdot", "/admin/downloads/%2e%2e%2fsecret.txt", http.StatusBadRequest, ""},
		{"symlink escape", "/admin/downloads/link.zip", http.StatusBadRequest, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.AddCookie(session)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantBody != "" {
				if rec.Body.String() != tt.wantBody {
					t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
				}
				if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
					t.Errorf("Content-Disposition = %q, want attachment", cd)
				}
			}
		})
	}
}
