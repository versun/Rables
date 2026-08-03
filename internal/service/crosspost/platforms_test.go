package crosspost

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"rables/internal/db/query"
)

func mastodonCfg(serverURL, token string) query.Crosspost {
	cfg := query.Crosspost{Platform: "mastodon", Enabled: 1}
	cfg.ServerUrl = nullStringForTest(serverURL)
	cfg.AccessToken = nullStringForTest(token)
	return cfg
}

func nullStringForTest(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

// fakeMastodon records the requests the platform client makes.
type fakeMastodon struct {
	mu sync.Mutex

	statusForm  map[string][]string
	statusAuth  string
	mediaNames  []string
	mediaTypes  []string
	mediaCalls  int
	verifiedTok string

	statusCode int // override for /api/v1/statuses; 0 → 200
	mediaCode  int // override for /api/v2/media; 0 → 200
	rateReset  string
}

func (f *fakeMastodon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/accounts/verify_credentials", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.verifiedTok = r.Header.Get("Authorization")
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer good-token" {
			http.Error(w, `{"error":"invalid"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"acct":"user"}`))
	})
	mux.HandleFunc("/api/v2/media", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.mediaCalls++
		f.mu.Unlock()
		if f.mediaCode != 0 {
			if f.rateReset != "" {
				w.Header().Set("X-RateLimit-Reset", f.rateReset)
			}
			w.WriteHeader(f.mediaCode)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		_ = file.Close()
		f.mu.Lock()
		f.mediaNames = append(f.mediaNames, header.Filename)
		f.mediaTypes = append(f.mediaTypes, header.Header.Get("Content-Type"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"m%d"}`, f.mediaCalls)
	})
	mux.HandleFunc("/api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.statusAuth = r.Header.Get("Authorization")
		f.mu.Unlock()
		if f.statusCode != 0 {
			if f.rateReset != "" {
				w.Header().Set("X-RateLimit-Reset", f.rateReset)
			}
			w.WriteHeader(f.statusCode)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.statusForm = r.PostForm
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://mastodon.example/@user/12345"}`))
	})
	return mux
}

func newFakeMastodon(t *testing.T) (*fakeMastodon, *httptest.Server) {
	t.Helper()
	f := &fakeMastodon{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, srv
}

func TestMastodonVerify(t *testing.T) {
	f, srv := newFakeMastodon(t)
	p := mastodonPlatform{client: srv.Client()}

	if err := p.Verify(t.Context(), mastodonCfg(srv.URL, "good-token")); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if f.verifiedTok != "Bearer good-token" {
		t.Errorf("authorization = %q", f.verifiedTok)
	}

	err := p.Verify(t.Context(), mastodonCfg(srv.URL, "bad-token"))
	if err == nil || !strings.Contains(err.Error(), "Verification failed: 401") {
		t.Errorf("bad token error = %v", err)
	}

	err = p.Verify(t.Context(), mastodonCfg(srv.URL, ""))
	if err == nil || err.Error() != "Access token are required" {
		t.Errorf("blank token error = %v", err)
	}

	err = p.Verify(t.Context(), mastodonCfg("::bad", "x"))
	if err == nil || err.Error() != "Server URL must be a valid http(s) URL" {
		t.Errorf("bad url error = %v", err)
	}
}

func TestMastodonPost(t *testing.T) {
	f, srv := newFakeMastodon(t)
	p := mastodonPlatform{client: srv.Client()}

	in := PostInput{
		Text: "Hello Mastodon",
		Images: []Image{
			{Filename: "a.png", ContentType: "image/png", Data: []byte("pngdata")},
			{Filename: "b.jpg", ContentType: "image/jpeg", Data: []byte("jpgdata")},
		},
	}
	postURL, err := p.Post(t.Context(), mastodonCfg(srv.URL, "tok"), in)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if postURL != "https://mastodon.example/@user/12345" {
		t.Errorf("post url = %q", postURL)
	}
	if f.mediaCalls != 2 {
		t.Fatalf("media uploads = %d, want 2", f.mediaCalls)
	}
	if f.mediaNames[0] != "a.png" || f.mediaTypes[0] != "image/png" {
		t.Errorf("first upload = %q %q", f.mediaNames[0], f.mediaTypes[0])
	}
	form := f.statusForm
	if got := form["status"]; len(got) != 1 || got[0] != "Hello Mastodon" {
		t.Errorf("status field = %v", got)
	}
	if got := form["visibility"]; len(got) != 1 || got[0] != "public" {
		t.Errorf("visibility field = %v", got)
	}
	if got := form["media_ids[]"]; len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Errorf("media_ids[] = %v", got)
	}
	if f.statusAuth != "Bearer tok" {
		t.Errorf("authorization = %q", f.statusAuth)
	}
}

