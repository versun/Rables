package httpd

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"rables/internal/db/query"
)

// TestSetupLoginFlow covers the full path: setup redirect, setup, login,
// a protected page, and logout.
func TestSetupLoginFlow(t *testing.T) {
	_, h := newTestServer(t)

	// Before setup, the login page stays reachable (the /session prefix is
	// exempt from the setup redirect) but the rest of the site is not.
	rec := doRequest(t, h, http.MethodGet, "/session/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /session/new pre-setup: status = %d, want %d", rec.Code, http.StatusOK)
	}
	rec = doRequest(t, h, http.MethodGet, "/", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("GET / pre-setup: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	// The setup page itself is reachable.
	if rec := doRequest(t, h, http.MethodGet, "/setup", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /setup: status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Complete setup; the flash cookie rides along to the login page.
	setupRec := completeSetup(t, h)
	flashCookie := findCookie(setupRec, flashCookieName)
	if flashCookie == nil {
		t.Fatal("setup did not set a flash cookie")
	}
	rec = doRequest(t, h, http.MethodGet, "/session/new", nil, flashCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /session/new: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "Setup completed successfully!") {
		t.Error("login page does not show the setup flash notice")
	}
	if c := findCookie(rec, flashCookieName); c == nil || c.MaxAge != -1 {
		t.Error("flash cookie was not cleared after display")
	}

	// Setup now bounces to the admin root.
	rec = doRequest(t, h, http.MethodGet, "/setup", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/" {
		t.Fatalf("GET /setup post-setup: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}

	// Wrong password: back to the login page with an alert.
	rec = doRequest(t, h, http.MethodPost, "/session", url.Values{
		"user_name": {"admin"}, "password": {"wrong"},
	})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
		t.Fatalf("bad login: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if c := findCookie(rec, sessionCookieName); c != nil && c.Value != "" {
		t.Error("failed login set a session cookie")
	}
	rec = doRequest(t, h, http.MethodGet, "/session/new", nil, findCookie(rec, flashCookieName))
	if !strings.Contains(rec.Body.String(), "Try another username or password.") {
		t.Error("login page does not show the bad-credentials alert")
	}

	// Correct password (username normalized: stored lowercase).
	session := login(t, h, "ADMIN", "secret-pw")

	// Protected page renders with the session.
	rec = doRequest(t, h, http.MethodGet, "/users/1/edit", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /users/1/edit: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "Account Settings") {
		t.Error("account page did not render")
	}

	// Logout kills the session.
	rec = doRequest(t, h, http.MethodPost, "/session/destroy", nil, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("logout: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = doRequest(t, h, http.MethodGet, "/users/1/edit", nil, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
		t.Fatalf("protected page after logout: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestSessionRow checks the sessions row contents (ip, user_agent, token).
func TestSessionRow(t *testing.T) {
	s, h := newTestServer(t)
	completeSetup(t, h)

	form := url.Values{"user_name": {"admin"}, "password": {"secret-pw"}}
	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "test-agent")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	cookie := findCookie(rec, sessionCookieName)
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	if got := len(cookie.Value); got != 64 {
		t.Errorf("token length = %d, want 64 hex chars (32 bytes)", got)
	}
	sess, err := s.Q.GetSessionByToken(t.Context(), cookie.Value)
	if err != nil {
		t.Fatalf("session row: %v", err)
	}
	if sess.UserAgent.String != "test-agent" {
		t.Errorf("user_agent = %q, want test-agent", sess.UserAgent.String)
	}
	if sess.IpAddress.String != "192.0.2.1" {
		t.Errorf("ip_address = %q, want 192.0.2.1", sess.IpAddress.String)
	}
}

// TestChangePassword mirrors UsersController#update.
func TestChangePassword(t *testing.T) {
	s, h := newTestServer(t)
	completeSetup(t, h)
	session := login(t, h, "admin", "secret-pw")

	change := func(form url.Values) *httptest.ResponseRecorder {
		return doRequest(t, h, http.MethodPost, "/users/1", form, session)
	}

	// Password change without the current password is rejected (422).
	rec := change(url.Values{
		"user_name": {"admin"}, "current_password": {"wrong"},
		"password": {"new-pw"}, "password_confirmation": {"new-pw"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong current password: status = %d, want 422", rec.Code)
	}

	// Mismatched confirmation is rejected (422).
	rec = change(url.Values{
		"user_name": {"admin"}, "current_password": {"secret-pw"},
		"password": {"new-pw"}, "password_confirmation": {"other"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched confirmation: status = %d, want 422", rec.Code)
	}

	// Over-72-byte new password is rejected (422), mirroring the Rails
	// has_secure_password length validation instead of erroring in bcrypt.
	long := strings.Repeat("a", 100)
	rec = change(url.Values{
		"user_name": {"admin"}, "current_password": {"secret-pw"},
		"password": {long}, "password_confirmation": {long},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too-long password: status = %d, want 422", rec.Code)
	}

	// Valid change succeeds and the new password works.
	rec = change(url.Values{
		"user_name": {"admin"}, "current_password": {"secret-pw"},
		"password": {"new-pw"}, "password_confirmation": {"new-pw"},
	})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/posts" {
		t.Fatalf("valid change: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	user, err := s.Q.GetUserByUserName(t.Context(), "admin")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordDigest), []byte("new-pw")) != nil {
		t.Error("password digest was not updated")
	}

	// Blank new password keeps the old one (username-only update).
	rec = change(url.Values{"user_name": {"renamed"}, "current_password": {""}, "password": {""}, "password_confirmation": {""}})
	if rec.Code != http.StatusFound {
		t.Fatalf("username-only update: status = %d, want %d", rec.Code, http.StatusFound)
	}
	user, _ = s.Q.GetUserByUserName(t.Context(), "renamed")
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordDigest), []byte("new-pw")) != nil {
		t.Error("blank password changed the digest")
	}
}

// TestOriginCheck covers the CSRF-replacement middleware.
func TestOriginCheck(t *testing.T) {
	_, h := newTestServer(t)
	completeSetup(t, h)

	tests := []struct {
		name      string
		origin    string
		fetchSite string
		want      int
	}{
		{name: "no headers allowed (curl)", want: http.StatusFound},
		{name: "matching origin", origin: "http://example.com", want: http.StatusFound},
		{name: "matching origin with default port omitted", origin: "https://example.com", want: http.StatusFound},
		{name: "origin host mismatch", origin: "http://evil.com", want: http.StatusForbidden},
		{name: "origin port mismatch", origin: "http://example.com:9999", want: http.StatusForbidden},
		{name: "bad origin url", origin: "://nope", want: http.StatusForbidden},
		{name: "sec-fetch-site same-origin", fetchSite: "same-origin", want: http.StatusFound},
		{name: "sec-fetch-site same-site", fetchSite: "same-site", want: http.StatusFound},
		{name: "sec-fetch-site none", fetchSite: "none", want: http.StatusFound},
		{name: "sec-fetch-site cross-site", fetchSite: "cross-site", want: http.StatusForbidden},
		{name: "good origin beats cross-site fetch", origin: "http://example.com", fetchSite: "cross-site", want: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{"user_name": {"admin"}, "password": {"secret-pw"}}
			req := httptest.NewRequest(http.MethodPost, "http://example.com/session", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// TestSettingsRowWithoutSetupCompleted: settings row exists but
// setup_completed = 0 still redirects to /setup.
func TestSettingsRowWithoutSetupCompleted(t *testing.T) {
	s, h := newTestServer(t)
	now := time.Now().Unix()
	if _, err := s.Q.CreateUser(t.Context(), query.CreateUserParams{
		UserName: "existing", PasswordDigest: "x", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.Q.EnsureSettings(t.Context(), query.EnsureSettingsParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("GET /: status = %d location = %q, want 302 /setup", rec.Code, rec.Header().Get("Location"))
	}

	// Setup is still accepted and flips the flag.
	completeSetup(t, h)
	settings, err := s.Q.GetSettings(t.Context())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if settings.SetupCompleted != 1 {
		t.Errorf("setup_completed = %d, want 1", settings.SetupCompleted)
	}
	if settings.Title.String != "My Blog" {
		t.Errorf("title = %q, want My Blog", settings.Title.String)
	}
	rec = doRequest(t, h, http.MethodGet, "/", nil)
	if rec.Code == http.StatusFound && rec.Header().Get("Location") == "/setup" {
		t.Error("still redirects to /setup after completion")
	}
}

// TestSetupValidation covers the form validations of POST /setup.
func TestSetupValidation(t *testing.T) {
	_, h := newTestServer(t)

	tests := []struct {
		name string
		form url.Values
	}{
		{name: "password mismatch", form: with(setupForm, "password_confirmation", "other")},
		{name: "password too long", form: with(with(setupForm, "password", strings.Repeat("a", 100)), "password_confirmation", strings.Repeat("a", 100))},
		{name: "blank password", form: with(setupForm, "password", "")},
		{name: "blank username", form: with(setupForm, "user_name", "")},
		{name: "blank title", form: with(setupForm, "title", "")},
		{name: "blank url", form: with(setupForm, "url", "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, h, http.MethodPost, "/setup", tt.form)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422", rec.Code)
			}
		})
	}
}

// with returns a copy of the form with one value replaced.
func with(form url.Values, key, value string) url.Values {
	out := make(url.Values, len(form))
	for k, v := range form {
		out[k] = append([]string(nil), v...)
	}
	out.Set(key, value)
	return out
}
