package httpd

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/templates"
)

// newCrosspostsTestServer builds a Server and mounts only the crossposts
// routes, independent of the integrator's router wiring.
func newCrosspostsTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterCrosspostRoutes(r, s)
	return s, r
}

func crosspostRow(t *testing.T, s *Server, platform string) (enabled int64, serverURL, accessToken string, maxChars sql.NullInt64) {
	t.Helper()
	var srv, tok sql.NullString
	err := s.DB.QueryRowContext(t.Context(),
		`SELECT enabled, server_url, access_token, max_characters FROM crossposts WHERE platform = ?`, platform).
		Scan(&enabled, &srv, &tok, &maxChars)
	if err != nil {
		t.Fatalf("load crosspost %q: %v", platform, err)
	}
	return enabled, srv.String, tok.String, maxChars
}

func TestAdminCrosspostsIndexRequiresAuth(t *testing.T) {
	_, h := newCrosspostsTestServer(t)
	rec := doRequest(t, h, http.MethodGet, "/admin/crossposts", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/session/new" {
		t.Errorf("location = %q", loc)
	}
}

func TestAdminCrosspostsIndex(t *testing.T) {
	s, h := newCrosspostsTestServer(t)
	session := settingsSession(t, s)

	rec := doRequest(t, h, http.MethodGet, "/admin/crossposts", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Mastodon", "X (Twitter)", "Bluesky", "Xiaohongshu", `action="/admin/crossposts/mastodon"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	// GET is read-only: no crossposts rows are created.
	var n int
	if err := s.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM crossposts`).Scan(&n); err != nil {
		t.Fatalf("count crossposts: %v", err)
	}
	if n != 0 {
		t.Errorf("crossposts rows = %d, want 0 (find_or_initialize)", n)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/crossposts?platform=bluesky", nil, session)
	if !strings.Contains(rec.Body.String(), `action="/admin/crossposts/bluesky"`) {
		t.Error("bluesky tab did not render the bluesky form")
	}
	for _, platform := range []string{"twitter", "xiaohongshu"} {
		rec = doRequest(t, h, http.MethodGet, "/admin/crossposts?platform="+platform, nil, session)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="/admin/crossposts/`+platform+`"`) {
			t.Errorf("%s tab did not render its form (status %d)", platform, rec.Code)
		}
	}
}

