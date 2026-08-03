package httpd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/service/twittersync"
	"rables/internal/templates"
)

// newTwitterSyncTestServer builds a Server and mounts only the twitter sync
// admin routes on a test-local chi router.
func newTwitterSyncTestServer(t *testing.T) (*Server, http.Handler) {
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
	s := NewServer(database, config.Config{Addr: ":8080", HMACSecret: "x"}, logger, renderer)
	r := chi.NewRouter()
	RegisterTwitterSyncRoutes(r, s)
	return s, r
}

func twitterSyncSessionCookie(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	now := time.Now().Unix()
	user, err := s.Q.CreateUser(t.Context(), query.CreateUserParams{
		UserName: "admin", PasswordDigest: "x", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.Q.CreateSession(t.Context(), query.CreateSessionParams{
		Token: "twitter-sync-test-token", UserID: user.ID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: "twitter-sync-test-token"}
}

func twitterSyncForm(values map[string]string) url.Values {
	form := url.Values{}
	for k, v := range values {
		form.Set("twitter_sync["+k+"]", v)
	}
	return form
}

func getTwitterSyncRow(t *testing.T, s *Server) query.TwitterSync {
	t.Helper()
	row, err := s.Q.GetTwitterSync(context.Background())
	if err != nil {
		t.Fatalf("get twitter_syncs: %v", err)
	}
	return row
}

func TestTwitterSyncShow(t *testing.T) {
	s, h := newTwitterSyncTestServer(t)
	cookie := twitterSyncSessionCookie(t, s)

	req := httptest.NewRequest(http.MethodGet, "/admin/twitter_sync", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Twitter Sync", `name="twitter_sync[username]"`, `name="twitter_sync[start_date]"`,
		`name="twitter_sync[sync_schedule]"`, "every_15_minutes", "Never",
		`/admin/twitter_sync/sync_now`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("show body missing %q", want)
		}
	}
	// The singleton row now exists (first_or_create).
	row := getTwitterSyncRow(t, s)
	if row.SyncSchedule != "every_15_minutes" {
		t.Errorf("default sync_schedule = %q, want every_15_minutes", row.SyncSchedule)
	}
}

func TestTwitterSyncShowRequiresAuth(t *testing.T) {
	_, h := newTwitterSyncTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/twitter_sync", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 redirect to login", rec.Code)
	}
}

func TestTwitterSyncUpdate(t *testing.T) {
	s, h := newTwitterSyncTestServer(t)
	cookie := twitterSyncSessionCookie(t, s)

	post := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/twitter_sync", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Happy path: @ prefix is normalized away.
	rec := post(twitterSyncForm(map[string]string{
		"enabled": "1", "username": "@alice", "sync_schedule": "daily", "start_date": "2024-06-01",
	}))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if flash := flashOf(t, rec); flash.Notice != "Twitter sync settings updated successfully." {
		t.Errorf("notice = %q", flash.Notice)
	}
	row := getTwitterSyncRow(t, s)
	if row.Enabled != 1 || row.Username.String != "alice" || row.SyncSchedule != "daily" || row.StartDate.String != "2024-06-01" {
		t.Errorf("row = %+v, want enabled/alice/daily/2024-06-01", row)
	}

	// Validation errors redirect with the Rails full message.
	rec = post(twitterSyncForm(map[string]string{"enabled": "1", "username": "", "sync_schedule": "daily"}))
	if flash := flashOf(t, rec); flash.Alert != "Username can't be blank" {
		t.Errorf("alert = %q, want %q", flash.Alert, "Username can't be blank")
	}
	rec = post(twitterSyncForm(map[string]string{"username": "alice", "sync_schedule": "fortnightly"}))
	if flash := flashOf(t, rec); flash.Alert != "Sync schedule is not included in the list" {
		t.Errorf("alert = %q, want schedule inclusion error", flash.Alert)
	}
}

