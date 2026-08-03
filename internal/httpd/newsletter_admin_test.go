package httpd

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/templates"
)

// newNewsletterTestServer builds a Server and mounts only the newsletter
// routes, independent of the integrator's router wiring.
func newNewsletterTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterNewsletterRoutes(r, s)
	return s, r
}

// newsletterJSON posts a JSON body like the Rails Stimulus verify controller.
func newsletterJSON(t *testing.T, h http.Handler, payload string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/newsletter/verify", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestNewsletterRequiresAuth: the admin newsletter pages bounce anonymous users.
func TestNewsletterRequiresAuth(t *testing.T) {
	_, h := newNewsletterTestServer(t)

	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/admin/newsletter"},
		{http.MethodPost, "/admin/newsletter"},
		{http.MethodPost, "/admin/newsletter/verify"},
	} {
		rec := doRequest(t, h, req.method, req.path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("%s %s: status = %d location = %q, want 302 /session/new",
				req.method, req.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestNewsletterShowDefaults: GET renders the tabs and the native defaults.
func TestNewsletterShowDefaults(t *testing.T) {
	s, h := newNewsletterTestServer(t)
	session := settingsSession(t, s)

	rec := doRequest(t, h, http.MethodGet, "/admin/newsletter", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET show: status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, marker := range []string{"tab=general", "tab=native", "tab=listmonk"} {
		if !strings.Contains(body, marker) {
			t.Errorf("show page missing %q", marker)
		}
	}
	if !strings.Contains(body, `<option value="native" selected>`) {
		t.Error("general tab does not default provider to native")
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/newsletter?tab=native", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET show native: status = %d", rec.Code)
	}
	body = rec.Body.String()
	// Fresh row: port defaults to 587 and STARTTLS is checked (NULL means on).
	if !strings.Contains(body, `name="newsletter_setting[smtp_port]" value="587"`) {
		t.Error("native tab does not default smtp_port to 587")
	}
	if !strings.Contains(body, `name="newsletter_setting[smtp_enable_starttls]" value="1" checked`) {
		t.Error("native tab does not default STARTTLS to checked")
	}
}

// TestNewsletterUpdateRoundtrip: settings save, read back, and the general
// tab's partial update preserves the SMTP columns (Rails partial permit).
func TestNewsletterUpdateRoundtrip(t *testing.T) {
	s, h := newNewsletterTestServer(t)
	session := settingsSession(t, s)
	ctx := t.Context()

	nativeForm := url.Values{
		"tab":                                      {"native"},
		"newsletter_setting[smtp_address]":         {"smtp.example.com"},
		"newsletter_setting[smtp_port]":            {"2525"},
		"newsletter_setting[smtp_user_name]":       {"mailer"},
		"newsletter_setting[smtp_password]":        {"secret-pw"},
		"newsletter_setting[smtp_domain]":          {"helo.example.com"},
		"newsletter_setting[smtp_authentication]":  {"login"},
		"newsletter_setting[smtp_enable_starttls]": {"0", "1"},
		"newsletter_setting[from_email]":           {"news@example.com"},
	}
	rec := doRequest(t, h, http.MethodPost, "/admin/newsletter", nativeForm, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/newsletter?tab=native" {
		t.Fatalf("POST native: status = %d location = %q, want 302 tab=native", rec.Code, rec.Header().Get("Location"))
	}

	st, err := s.NewsletterSetting(ctx)
	if err != nil {
		t.Fatalf("load setting: %v", err)
	}
	if st.SmtpAddress.String != "smtp.example.com" || st.SmtpPort.Int64 != 2525 ||
		st.SmtpUserName.String != "mailer" || st.SmtpPassword.String != "secret-pw" ||
		st.SmtpDomain.String != "helo.example.com" || st.SmtpAuthentication.String != "login" ||
		!st.SmtpEnableStarttls.Valid || st.SmtpEnableStarttls.Int64 != 1 ||
		st.FromEmail.String != "news@example.com" {
		t.Errorf("saved setting = %+v", st)
	}

	// The general tab submits only enabled/provider; the SMTP columns survive.
	generalForm := url.Values{
		"tab":                          {"general"},
		"newsletter_setting[enabled]":  {"0", "1"},
		"newsletter_setting[provider]": {"native"},
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/newsletter", generalForm, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/newsletter?tab=general" {
		t.Fatalf("POST general: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	st, err = s.NewsletterSetting(ctx)
	if err != nil {
		t.Fatalf("reload setting: %v", err)
	}
	if st.Enabled != 1 || st.Provider != "native" {
		t.Errorf("general tab update: enabled = %d provider = %q", st.Enabled, st.Provider)
	}
	if st.SmtpAddress.String != "smtp.example.com" || st.SmtpPassword.String != "secret-pw" {
		t.Error("general tab update wiped SMTP columns")
	}
}

// TestNewsletterPasswordPlaceholder: the masked password placeholder keeps
// the stored password, a real value replaces it.
func TestNewsletterPasswordPlaceholder(t *testing.T) {
	s, h := newNewsletterTestServer(t)
	session := settingsSession(t, s)
	ctx := t.Context()

	form := func(password string) url.Values {
		return url.Values{
			"tab":                                {"native"},
			"newsletter_setting[smtp_address]":   {"smtp.example.com"},
			"newsletter_setting[smtp_port]":      {"587"},
			"newsletter_setting[smtp_user_name]": {"mailer"},
			"newsletter_setting[smtp_password]":  {password},
			"newsletter_setting[from_email]":     {"news@example.com"},
		}
	}

	if rec := doRequest(t, h, http.MethodPost, "/admin/newsletter", form("first-pw"), session); rec.Code != http.StatusFound {
		t.Fatalf("seed password: status = %d", rec.Code)
	}
	for _, keep := range []string{"••••••••", ""} {
		if rec := doRequest(t, h, http.MethodPost, "/admin/newsletter", form(keep), session); rec.Code != http.StatusFound {
			t.Fatalf("placeholder %q: status = %d", keep, rec.Code)
		}
		st, err := s.NewsletterSetting(ctx)
		if err != nil {
			t.Fatalf("load setting: %v", err)
		}
		if st.SmtpPassword.String != "first-pw" {
			t.Errorf("placeholder %q: password = %q, want first-pw", keep, st.SmtpPassword.String)
		}
	}
	if rec := doRequest(t, h, http.MethodPost, "/admin/newsletter", form("second-pw"), session); rec.Code != http.StatusFound {
		t.Fatalf("replace password: status = %d", rec.Code)
	}
	st, _ := s.NewsletterSetting(ctx)
	if st.SmtpPassword.String != "second-pw" {
		t.Errorf("replaced password = %q, want second-pw", st.SmtpPassword.String)
	}
}

// TestNewsletterUpdateValidation: model validation failures re-render with
// 422 and the Rails error messages.
func TestNewsletterUpdateValidation(t *testing.T) {
	s, h := newNewsletterTestServer(t)
	session := settingsSession(t, s)

	form := url.Values{
		"tab":                          {"general"},
		"newsletter_setting[enabled]":  {"0", "1"},
		"newsletter_setting[provider]": {"native"},
	}
	rec := doRequest(t, h, http.MethodPost, "/admin/newsletter", form, session)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank from_email: status = %d, want 422", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "From email can&#39;t be blank") ||
		!strings.Contains(body, "From email is invalid") {
		t.Error("blank from_email should fail presence and format")
	}

	form.Set("newsletter_setting[provider]", "bogus")
	rec = doRequest(t, h, http.MethodPost, "/admin/newsletter", form, session)
	if rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Provider is not included in the list") {
		t.Error("bogus provider should fail inclusion")
	}
}

// TestNewsletterListmonkRoundtrip: the listmonk form saves and validates.
func TestNewsletterListmonkRoundtrip(t *testing.T) {
	s, h := newNewsletterTestServer(t)
	session := settingsSession(t, s)
	ctx := t.Context()

	form := url.Values{
		"tab":                   {"listmonk"},
		"listmonk[url]":         {"https://listmonk.example.com"},
		"listmonk[username]":    {"admin"},
		"listmonk[api_key]":     {"token"},
		"listmonk[list_id]":     {"3"},
		"listmonk[template_id]": {"7"},
	}
	rec := doRequest(t, h, http.MethodPost, "/admin/newsletter", form, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/newsletter?tab=listmonk" {
		t.Fatalf("POST listmonk: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	lm, err := s.ListmonkConfig(ctx)
	if err != nil {
		t.Fatalf("load listmonk: %v", err)
	}
	if lm.Url.String != "https://listmonk.example.com" || lm.Username.String != "admin" ||
		lm.ApiKey.String != "token" || lm.ListID.Int64 != 3 || lm.TemplateID.Int64 != 7 {
		t.Errorf("saved listmonk = %+v", lm)
	}

	form.Set("listmonk[url]", "")
	rec = doRequest(t, h, http.MethodPost, "/admin/newsletter", form, session)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank url: status = %d, want 422", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Url can&#39;t be blank") || !strings.Contains(body, "Url 格式无效") {
		t.Error("blank url should fail presence and format")
	}
}

// TestNewsletterVerifyListmonk exercises POST /admin/newsletter/verify in the
// listmonk branch against a fake listmonk HTTP API.
func TestNewsletterVerifyListmonk(t *testing.T) {
	s, h := newNewsletterTestServer(t)
	session := settingsSession(t, s)

	listmonk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, key, ok := r.BasicAuth()
		if !ok || user != "admin" || key != "token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Invalid API credentials"}`)
			return
		}
		switch r.URL.Path {
		case "/api/lists":
			fmt.Fprint(w, `{"data":{"results":[{"id":3,"name":"Newsletter"}],"total":1}}`)
		case "/api/templates":
			fmt.Fprint(w, `{"data":[{"id":7,"name":"Default"}]}`)
		}
	}))
	defer listmonk.Close()

	// Missing fields are rejected before any HTTP call.
	rec := doRequest(t, h, http.MethodPost, "/admin/newsletter/verify", url.Values{"url": {listmonk.URL}}, session)
	if rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Please configure all required fields first") {
		t.Errorf("unconfigured: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Happy path over a JSON body, like the Rails Stimulus controller posts.
	rec = newsletterJSON(t, h, fmt.Sprintf(
		`{"url":%q,"username":"admin","api_key":"token","list_id":"3","template_id":"7"}`, listmonk.URL), session)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify: status = %d body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, marker := range []string{
		`"success":true`, `"id":3`, `"Newsletter"`, `"id":7`, `"Default"`,
		`"current_list_id":3`, `"current_template_id":7`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("verify body missing %q: %s", marker, body)
		}
	}

	// Bad credentials surface the listmonk error (last_error semantics: the
	// templates call fails last and its message wins).
	rec = newsletterJSON(t, h, fmt.Sprintf(
		`{"url":%q,"username":"admin","api_key":"wrong"}`, listmonk.URL), session)
	if rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Fetch Template failed! 401") {
		t.Errorf("bad credentials: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Reachable but empty listmonk fails the presence check like Rails.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/lists":
			fmt.Fprint(w, `{"data":{"results":[],"total":0}}`)
		case "/api/templates":
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	defer empty.Close()
	rec = newsletterJSON(t, h, fmt.Sprintf(
		`{"url":%q,"username":"admin","api_key":"token"}`, empty.URL), session)
	if rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Failed to fetch lists or templates") {
		t.Errorf("empty listmonk: status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// fakeVerifySMTPServer is the minimal SMTP server for the verify endpoint
// tests (plan T18 DoD: verify against a fake SMTP server).
type fakeVerifySMTPServer struct {
	addr     string
	ln       net.Listener
	failAuth bool

	mu    sync.Mutex
	auths [][2]string
}

func newFakeVerifySMTPServer(t *testing.T, failAuth bool) *fakeVerifySMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeVerifySMTPServer{addr: ln.Addr().String(), ln: ln, failAuth: failAuth}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handle(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return srv
}

func (s *fakeVerifySMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)
	reply := func(line string) { fmt.Fprint(w, line+"\r\n"); w.Flush() }
	reply("220 fake ESMTP ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			reply("250-fake\r\n250-AUTH PLAIN LOGIN\r\n250 8BITMIME")
		case "AUTH":
			// PLAIN carries an inline initial response in these tests.
			parts := strings.Split(decodeBase64(arg[len("PLAIN "):]), "\x00")
			user, pass := "", ""
			if len(parts) == 3 {
				user, pass = parts[1], parts[2]
			}
			s.mu.Lock()
			s.auths = append(s.auths, [2]string{user, pass})
			s.mu.Unlock()
			if s.failAuth {
				reply("535 5.7.8 authentication failed")
			} else {
				reply("235 2.7.0 authentication succeeded")
			}
		case "QUIT":
			reply("221 Bye")
			return
		default:
			reply("250 OK")
		}
	}
}

func decodeBase64(v string) string {
	b, _ := base64.StdEncoding.DecodeString(v)
	return string(b)
}

func (s *fakeVerifySMTPServer) hostPort() (string, string) {
	host, port, _ := net.SplitHostPort(s.addr)
	return host, port
}

// TestNewsletterVerifySMTP exercises the native branch of the verify endpoint.
func TestNewsletterVerifySMTP(t *testing.T) {
	s, h := newNewsletterTestServer(t)
	session := settingsSession(t, s)

	srv := newFakeVerifySMTPServer(t, false)
	host, port := srv.hostPort()

	form := func(overrides map[string]string) url.Values {
		v := url.Values{
			"smtp_address":         {host},
			"smtp_port":            {port},
			"smtp_user_name":       {"mailer"},
			"smtp_password":        {"secret-pw"},
			"smtp_enable_starttls": {"0"},
			"from_email":           {"news@example.com"},
		}
		for k, val := range overrides {
			v.Set(k, val)
		}
		return v
	}

	// Missing fields are rejected before dialing.
	rec := doRequest(t, h, http.MethodPost, "/admin/newsletter/verify", form(map[string]string{"from_email": ""}), session)
	if rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Please fill in all required fields") {
		t.Errorf("missing fields: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Successful handshake + auth.
	rec = doRequest(t, h, http.MethodPost, "/admin/newsletter/verify", form(nil), session)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"success":true`) ||
		!strings.Contains(rec.Body.String(), "SMTP configuration verified successfully!") {
		t.Errorf("verify ok: status = %d body = %s", rec.Code, rec.Body.String())
	}
	srv.mu.Lock()
	if len(srv.auths) != 1 || srv.auths[0] != [2]string{"mailer", "secret-pw"} {
		t.Errorf("captured auths = %v", srv.auths)
	}
	srv.mu.Unlock()

	// Auth failure maps to the Rails message.
	badSrv := newFakeVerifySMTPServer(t, true)
	badHost, badPort := badSrv.hostPort()
	rec = doRequest(t, h, http.MethodPost, "/admin/newsletter/verify",
		form(map[string]string{"smtp_address": badHost, "smtp_port": badPort}), session)
	if rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Authentication failed: Invalid credentials") {
		t.Errorf("auth failure: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// A refused connection maps to the Rails message.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadHost, deadPort, _ := net.SplitHostPort(dead.Addr().String())
	dead.Close()
	rec = doRequest(t, h, http.MethodPost, "/admin/newsletter/verify",
		form(map[string]string{"smtp_address": deadHost, "smtp_port": deadPort}), session)
	if rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Connection refused") {
		t.Errorf("refused: status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// TestNewsletterVerifySMTPPasswordFallback: the placeholder password falls
// back to the stored one before dialing.
func TestNewsletterVerifySMTPPasswordFallback(t *testing.T) {
	s, h := newNewsletterTestServer(t)
	session := settingsSession(t, s)
	ctx := t.Context()

	now := time.Now().Unix()
	if err := s.Q.EnsureNewsletterSetting(ctx, query.EnsureNewsletterSettingParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("ensure newsletter setting: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		"UPDATE newsletter_settings SET smtp_password = 'stored-pw' WHERE id = 1"); err != nil {
		t.Fatalf("store password: %v", err)
	}

	srv := newFakeVerifySMTPServer(t, false)
	host, port := srv.hostPort()
	rec := newsletterJSON(t, h, fmt.Sprintf(
		`{"smtp_address":%q,"smtp_port":%q,"smtp_user_name":"mailer","smtp_password":"••••••••","smtp_enable_starttls":"0","from_email":"news@example.com"}`,
		host, port), session)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify with placeholder: status = %d body = %s", rec.Code, rec.Body.String())
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.auths) != 1 || srv.auths[0] != [2]string{"mailer", "stored-pw"} {
		t.Errorf("captured auths = %v, want mailer/stored-pw", srv.auths)
	}
}