func TestAdminCrosspostsUpdate(t *testing.T) {
	s, h := newCrosspostsTestServer(t)
	session := settingsSession(t, s)

	form := url.Values{
		"crosspost[platform]":               {"mastodon"},
		"crosspost[enabled]":                {"0", "1"},
		"crosspost[server_url]":             {"https://mastodon.example"},
		"crosspost[client_key]":             {"ck"},
		"crosspost[client_secret]":          {"cs"},
		"crosspost[access_token]":           {"tok"},
		"crosspost[max_characters]":         {"420"},
		"crosspost[auto_fetch_comments]":    {"0", "1"},
		"crosspost[comment_fetch_schedule]": {"daily"},
	}
	rec := doRequest(t, h, http.MethodPost, "/admin/crossposts/mastodon", form, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/crossposts?platform=mastodon" {
		t.Errorf("location = %q", loc)
	}
	if flash := flashOf(t, rec); flash.Notice != "CrossPost settings updated successfully." {
		t.Errorf("notice = %q", flash.Notice)
	}

	enabled, serverURL, token, maxChars := crosspostRow(t, s, "mastodon")
	if enabled != 1 || serverURL != "https://mastodon.example" || token != "tok" {
		t.Errorf("row = enabled %d server %q token %q", enabled, serverURL, token)
	}
	if !maxChars.Valid || maxChars.Int64 != 420 {
		t.Errorf("max_characters = %v", maxChars)
	}
	var schedule string
	var autoFetch int64
	if err := s.DB.QueryRowContext(t.Context(),
		`SELECT auto_fetch_comments, comment_fetch_schedule FROM crossposts WHERE platform = 'mastodon'`).
		Scan(&autoFetch, &schedule); err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	if autoFetch != 1 || schedule != "daily" {
		t.Errorf("fetch comments = %d %q", autoFetch, schedule)
	}
	var activity int64
	if err := s.DB.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM activity_logs WHERE target = 'crosspost' AND action = 'updated'`).Scan(&activity); err != nil {
		t.Fatalf("count activity: %v", err)
	}
	if activity != 1 {
		t.Errorf("updated activity rows = %d, want 1", activity)
	}
}

func TestAdminCrosspostsUpdateValidation(t *testing.T) {
	tests := []struct {
		name      string
		platform  string
		form      url.Values
		wantAlert string
	}{
		{
			name:     "enabled mastodon requires credentials",
			platform: "mastodon",
			form: url.Values{
				"crosspost[platform]": {"mastodon"},
				"crosspost[enabled]":  {"0", "1"},
			},
			wantAlert: "Client key can't be blank, Client secret can't be blank, Access token can't be blank",
		},
		{
			name:     "max characters must be positive",
			platform: "bluesky",
			form: url.Values{
				"crosspost[platform]":       {"bluesky"},
				"crosspost[max_characters]": {"0"},
			},
			wantAlert: "Max characters must be greater than 0",
		},
		{
			name:     "server url needs scheme and host",
			platform: "mastodon",
			form: url.Values{
				"crosspost[platform]":   {"mastodon"},
				"crosspost[server_url]": {"not-a-url"},
			},
			wantAlert: "Server url must be a valid http(s) URL",
		},
		{
			name:     "server url rejects credentials",
			platform: "mastodon",
			form: url.Values{
				"crosspost[platform]":   {"mastodon"},
				"crosspost[server_url]": {"https://user:pw@m.c"},
			},
			wantAlert: "Server url must not include credentials",
		},
		{
			name:     "unknown platform is not in the list",
			platform: "bogus",
			form: url.Values{
				"crosspost[platform]": {"bogus"},
			},
			wantAlert: "Platform is not included in the list",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, h := newCrosspostsTestServer(t)
			session := settingsSession(t, s)

			rec := doRequest(t, h, http.MethodPost, "/admin/crossposts/"+tt.platform, tt.form, session)
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d", rec.Code)
			}
			if flash := flashOf(t, rec); flash.Alert != tt.wantAlert {
				t.Errorf("alert = %q, want %q", flash.Alert, tt.wantAlert)
			}
			// Validation failures leave the row disabled/absent defaults.
			if tt.platform != "bogus" {
				enabled, _, _, _ := crosspostRow(t, s, tt.platform)
				if enabled != 0 {
					t.Errorf("enabled = %d, want 0 after failed update", enabled)
				}
			}
		})
	}
}

// TestAdminCrosspostsUpdateJunkMaxCharacters: the ActiveModel integer
// typecast turns junk into nil, which passes validation (allow_nil).
func TestAdminCrosspostsUpdateJunkMaxCharacters(t *testing.T) {
	s, h := newCrosspostsTestServer(t)
	session := settingsSession(t, s)

	form := url.Values{
		"crosspost[platform]":       {"bluesky"},
		"crosspost[max_characters]": {"abc"},
	}
	rec := doRequest(t, h, http.MethodPost, "/admin/crossposts/bluesky", form, session)
	if flash := flashOf(t, rec); flash.Alert != "" {
		t.Fatalf("alert = %q, want none", flash.Alert)
	}
	_, _, _, maxChars := crosspostRow(t, s, "bluesky")
	if maxChars.Valid {
		t.Errorf("max_characters = %v, want NULL", maxChars)
	}
}

// TestAdminCrosspostsVerifyUnconfigured: a platform without a registered
// implementation reports "not configured" (twitter until T21 lands).
func TestAdminCrosspostsVerifyUnconfigured(t *testing.T) {
	s, h := newCrosspostsTestServer(t)
	session := settingsSession(t, s)

	form := url.Values{"crosspost[platform]": {"bogus"}}
	rec := doRequest(t, h, http.MethodPost, "/admin/crossposts/bogus/verify", form, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "error" || out.Message != "Platform bogus is not configured." {
		t.Errorf("result = %+v", out)
	}
}

func TestAdminCrosspostsVerifyPlatformMismatch(t *testing.T) {
	s, h := newCrosspostsTestServer(t)
	session := settingsSession(t, s)

	form := url.Values{"crosspost[platform]": {"bluesky"}}
	rec := doRequest(t, h, http.MethodPost, "/admin/crossposts/mastodon/verify", form, session)
	var out struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "error" || out.Message != "Verification failed. Please check your settings and try again." {
		t.Errorf("result = %+v", out)
	}
}

// TestAdminCrosspostsVerifyMastodon runs the registered mastodon platform's
// Verify against a fake server, end to end through the handler.
func TestAdminCrosspostsVerifyMastodon(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/accounts/verify_credentials" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"invalid"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"acct":"user"}`))
	}))
	t.Cleanup(fake.Close)

	s, h := newCrosspostsTestServer(t)
	session := settingsSession(t, s)

	form := url.Values{
		"crosspost[platform]":     {"mastodon"},
		"crosspost[server_url]":   {fake.URL},
		"crosspost[access_token]": {"tok"},
	}
	rec := doRequest(t, h, http.MethodPost, "/admin/crossposts/mastodon/verify", form, session)
	var out struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" || out.Message != "Verified Successfully!" {
		t.Errorf("result = %+v", out)
	}

	form.Set("crosspost[access_token]", "wrong")
	rec = doRequest(t, h, http.MethodPost, "/admin/crossposts/mastodon/verify", form, session)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "error" || !strings.Contains(out.Message, "Verification failed: 401") {
		t.Errorf("result = %+v", out)
	}
}
