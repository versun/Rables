package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/service/activity"
	"rables/internal/templates"
)

// newActivitiesTestServer builds a Server with the activities route on a
// test-local chi router.
func newActivitiesTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterActivitiesRoutes(r, s)
	return s, r
}

// TestAdminActivitiesAuth: the route sits behind RequireAuth.
func TestAdminActivitiesAuth(t *testing.T) {
	_, h := newActivitiesTestServer(t)
	rec := doRequest(t, h, http.MethodGet, "/admin/activities", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
		t.Errorf("GET /admin/activities unauthenticated: status = %d location = %q, want 302 /session/new",
			rec.Code, rec.Header().Get("Location"))
	}
}

// TestAdminActivitiesIndex mirrors Admin::ActivitiesController#index: latest
// rows with level token, action, target and description.
func TestAdminActivitiesIndex(t *testing.T) {
	s, h := newActivitiesTestServer(t)
	session := redirectsSessionCookie(t, s)
	ctx := t.Context()

	activity.Log(ctx, s.DB, "info", "created", "redirect", `regex="^/a$" replacement="/b"`)
	activity.Log(ctx, s.DB, "warning", "synced", "twitter", `count="3"`)

	rec := doRequest(t, h, http.MethodGet, "/admin/activities", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("index: status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"created", "redirect", `regex=&#34;^/a$&#34; replacement=&#34;/b&#34;`, "level-info", "synced", "level-warn"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
}