func TestTwitterSyncUpdateResetsCursor(t *testing.T) {
	s, h := newTwitterSyncTestServer(t)
	cookie := twitterSyncSessionCookie(t, s)
	post := func(form url.Values) {
		req := httptest.NewRequest(http.MethodPost, "/admin/twitter_sync", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
	}

	// Seed a row with an established cursor.
	now := time.Now().Unix()
	if _, err := s.DB.Exec(`INSERT INTO twitter_syncs
		(id, enabled, username, user_id, since_id, start_date, sync_schedule, last_synced_at, last_error, created_at, updated_at)
		VALUES (1, 1, 'alice', '42', '100', NULL, 'hourly', ?, 'boom', ?, ?)`, now-3600, now, now); err != nil {
		t.Fatalf("seed twitter_syncs: %v", err)
	}

	// Unrelated change (schedule only): cursor untouched.
	post(twitterSyncForm(map[string]string{"enabled": "1", "username": "alice", "sync_schedule": "daily"}))
	row := getTwitterSyncRow(t, s)
	if row.SinceID.String != "100" || row.UserID.String != "42" || !row.LastSyncedAt.Valid {
		t.Errorf("schedule-only update reset cursor: %+v", row)
	}

	// start_date change: cursor resets, user_id stays.
	post(twitterSyncForm(map[string]string{"enabled": "1", "username": "alice", "sync_schedule": "daily", "start_date": "2024-01-01"}))
	row = getTwitterSyncRow(t, s)
	if row.SinceID.Valid || row.LastSyncedAt.Valid || row.LastError.Valid {
		t.Errorf("start_date change must clear since_id/last_synced_at/last_error: %+v", row)
	}
	if row.UserID.String != "42" {
		t.Errorf("start_date change must keep user_id, got %q", row.UserID.String)
	}

	// Re-establish the cursor, then change the username: user_id resets too.
	if _, err := s.DB.Exec(`UPDATE twitter_syncs SET since_id = '200', last_synced_at = ?, last_error = 'x'`, now); err != nil {
		t.Fatalf("re-seed cursor: %v", err)
	}
	post(twitterSyncForm(map[string]string{"enabled": "1", "username": "bob", "sync_schedule": "daily", "start_date": "2024-01-01"}))
	row = getTwitterSyncRow(t, s)
	if row.UserID.Valid || row.SinceID.Valid || row.LastSyncedAt.Valid || row.LastError.Valid {
		t.Errorf("username change must clear user_id and cursor: %+v", row)
	}
	if row.Username.String != "bob" {
		t.Errorf("username = %q, want bob", row.Username.String)
	}
}

func TestTwitterSyncNowGuard(t *testing.T) {
	s, h := newTwitterSyncTestServer(t)
	cookie := twitterSyncSessionCookie(t, s)
	postNow := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/twitter_sync/sync_now", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Nothing configured → alert, no run.
	rec := postNow()
	if flash := flashOf(t, rec); !strings.HasPrefix(flash.Alert, "Twitter sync is not enabled") {
		t.Errorf("alert = %q, want the not-enabled guard", flash.Alert)
	}

	// Enabled but the crosspost credentials are missing → still guarded.
	// (The first sync_now call already first_or_created the row.)
	if _, err := s.DB.Exec(`UPDATE twitter_syncs SET enabled = 1, username = 'alice', sync_schedule = 'hourly'`); err != nil {
		t.Fatalf("seed twitter_syncs: %v", err)
	}
	rec = postNow()
	if flash := flashOf(t, rec); !strings.HasPrefix(flash.Alert, "Twitter sync is not enabled") {
		t.Errorf("alert = %q, want the missing-credentials guard", flash.Alert)
	}
}

func TestTwitterSyncNowRuns(t *testing.T) {
	s, h := newTwitterSyncTestServer(t)
	cookie := twitterSyncSessionCookie(t, s)
	now := time.Now().Unix()
	if _, err := s.DB.Exec(`INSERT INTO twitter_syncs (id, enabled, username, sync_schedule, created_at, updated_at)
		VALUES (1, 1, 'alice', 'hourly', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed twitter_syncs: %v", err)
	}
	if _, err := s.DB.Exec(`INSERT INTO crossposts (platform, enabled, api_key, api_key_secret, access_token, access_token_secret, created_at, updated_at)
		VALUES ('twitter', 1, 'k', 'ks', 't', 'ts', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed crossposts: %v", err)
	}

	// Fake X API; the shared syncer (same Ext key the integrator uses) points
	// at it so the async sync_now run is observable.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/users/by/username/"):
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
		default:
			fmt.Fprint(w, `{"data":[{"id":"7","text":"hello","created_at":"2024-06-01T12:00:00.000Z"}],"meta":{"result_count":1}}`)
		}
	}))
	t.Cleanup(api.Close)
	syncer := twittersync.NewSyncer(s.DB, t.TempDir())
	syncer.SetBaseURL(api.URL)
	syncer.SetHTTPClient(api.Client())
	s.Ext.Store(twitterSyncExtKey, syncer)

	req := httptest.NewRequest(http.MethodPost, "/admin/twitter_sync/sync_now", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if flash := flashOf(t, rec); flash.Notice != "Twitter sync has been queued." {
		t.Errorf("notice = %q", flash.Notice)
	}

	// The async run archives the tweet (perform_later semantics).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM articles WHERE slug = 'tweet-7'`).Scan(&count); err != nil {
			t.Fatalf("count articles: %v", err)
		}
		if count == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("sync_now run did not archive tweet-7 within 5s")
}
