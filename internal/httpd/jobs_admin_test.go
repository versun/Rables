package httpd

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/templates"
)

// newJobsAdminTestServer builds a Server with the jobs route on a test-local
// chi router.
func newJobsAdminTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
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
	s := NewServer(database, config.Config{Addr: ":8080", DataDir: t.TempDir(), HMACSecret: "x"}, logger, renderer)
	r := chi.NewRouter()
	RegisterJobsAdminRoutes(r, s)
	return s, r
}

// enqueueTestJobRun inserts one queued job_runs row.
func enqueueTestJobRun(t *testing.T, s *Server, kind string) int64 {
	t.Helper()
	now := time.Now().Unix()
	id, err := s.Q.EnqueueJobRun(t.Context(), query.EnqueueJobRunParams{
		Kind: kind, RunAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("enqueue job run: %v", err)
	}
	return id
}

// TestAdminJobsAuth: the route sits behind RequireAuth.
func TestAdminJobsAuth(t *testing.T) {
	_, h := newJobsAdminTestServer(t)
	rec := doRequest(t, h, http.MethodGet, "/admin/jobs", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
		t.Errorf("GET /admin/jobs unauthenticated: status = %d location = %q, want 302 /session/new",
			rec.Code, rec.Header().Get("Location"))
	}
}

// mainContent extracts the <main> section of an admin page so list
// assertions are not confused by the sidebar nav links.
func mainContent(body string) string {
	if i := strings.Index(body, `<main class="admin-main">`); i >= 0 {
		return body[i:]
	}
	return body
}

// TestAdminJobsIndex: id DESC list with kind/status/attempts/last_error.
func TestAdminJobsIndex(t *testing.T) {
	s, h := newJobsAdminTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	enqueueTestJobRun(t, s, "crosspost")
	failedID := enqueueTestJobRun(t, s, "send_newsletter")
	if err := s.Q.FailJobRun(ctx, query.FailJobRunParams{
		Attempts: 3, LastError: sql.NullString{String: "smtp down", Valid: true},
		UpdatedAt: time.Now().Unix(), ID: failedID,
	}); err != nil {
		t.Fatalf("fail job run: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/admin/jobs", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("index: status = %d", rec.Code)
	}
	body := mainContent(rec.Body.String())
	for _, want := range []string{"crosspost", "send_newsletter", "queued", "failed", "smtp down"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	// id DESC: the later insert renders first.
	if strings.Index(body, "send_newsletter") > strings.Index(body, "crosspost") {
		t.Errorf("index not ordered by id DESC: %q", body)
	}
}

// TestAdminJobsStatusFilter: ?status= filters to the given status; an unknown
// status lists everything.
func TestAdminJobsStatusFilter(t *testing.T) {
	s, h := newJobsAdminTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	enqueueTestJobRun(t, s, "crosspost")
	failedID := enqueueTestJobRun(t, s, "send_newsletter")
	if err := s.Q.FailJobRun(ctx, query.FailJobRunParams{
		Attempts: 1, LastError: sql.NullString{String: "boom", Valid: true},
		UpdatedAt: time.Now().Unix(), ID: failedID,
	}); err != nil {
		t.Fatalf("fail job run: %v", err)
	}

	tests := []struct {
		query   string
		present []string
		absent  []string
	}{
		{"?status=failed", []string{"send_newsletter"}, []string{"crosspost"}},
		{"?status=queued", []string{"crosspost"}, []string{"send_newsletter"}},
		{"?status=bogus", []string{"crosspost", "send_newsletter"}, nil},
		{"", []string{"crosspost", "send_newsletter"}, nil},
	}
	for _, tt := range tests {
		rec := doRequest(t, h, http.MethodGet, "/admin/jobs"+tt.query, nil, session)
		if rec.Code != http.StatusOK {
			t.Fatalf("index %q: status = %d", tt.query, rec.Code)
		}
		body := mainContent(rec.Body.String())
		for _, want := range tt.present {
			if !strings.Contains(body, want) {
				t.Errorf("index %q missing %q", tt.query, want)
			}
		}
		for _, unwanted := range tt.absent {
			if strings.Contains(body, unwanted) {
				t.Errorf("index %q should not contain %q", tt.query, unwanted)
			}
		}
	}
}

// TestAdminJobsPagination: 100 per page; invalid page params 404.
func TestAdminJobsPagination(t *testing.T) {
	s, h := newJobsAdminTestServer(t)
	session := redirectsSessionCookie(t, s)

	for i := 0; i < adminJobsPerPage+1; i++ {
		enqueueTestJobRun(t, s, fmt.Sprintf("kind-%03d", i))
	}

	rec := doRequest(t, h, http.MethodGet, "/admin/jobs", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("page 1: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, fmt.Sprintf("kind-%03d", adminJobsPerPage)) {
		t.Errorf("page 1 missing newest row")
	}
	if strings.Contains(body, "kind-000") {
		t.Errorf("page 1 should not contain the 101st-oldest row")
	}
	if !strings.Contains(body, `href="/admin/jobs?page=2"`) {
		t.Errorf("page 1 missing link to page 2")
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/jobs?page=2", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("page 2: status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "kind-000") {
		t.Errorf("page 2 missing the oldest row")
	}

	for _, bad := range []string{"?page=0", "?page=-1", "?page=abc", "?page=1.5"} {
		rec = doRequest(t, h, http.MethodGet, "/admin/jobs"+bad, nil, session)
		if rec.Code != http.StatusNotFound {
			t.Errorf("index %q: status = %d, want 404", bad, rec.Code)
		}
	}
}