func TestMastodonPostRateLimited(t *testing.T) {
	f, srv := newFakeMastodon(t)
	f.statusCode = http.StatusTooManyRequests
	f.rateReset = fmt.Sprintf("%d", time.Now().Add(5*time.Minute).Unix())
	p := mastodonPlatform{client: srv.Client()}

	_, err := p.Post(t.Context(), mastodonCfg(srv.URL, "tok"), PostInput{Text: "x"})
	if err == nil || !IsTransient(err) {
		t.Fatalf("429 error = %v, want transient (429 单独退避)", err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %v, want the 429 context", err)
	}
}

func TestMastodonPostServerErrorIsPermanent(t *testing.T) {
	f, srv := newFakeMastodon(t)
	f.statusCode = http.StatusInternalServerError
	p := mastodonPlatform{client: srv.Client()}

	// Rails MastodonService#post only retries raised network errors; an HTTP
	// 500 response is logged and swallowed (nil), so only 429 is transient.
	_, err := p.Post(t.Context(), mastodonCfg(srv.URL, "tok"), PostInput{Text: "x"})
	if err == nil || IsTransient(err) {
		t.Fatalf("500 error = %v, want permanent", err)
	}
}

func TestMastodonMedia429AbortsPost(t *testing.T) {
	f, srv := newFakeMastodon(t)
	f.mediaCode = http.StatusTooManyRequests
	f.rateReset = fmt.Sprintf("%d", time.Now().Add(time.Minute).Unix())
	p := mastodonPlatform{client: srv.Client()}

	in := PostInput{Text: "x", Images: []Image{{Filename: "a.png", ContentType: "image/png", Data: []byte("d")}}}
	_, err := p.Post(t.Context(), mastodonCfg(srv.URL, "tok"), in)
	if err == nil || !IsTransient(err) {
		t.Fatalf("media 429 error = %v, want transient", err)
	}
	if f.statusForm != nil {
		t.Error("statuses was called despite the media upload rate limit")
	}
}

func TestMastodonAPIURL(t *testing.T) {
	tests := []struct {
		server   string
		endpoint string
		want     string
		wantErr  bool
	}{
		{"https://mastodon.social", "/api/v1/statuses", "https://mastodon.social/api/v1/statuses", false},
		{"https://mastodon.social/", "/api/v1/statuses", "https://mastodon.social/api/v1/statuses", false},
		{"https://m.c:443", "/api/v2/media", "https://m.c/api/v2/media", false},
		{"http://localhost:3000", "/api/v1/statuses", "http://localhost:3000/api/v1/statuses", false},
		{"https://m.c/instance/", "/api/v1/statuses", "https://m.c/instance/api/v1/statuses", false},
		{"  https://m.c  ", "/api/v1/statuses", "https://m.c/api/v1/statuses", false},
		{"", "/api/v1/statuses", "", true},
		{"ftp://m.c", "/api/v1/statuses", "", true},
		{"https://user:pw@m.c", "/api/v1/statuses", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.server, func(t *testing.T) {
			got, err := mastodonAPIURL(tt.server, tt.endpoint)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("mastodonAPIURL(%q) = %q, want error", tt.server, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mastodonAPIURL(%q): %v", tt.server, err)
			}
			if got != tt.want {
				t.Errorf("mastodonAPIURL(%q) = %q, want %q", tt.server, got, tt.want)
			}
		})
	}
}

func TestParseMastodonRateLimit(t *testing.T) {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "300")
	h.Set("X-RateLimit-Remaining", "7")
	h.Set("X-RateLimit-Reset", "2000000000")
	rl := parseMastodonRateLimit(h)
	if rl.Limit != 300 || rl.Remaining != 7 || !rl.HasReset || rl.ResetAt.Unix() != 2000000000 {
		t.Errorf("parse = %+v", rl)
	}
	if rl := parseMastodonRateLimit(http.Header{}); rl.Remaining != -1 || rl.HasReset {
		t.Errorf("empty headers = %+v, want absent markers", rl)
	}
}

