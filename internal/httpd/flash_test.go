package httpd

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"rables/internal/config"
	"rables/internal/templates"
)

// signedFlashCookie runs SetFlash and returns the cookie it emitted, so tests
// can replay a properly signed flash on the next request.
func signedFlashCookie(t *testing.T, s *Server, flash templates.Flash) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	s.SetFlash(rec, flash)
	c := findCookie(rec, flashCookieName)
	if c == nil {
		t.Fatal("SetFlash did not set a flash cookie")
	}
	return c
}

// popFlash replays cookies through PopFlash, returning the flash and the
// recorder holding the clearing Set-Cookie.
func popFlash(s *Server, cookies ...*http.Cookie) (templates.Flash, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	return s.PopFlash(req, rec), rec
}

func TestFlashRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	want := templates.Flash{Notice: "saved", Alert: "careful"}
	flash, rec := popFlash(s, signedFlashCookie(t, s, want))
	if flash != want {
		t.Errorf("flash = %+v, want %+v", flash, want)
	}
	if cleared := findCookie(rec, flashCookieName); cleared == nil || cleared.Value != "" {
		t.Errorf("flash cookie not cleared: %+v", cleared)
	}
}

// TestCacheBustTarget: the redirect target gains the "_" cache-busting query
// parameter (see redirectWithFlash), joined with "&" when a query exists.
func TestCacheBustTarget(t *testing.T) {
	plain := cacheBustTarget("/hello-world")
	raw, ok := strings.CutPrefix(plain, "/hello-world?_=")
	if !ok {
		t.Fatalf("cacheBustTarget = %q, want /hello-world?_=...", plain)
	}
	if _, err := strconv.ParseInt(raw, 36, 64); err != nil {
		t.Errorf("parameter value %q is not a base36 timestamp: %v", raw, err)
	}
	if withQuery := cacheBustTarget("/x?a=b"); !strings.HasPrefix(withQuery, "/x?a=b&_=") {
		t.Errorf("cacheBustTarget with query = %q, want /x?a=b&_=...", withQuery)
	}
}

// TestFlashTampered: a planted flash cookie (cookie tossing, plain-HTTP
// MitM) must never render — PopFlash treats a missing or bad signature like
// a missing cookie, yet still emits the clearing Set-Cookie.
func TestFlashTampered(t *testing.T) {
	s, _ := newTestServer(t)
	valid := signedFlashCookie(t, s, templates.Flash{Notice: "hello"})
	body, sig, _ := strings.Cut(valid.Value, ".")

	forged, err := json.Marshal(templates.Flash{Alert: "phishing"})
	if err != nil {
		t.Fatalf("marshal flash: %v", err)
	}
	enc := base64.RawURLEncoding
	other := &Server{Cfg: config.Config{HMACSecret: "other-secret"}}
	wrongSecret := signedFlashCookie(t, other, templates.Flash{Alert: "phishing"})

	values := map[string]string{
		"unsigned payload":   enc.EncodeToString(forged),
		"tampered payload":   enc.EncodeToString(forged) + "." + sig,
		"tampered signature": body + "." + enc.EncodeToString([]byte("forged-signature")),
		"wrong secret":       wrongSecret.Value,
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			flash, rec := popFlash(s, &http.Cookie{Name: flashCookieName, Value: value})
			if flash != (templates.Flash{}) {
				t.Errorf("flash = %+v, want zero Flash", flash)
			}
			if cleared := findCookie(rec, flashCookieName); cleared == nil || cleared.Value != "" {
				t.Errorf("flash cookie not cleared: %+v", cleared)
			}
		})
	}
}
