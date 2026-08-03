package httpd

import (
	"encoding/json"
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
	"rables/internal/templates"
)

// newSourcesTestServer builds a Server with the sources route on a test-local
// chi router.
func newSourcesTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterSourcesRoutes(r, s)
	return s, r
}

// fakeOEmbed points the oEmbed endpoint at an httptest server running handler.
func fakeOEmbed(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	saved := twitterOEmbedEndpoint
	twitterOEmbedEndpoint = srv.URL
	t.Cleanup(func() { twitterOEmbedEndpoint = saved })
	return srv
}

// postFetchTwitter posts a JSON {"url": ...} body like the source-reference
// Stimulus controller.
func postFetchTwitter(t *testing.T, h http.Handler, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/sources/fetch_twitter", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeJSONBody parses a JSON response into a map.
func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return data
}

// TestAdminFetchTwitterAuth: the route sits behind RequireAuth.
func TestAdminFetchTwitterAuth(t *testing.T) {
	_, h := newSourcesTestServer(t)
	rec := postFetchTwitter(t, h, `{"url":"https://x.com/u/status/1"}`)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
		t.Errorf("POST unauthenticated: status = %d location = %q, want 302 /session/new",
			rec.Code, rec.Header().Get("Location"))
	}
}

// TestAdminFetchTwitterSuccess: author/content backfilled from the oEmbed
// response, with only the <p> text extracted.
func TestAdminFetchTwitterSuccess(t *testing.T) {
	s, h := newSourcesTestServer(t)
	session := redirectsSessionCookie(t, s)

	var gotQuery url.Values
	fakeOEmbed(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"author_name": "Jane Doe",
			"html": "<blockquote class=\"twitter-tweet\"><p lang=\"en\" dir=\"ltr\">Hello &amp; goodbye</p>&mdash; Jane Doe (@jane) <a href=\"https://x.com/jane/status/1\">May 1</a></blockquote>"
		}`)
	})

	rec := postFetchTwitter(t, h, `{"url":"https://x.com/jane/status/1"}`, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch_twitter: status = %d body = %q", rec.Code, rec.Body.String())
	}
	data := decodeJSONBody(t, rec)
	if data["success"] != true || data["author"] != "Jane Doe" || data["content"] != "Hello & goodbye" {
		t.Errorf("fetch_twitter: got %v", data)
	}
	// The oEmbed request carries the tweet URL plus omit_script/dnt, like the
	// Ruby URI.encode_www_form query.
	if got := gotQuery.Get("url"); got != "https://x.com/jane/status/1" {
		t.Errorf("oEmbed url param = %q", got)
	}
	if gotQuery.Get("omit_script") != "true" || gotQuery.Get("dnt") != "true" {
		t.Errorf("oEmbed omit_script/dnt = %q/%q", gotQuery.Get("omit_script"), gotQuery.Get("dnt"))
	}
}

// TestAdminFetchTwitterValidation mirrors the 422 branches of the Rails
// controller (blank url, non-Twitter host). The oEmbed stub returns a valid
// payload, so a 422 proves the request never reached it.
func TestAdminFetchTwitterValidation(t *testing.T) {
	s, h := newSourcesTestServer(t)
	session := redirectsSessionCookie(t, s)
	fakeOEmbed(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"author_name":"Jane","html":"<p>hi</p>"}`)
	})

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"blank url", `{"url":""}`, "URL is required"},
		{"missing url", `{}`, "URL is required"},
		{"non-twitter host", `{"url":"https://example.com/tweet/1"}`, "Not a valid Twitter/X URL"},
		{"twitter lookalike", `{"url":"https://x.com.evil.com/1"}`, "Not a valid Twitter/X URL"},
		{"not a url", `{"url":"not a url"}`, "Not a valid Twitter/X URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postFetchTwitter(t, h, tt.body, session)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", rec.Code)
			}
			if data := decodeJSONBody(t, rec); data["error"] != tt.wantErr {
				t.Errorf("error = %v, want %q", data["error"], tt.wantErr)
			}
		})
	}
}

// TestAdminFetchTwitterFormParam: params[:url] also comes from a form post,
// like the Rails controller.
func TestAdminFetchTwitterFormParam(t *testing.T) {
	s, h := newSourcesTestServer(t)
	session := redirectsSessionCookie(t, s)
	fakeOEmbed(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"author_name":"Jane","html":"<p>hi</p>"}`)
	})

	rec := doRequest(t, h, http.MethodPost, "/admin/sources/fetch_twitter",
		url.Values{"url": {"https://twitter.com/u/status/1"}}, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	if data := decodeJSONBody(t, rec); data["success"] != true || data["author"] != "Jane" || data["content"] != "hi" {
		t.Errorf("got %v", data)
	}
}

// TestAdminFetchTwitterOEmbedFailure: any oEmbed failure (non-2xx, timeout,
// bad JSON) maps to 503 like the Ruby rescue → nil path.
func TestAdminFetchTwitterOEmbedFailure(t *testing.T) {
	s, h := newSourcesTestServer(t)
	session := redirectsSessionCookie(t, s)

	tests := []struct {
		name    string
		handler http.HandlerFunc
		timeout time.Duration
	}{
		{"oembed 404", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }, 0},
		{"oembed 500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, 0},
		{"bad json", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{oops`) }, 0},
		{"timeout", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, 100 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeOEmbed(t, tt.handler)
			if tt.timeout > 0 {
				saved := oEmbedTimeout
				oEmbedTimeout = tt.timeout
				t.Cleanup(func() { oEmbedTimeout = saved })
			}
			rec := postFetchTwitter(t, h, `{"url":"https://x.com/u/status/1"}`, session)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if data := decodeJSONBody(t, rec); data["error"] != "Failed to fetch tweet content" {
				t.Errorf("error = %v", data["error"])
			}
		})
	}
}

// TestIsTwitterURL mirrors twitter_url? host matching.
func TestIsTwitterURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://twitter.com/u/status/1", true},
		{"https://www.twitter.com/u/status/1", true},
		{"https://x.com/u/status/1", true},
		{"https://www.x.com/u/status/1", true},
		{"https://X.COM/u/status/1", true},
		{"https://x.com:443/u/status/1", true},
		{"https://example.com/x.com", false},
		{"https://notx.com/u/1", false},
		{"https://x.com.evil.com/u/1", false},
		{"x.com/u/status/1", false},
		{"", false},
		{"http://[::1", false},
	}
	for _, tt := range tests {
		if got := isTwitterURL(tt.url); got != tt.want {
			t.Errorf("isTwitterURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

// TestTweetContent: <p> texts joined with spaces, stripped, capped at 250
// characters (TwitterOembedService::MAX_CONTENT_LENGTH).
func TestTweetContent(t *testing.T) {
	long := strings.Repeat("a", 300)
	tests := []struct {
		name     string
		fragment string
		want     string
	}{
		{"empty", "", ""},
		{"no paragraph", `<blockquote>&mdash; A (@a)</blockquote>`, ""},
		{"single paragraph", `<p>Hello <b>bold</b> world</p>`, "Hello bold world"},
		{"entities decoded", `<p>fish &amp; chips</p>`, "fish & chips"},
		{"paragraphs joined", `<p>one</p><p>two</p>`, "one two"},
		{"stripped", `<p>  padded  </p>`, "padded"},
		{"truncated at 250 chars", `<p>` + long + `</p>`, long[:250]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tweetContent(tt.fragment); got != tt.want {
				t.Errorf("tweetContent(%q) = %q, want %q", tt.fragment, got, tt.want)
			}
		})
	}
}