// --- bluesky ---

func fakeJWT(exp time.Time) string {
	payload := fmt.Sprintf(`{"exp":%d}`, exp.Unix())
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".s"
}

type fakeBluesky struct {
	mu sync.Mutex

	createSessionCalls int
	refreshCalls       int
	createRecordAuth   []string
	createRecordBody   []map[string]any
	blobBodies         [][]byte
	blobTypes          []string

	sessionExp   time.Time // exp claim of tokens issued by createSession
	refreshedExp time.Time
	refreshFails bool
	recordCode   int // override for createRecord; 0 → 200
}

func (f *fakeBluesky) handler() http.Handler {
	write := func(w http.ResponseWriter, v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := json.Marshal(v)
		_, _ = w.Write(raw)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.createSessionCalls++
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			http.Error(w, `{"error":"unexpected auth"}`, http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["identifier"] != "handle.test" || body["password"] != "app-pw" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"InvalidLogin"}`))
			return
		}
		write(w, map[string]any{
			"accessJwt":  fakeJWT(f.sessionExp),
			"refreshJwt": "refresh-1",
			"did":        "did:plc:test",
		})
	})
	mux.HandleFunc("/com.atproto.server.refreshSession", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.refreshCalls++
		f.mu.Unlock()
		if f.refreshFails {
			w.WriteHeader(http.StatusBadRequest)
			write(w, map[string]any{"error": "ExpiredToken"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer refresh-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		write(w, map[string]any{
			"accessJwt":  fakeJWT(f.refreshedExp),
			"refreshJwt": "refresh-2",
			"did":        "did:plc:test",
		})
	})
	mux.HandleFunc("/com.atproto.repo.uploadBlob", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.blobBodies = append(f.blobBodies, data)
		f.blobTypes = append(f.blobTypes, r.Header.Get("Content-Type"))
		f.mu.Unlock()
		write(w, map[string]any{"blob": map[string]any{
			"$type":    "blob",
			"ref":      map[string]any{"$link": "bafy123"},
			"mimeType": r.Header.Get("Content-Type"),
			"size":     len(data),
		}})
	})
	mux.HandleFunc("/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.createRecordAuth = append(f.createRecordAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if f.recordCode != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.recordCode)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.createRecordBody = append(f.createRecordBody, body)
		f.mu.Unlock()
		write(w, map[string]any{"uri": "at://did:plc:test/app.bsky.feed.post/3k2"})
	})
	return mux
}

func newFakeBluesky(t *testing.T) (*fakeBluesky, *httptest.Server) {
	t.Helper()
	f := &fakeBluesky{
		sessionExp:   time.Now().Add(2 * time.Hour),
		refreshedExp: time.Now().Add(2 * time.Hour),
	}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, srv
}

func blueskyCfg(serverURL string) query.Crosspost {
	cfg := query.Crosspost{Platform: "bluesky", Enabled: 1}
	cfg.ServerUrl = nullStringForTest(serverURL)
	cfg.Username = nullStringForTest("handle.test")
	cfg.AppPassword = nullStringForTest("app-pw")
	return cfg
}

func TestBlueskyPostFlow(t *testing.T) {
	f, srv := newFakeBluesky(t)
	p := blueskyPlatform{client: srv.Client(), tokens: newFakeTokenCache()}

	in := PostInput{
		Text: "新文章 https://example.com/x 请看",
		Images: []Image{
			{Filename: "a.png", ContentType: "image/png", Data: []byte("small")},
		},
	}
	postURL, err := p.Post(t.Context(), blueskyCfg(srv.URL), in)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if want := "https://bsky.app/profile/handle.test/post/3k2"; postURL != want {
		t.Errorf("post url = %q, want %q", postURL, want)
	}
	if f.createSessionCalls != 1 {
		t.Errorf("createSession calls = %d, want 1", f.createSessionCalls)
	}
	if len(f.blobBodies) != 1 || string(f.blobBodies[0]) != "small" || f.blobTypes[0] != "image/png" {
		t.Errorf("blob upload = %d/%q %q", len(f.blobBodies), f.blobBodies, f.blobTypes)
	}
	if len(f.createRecordBody) != 1 {
		t.Fatalf("createRecord calls = %d", len(f.createRecordBody))
	}
	body := f.createRecordBody[0]
	if body["repo"] != "did:plc:test" || body["collection"] != "app.bsky.feed.post" {
		t.Errorf("repo/collection = %v %v", body["repo"], body["collection"])
	}
	record := body["record"].(map[string]any)
	if record["text"] != in.Text {
		t.Errorf("text = %v", record["text"])
	}
	facets := record["facets"].([]any)
	if len(facets) != 1 {
		t.Fatalf("facets = %v", facets)
	}
	facet := facets[0].(map[string]any)
	index := facet["index"].(map[string]any)
	start := int(index["byteStart"].(float64))
	end := int(index["byteEnd"].(float64))
	if in.Text[start:end] != "https://example.com/x" {
		t.Errorf("facet range [%d:%d] = %q", start, end, in.Text[start:end])
	}
	features := facet["features"].([]any)
	feature := features[0].(map[string]any)
	if feature["$type"] != "app.bsky.richtext.facet#link" || feature["uri"] != "https://example.com/x" {
		t.Errorf("feature = %v", feature)
	}
	embed := record["embed"].(map[string]any)
	if embed["$type"] != "app.bsky.embed.images" {
		t.Errorf("embed type = %v", embed["$type"])
	}
	images := embed["images"].([]any)
	if len(images) != 1 || images[0].(map[string]any)["alt"] != "a.png" {
		t.Errorf("embed images = %v", images)
	}
}

// TestBlueskySessionCacheReuse: a second platform instance with the same
// cache must not log in again (JWT 缓存 1h).
func TestBlueskySessionCacheReuse(t *testing.T) {
	f, srv := newFakeBluesky(t)
	cache := newFakeTokenCache()

	p1 := blueskyPlatform{client: srv.Client(), tokens: cache}
	if _, err := p1.Post(t.Context(), blueskyCfg(srv.URL), PostInput{Text: "one"}); err != nil {
		t.Fatalf("first Post: %v", err)
	}
	p2 := blueskyPlatform{client: srv.Client(), tokens: cache}
	if _, err := p2.Post(t.Context(), blueskyCfg(srv.URL), PostInput{Text: "two"}); err != nil {
		t.Fatalf("second Post: %v", err)
	}
	if f.createSessionCalls != 1 {
		t.Errorf("createSession calls = %d, want 1 (cached session reused)", f.createSessionCalls)
	}
	if f.refreshCalls != 0 {
		t.Errorf("refresh calls = %d, want 0", f.refreshCalls)
	}
}

// TestBlueskyRefreshExpiringToken: a token expiring within 60s is refreshed;
// the new token is used and stored.
func TestBlueskyRefreshExpiringToken(t *testing.T) {
	f, srv := newFakeBluesky(t)
	cache := newFakeTokenCache()
	p := blueskyPlatform{client: srv.Client(), tokens: cache}

	// Seed a valid-cache entry whose token expires in 30s.
	entry := fmt.Sprintf(`{"access_jwt":%q,"refresh_jwt":"refresh-1","did":"did:plc:test","cached_at":%d}`,
		fakeJWT(time.Now().Add(30*time.Second)), time.Now().Unix())
	if err := cache.Set(t.Context(), blueskyTokenCacheKey+":handle.test", entry); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	if _, err := p.Post(t.Context(), blueskyCfg(srv.URL), PostInput{Text: "x"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if f.createSessionCalls != 0 {
		t.Errorf("createSession calls = %d, want 0 (refresh path)", f.createSessionCalls)
	}
	if f.refreshCalls != 1 {
		t.Errorf("refresh calls = %d, want 1", f.refreshCalls)
	}
	if got := f.createRecordAuth[0]; !strings.HasPrefix(got, "Bearer h.") {
		t.Errorf("createRecord auth = %q, want the refreshed JWT", got)
	}
}

// TestBlueskyRefreshFallsBackToLogin: a rejected refresh token triggers one
// full login (refresh_or_regenerate_tokens).
func TestBlueskyRefreshFallsBackToLogin(t *testing.T) {
	f, srv := newFakeBluesky(t)
	f.refreshFails = true
	cache := newFakeTokenCache()
	p := blueskyPlatform{client: srv.Client(), tokens: cache}

	entry := fmt.Sprintf(`{"access_jwt":%q,"refresh_jwt":"refresh-1","did":"did:plc:test","cached_at":%d}`,
		fakeJWT(time.Now().Add(30*time.Second)), time.Now().Unix())
	if err := cache.Set(t.Context(), blueskyTokenCacheKey+":handle.test", entry); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	if _, err := p.Post(t.Context(), blueskyCfg(srv.URL), PostInput{Text: "x"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if f.refreshCalls != 1 || f.createSessionCalls != 1 {
		t.Errorf("refresh/login calls = %d/%d, want 1/1", f.refreshCalls, f.createSessionCalls)
	}
}

func TestBlueskyCreateRecordErrors(t *testing.T) {
	tests := []struct {
		name      string
		code      int
		transient bool
	}{
		{"500 is transient", 500, true},
		{"429 is transient", 429, true},
		{"400 is permanent", 400, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, srv := newFakeBluesky(t)
			f.recordCode = tt.code
			p := blueskyPlatform{client: srv.Client(), tokens: newFakeTokenCache()}
			_, err := p.Post(t.Context(), blueskyCfg(srv.URL), PostInput{Text: "x"})
			if err == nil {
				t.Fatal("want error")
			}
			if IsTransient(err) != tt.transient {
				t.Errorf("IsTransient = %v, want %v", IsTransient(err), tt.transient)
			}
		})
	}
}

func TestBlueskyVerify(t *testing.T) {
	_, srv := newFakeBluesky(t)
	p := blueskyPlatform{client: srv.Client(), tokens: newFakeTokenCache()}

	if err := p.Verify(t.Context(), blueskyCfg(srv.URL)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	err := p.Verify(t.Context(), query.Crosspost{Platform: "bluesky"})
	if err == nil || err.Error() != "App Password and username are required" {
		t.Errorf("blank creds error = %v", err)
	}
	bad := blueskyCfg(srv.URL)
	bad.AppPassword = nullStringForTest("wrong")
	err = p.Verify(t.Context(), bad)
	if err == nil || !strings.Contains(err.Error(), "Bluesky verification failed:") {
		t.Errorf("bad creds error = %v", err)
	}
}

// bigNoisePNG returns a decodable PNG larger than BlueskyService's limit.
func bigNoisePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1200, 900))
	rnd := rand.New(rand.NewSource(1))
	for y := 0; y < 900; y++ {
		for x := 0; x < 1200; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(rnd.Intn(256)), G: uint8(rnd.Intn(256)), B: uint8(rnd.Intn(256)), A: 255})
		}
	}
	var out = &bytes.Buffer{}
	if err := png.Encode(out, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if out.Len() <= blueskyMaxImageSize {
		t.Fatalf("noise png = %d bytes, want > %d", out.Len(), blueskyMaxImageSize)
	}
	return out.Bytes()
}

// TestBlueskyBlobCompression: images over 950KB are compressed under the
// limit and re-encoded as JPEG; small images pass through untouched.
func TestBlueskyBlobCompression(t *testing.T) {
	f, srv := newFakeBluesky(t)
	p := blueskyPlatform{client: srv.Client(), tokens: newFakeTokenCache()}

	big := bigNoisePNG(t)
	in := PostInput{
		Text: "x",
		Images: []Image{
			{Filename: "big.png", ContentType: "image/png", Data: big},
			{Filename: "small.png", ContentType: "image/png", Data: []byte("tiny")},
		},
	}
	if _, err := p.Post(t.Context(), blueskyCfg(srv.URL), in); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(f.blobBodies) != 2 {
		t.Fatalf("blob uploads = %d, want 2", len(f.blobBodies))
	}
	if len(f.blobBodies[0]) > blueskyMaxImageSize {
		t.Errorf("compressed blob = %d bytes, want <= %d", len(f.blobBodies[0]), blueskyMaxImageSize)
	}
	if f.blobTypes[0] != "image/jpeg" {
		t.Errorf("compressed content type = %q, want image/jpeg", f.blobTypes[0])
	}
	if got := f.blobBodies[0]; len(got) < 2 || got[0] != 0xff || got[1] != 0xd8 {
		t.Errorf("compressed blob is not a JPEG: % x", got[:min(8, len(got))])
	}
	if string(f.blobBodies[1]) != "tiny" || f.blobTypes[1] != "image/png" {
		t.Errorf("small image must pass through unchanged, got %d bytes %q", len(f.blobBodies[1]), f.blobTypes[1])
	}
}

func TestCompressImage(t *testing.T) {
	if _, ok := compressImage([]byte("not an image"), 1000); ok {
		t.Error("undecodable input must fail")
	}
	// Impossibly small limit: the dimension floor (100px) forces a give-up.
	if _, ok := compressImage(bigNoisePNG(t), 16); ok {
		t.Error("tiny limit must give up (MIN_IMAGE_DIMENSION)")
	}
}

func TestLinkFacets(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string // urls in facet order; nil = no facets
	}{
		{"no url", "just text", nil},
		{"single", "see https://example.com/a end", []string{"https://example.com/a"}},
		{"fragment kept", "https://example.com/a?x=1#frag done", []string{"https://example.com/a?x=1#frag"}},
		{"parens kept", "https://example.com/p(1)", []string{"https://example.com/p(1)"}},
		{"cjk stops match", "https://example.com/中文", []string{"https://example.com/"}},
		{"bracket stops match", "https://example.com/a[1]", []string{"https://example.com/a"}},
		{"two urls", "https://a.com and https://b.com/x", []string{"https://a.com", "https://b.com/x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facets := linkFacets(tt.text)
			if len(facets) != len(tt.want) {
				t.Fatalf("facets = %v, want %d", facets, len(tt.want))
			}
			for i, facet := range facets {
				index := facet["index"].(map[string]any)
				start, end := index["byteStart"].(int), index["byteEnd"].(int)
				if got := tt.text[start:end]; got != tt.want[i] {
					t.Errorf("facet %d range = %q, want %q", i, got, tt.want[i])
				}
			}
		})
	}
}
