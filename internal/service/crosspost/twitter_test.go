package crosspost

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dghubble/oauth1"

	"rables/internal/db/query"
)

const (
	testConsumerKey    = "consumer-key"
	testConsumerSecret = "consumer-secret"
	testAccessToken    = "access-token"
	testAccessSecret   = "access-secret"
)

func twitterCfg() query.Crosspost {
	cfg := query.Crosspost{Platform: "twitter", Enabled: 1}
	cfg.ApiKey = nullStringForTest(testConsumerKey)
	cfg.ApiKeySecret = nullStringForTest(testConsumerSecret)
	cfg.AccessToken = nullStringForTest(testAccessToken)
	cfg.AccessTokenSecret = nullStringForTest(testAccessSecret)
	return cfg
}

// verifyOAuth1Header recomputes the request's OAuth1 HMAC-SHA1 signature from
// the Authorization header parameters (the nonce/timestamp the client picked)
// and the known test secrets — a reproducible check that every request is
// signed per RFC 5849.
func verifyOAuth1Header(r *http.Request) error {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "OAuth ") {
		return fmt.Errorf("missing OAuth authorization header")
	}
	params := map[string]string{}
	for _, pair := range strings.Split(strings.TrimPrefix(auth, "OAuth "), ", ") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("malformed oauth param %q", pair)
		}
		v, err := url.QueryUnescape(strings.Trim(kv[1], `"`))
		if err != nil {
			return fmt.Errorf("decode %s: %v", kv[0], err)
		}
		params[kv[0]] = v
	}
	for _, key := range []string{"oauth_consumer_key", "oauth_nonce", "oauth_signature",
		"oauth_signature_method", "oauth_timestamp", "oauth_token", "oauth_version"} {
		if params[key] == "" {
			return fmt.Errorf("missing %s", key)
		}
	}
	if params["oauth_consumer_key"] != testConsumerKey {
		return fmt.Errorf("consumer key = %q", params["oauth_consumer_key"])
	}
	if params["oauth_token"] != testAccessToken {
		return fmt.Errorf("token = %q", params["oauth_token"])
	}
	if params["oauth_signature_method"] != "HMAC-SHA1" {
		return fmt.Errorf("signature method = %q", params["oauth_signature_method"])
	}
	if params["oauth_version"] != "1.0" {
		return fmt.Errorf("version = %q", params["oauth_version"])
	}
	ts, err := strconv.ParseInt(params["oauth_timestamp"], 10, 64)
	if err != nil || ts < time.Now().Unix()-120 || ts > time.Now().Unix()+120 {
		return fmt.Errorf("timestamp = %q", params["oauth_timestamp"])
	}

	// Rebuild the signature base string: method & base URI & sorted params
	// (oauth params minus the signature, plus the query params).
	signing := map[string]string{}
	for k, v := range params {
		if k != "oauth_signature" {
			signing[k] = v
		}
	}
	for k, vs := range r.URL.Query() {
		signing[k] = vs[0]
	}
	keys := make([]string, 0, len(signing))
	for k := range signing {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, oauth1.PercentEncode(k)+"="+oauth1.PercentEncode(signing[k]))
	}
	baseURI := "http://" + strings.ToLower(r.Host) + r.URL.Path
	baseString := strings.ToUpper(r.Method) + "&" + oauth1.PercentEncode(baseURI) +
		"&" + oauth1.PercentEncode(strings.Join(pairs, "&"))
	mac := hmac.New(sha1.New, []byte(testConsumerSecret+"&"+testAccessSecret))
	_, _ = mac.Write([]byte(baseString))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if params["oauth_signature"] != want {
		return fmt.Errorf("signature mismatch: header %q, recomputed %q", params["oauth_signature"], want)
	}
	return nil
}

type recordedAppend struct {
	mediaID      string
	segmentIndex string
	contentType  string
	data         []byte
}

type tweetScript struct {
	code int
	body string
}

// fakeTwitter is a scripted X API (v2 endpoints, x-0.19.0 wire shape).
type fakeTwitter struct {
	mu sync.Mutex

	oauthFailures []string
	requestLog    []string
	usersMeCalls  int
	tweetBodies   []map[string]any
	initBodies    []map[string]any
	appends       []recordedAppend
	finalizes     []string
	statusPolls   int

	usersMeCode int // 0 → 200
	initCode    int // 0 → 200
	// tweetScripts are consumed one per POST /2/tweets; empty falls back to a
	// 201 with a fresh id.
	tweetScripts []tweetScript
	// finalizeState / statusState drive processing_info.
	finalizeState string
	statusState   string

	mediaSeq int
}

func (f *fakeTwitter) log(format string, args ...any) {
	f.requestLog = append(f.requestLog, fmt.Sprintf(format, args...))
}

func (f *fakeTwitter) writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func (f *fakeTwitter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/2/users/me", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.usersMeCalls++
		f.log("ME")
		if f.usersMeCode != 0 {
			f.writeJSON(w, f.usersMeCode, `{"errors":[{"message":"boom"}]}`)
			return
		}
		f.writeJSON(w, http.StatusOK, `{"data":{"id":"u1","username":"tester"}}`)
	})
	mux.HandleFunc("/2/tweets", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.log("TWEET")
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.tweetBodies = append(f.tweetBodies, body)
		if len(f.tweetScripts) > 0 {
			script := f.tweetScripts[0]
			f.tweetScripts = f.tweetScripts[1:]
			f.writeJSON(w, script.code, script.body)
			return
		}
		f.writeJSON(w, http.StatusCreated, fmt.Sprintf(`{"data":{"id":"t%d","text":"ok"}}`, len(f.tweetBodies)))
	})
	mux.HandleFunc("/2/media/upload/initialize", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.mediaSeq++
		id := fmt.Sprintf("m%d", f.mediaSeq)
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.initBodies = append(f.initBodies, body)
		f.log("INIT %s", id)
		if f.initCode != 0 {
			f.writeJSON(w, f.initCode, `{"errors":[{"message":"init failed"}]}`)
			return
		}
		f.writeJSON(w, http.StatusOK, fmt.Sprintf(`{"data":{"id":%q}}`, id))
	})
	mux.HandleFunc("/2/media/upload/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.TrimPrefix(r.URL.Path, "/2/media/upload/")
		mediaID, action, _ := strings.Cut(rest, "/")
		switch action {
		case "append":
			// The gem's media part carries no filename, so it lands in the
			// value side of a parsed form; stream the parts instead to keep
			// the per-part headers visible.
			mr, err := r.MultipartReader()
			if err != nil {
				f.writeJSON(w, http.StatusBadRequest, `{"errors":[{"message":"bad multipart"}]}`)
				return
			}
			rec := recordedAppend{mediaID: mediaID}
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					f.writeJSON(w, http.StatusBadRequest, `{"errors":[{"message":"bad multipart"}]}`)
					return
				}
				data, _ := io.ReadAll(part)
				switch part.FormName() {
				case "segment_index":
					rec.segmentIndex = string(data)
				case "media":
					rec.contentType = part.Header.Get("Content-Type")
					rec.data = data
				}
			}
			if rec.data == nil {
				f.writeJSON(w, http.StatusBadRequest, `{"errors":[{"message":"no media part"}]}`)
				return
			}
			f.appends = append(f.appends, rec)
			f.log("APPEND %s %s", mediaID, rec.segmentIndex)
			w.WriteHeader(http.StatusNoContent)
		case "finalize":
			f.finalizes = append(f.finalizes, mediaID)
			f.log("FINALIZE %s", mediaID)
			if f.finalizeState != "" {
				f.writeJSON(w, http.StatusOK, fmt.Sprintf(
					`{"data":{"id":%q,"processing_info":{"state":%q,"check_after_secs":0}}}`, mediaID, f.finalizeState))
				return
			}
			f.writeJSON(w, http.StatusOK, fmt.Sprintf(`{"data":{"id":%q}}`, mediaID))
		default:
			f.writeJSON(w, http.StatusNotFound, `{}`)
		}
	})
	mux.HandleFunc("/2/media/upload", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.statusPolls++
		f.log("STATUS %s", r.URL.Query().Get("media_id"))
		state := f.statusState
		f.writeJSON(w, http.StatusOK, fmt.Sprintf(
			`{"data":{"id":%q,"processing_info":{"state":%q,"check_after_secs":0}}}`,
			r.URL.Query().Get("media_id"), state))
	})
	return mux
}

// authRecorder wraps the mux and records every OAuth verification failure.
func (f *fakeTwitter) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if err := verifyOAuth1Header(r); err != nil {
		f.mu.Lock()
		f.oauthFailures = append(f.oauthFailures, r.Method+" "+r.URL.Path+": "+err.Error())
		f.mu.Unlock()
	}
	f.handler().ServeHTTP(w, r)
}

func newFakeTwitter(t *testing.T) (*fakeTwitter, *httptest.Server) {
	t.Helper()
	f := &fakeTwitter{}
	srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(srv.Close)
	return f, srv
}

func newTwitterPlatform(srv *httptest.Server, cache tokenCache) twitterPlatform {
	return twitterPlatform{
		baseURL: srv.URL + "/2", // like the gem's DEFAULT_BASE_URL https://api.twitter.com/2/
		client:  srv.Client(),
		tokens:  cache,
		sleep:   func(context.Context, time.Duration) error { return nil },
	}
}

func TestTwitterVerify(t *testing.T) {
	f, srv := newFakeTwitter(t)
	p := newTwitterPlatform(srv, newFakeTokenCache())

	if err := p.Verify(t.Context(), twitterCfg()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if f.usersMeCalls != 1 {
		t.Errorf("users/me calls = %d, want 1", f.usersMeCalls)
	}

	err := p.Verify(t.Context(), query.Crosspost{Platform: "twitter"})
	if err == nil || err.Error() != "Please fill in all information" {
		t.Errorf("blank creds error = %v", err)
	}

	f.usersMeCode = http.StatusUnauthorized
	err = p.Verify(t.Context(), twitterCfg())
	if err == nil || !strings.HasPrefix(err.Error(), "Twitter verification failed: ") {
		t.Errorf("401 error = %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("401 error should carry the API message, got %v", err)
	}
	if len(f.oauthFailures) != 0 {
		t.Errorf("oauth failures = %v", f.oauthFailures)
	}
}

func TestTwitterPostTextOnly(t *testing.T) {
	f, srv := newFakeTwitter(t)
	p := newTwitterPlatform(srv, newFakeTokenCache())

	postURL, err := p.Post(t.Context(), twitterCfg(), PostInput{Text: "Hello X"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if want := "https://x.com/tester/status/t1"; postURL != want {
		t.Errorf("post url = %q, want %q", postURL, want)
	}
	if len(f.tweetBodies) != 1 {
		t.Fatalf("tweets = %d, want 1", len(f.tweetBodies))
	}
	body := f.tweetBodies[0]
	if body["text"] != "Hello X" {
		t.Errorf("text = %v", body["text"])
	}
	if _, ok := body["media"]; ok {
		t.Errorf("unexpected media key: %v", body["media"])
	}
	if _, ok := body["quote_tweet_id"]; ok {
		t.Errorf("unexpected quote_tweet_id: %v", body["quote_tweet_id"])
	}
	if len(f.appends) != 0 || len(f.initBodies) != 0 {
		t.Error("no media requests expected for a text-only post")
	}
	if len(f.oauthFailures) != 0 {
		t.Errorf("oauth failures = %v", f.oauthFailures)
	}
}

func TestTwitterPostWithImages(t *testing.T) {
	f, srv := newFakeTwitter(t)
	p := newTwitterPlatform(srv, newFakeTokenCache())

	in := PostInput{
		Text: "with pics",
		Images: []Image{
			{Filename: "a.png", ContentType: "image/png", Data: []byte("png-bytes")},
			{Filename: "b.jpg", ContentType: "image/jpeg", Data: []byte("jpg-bytes")},
		},
	}
	postURL, err := p.Post(t.Context(), twitterCfg(), in)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if want := "https://x.com/tester/status/t1"; postURL != want {
		t.Errorf("post url = %q, want %q", postURL, want)
	}

	// The chunked sequence runs INIT → APPEND → FINALIZE per image, in order.
	wantLog := []string{"ME", "INIT m1", "APPEND m1 0", "FINALIZE m1", "INIT m2", "APPEND m2 0", "FINALIZE m2", "TWEET"}
	if strings.Join(f.requestLog, ",") != strings.Join(wantLog, ",") {
		t.Errorf("request log = %v, want %v", f.requestLog, wantLog)
	}

	init1 := f.initBodies[0]
	if init1["media_type"] != "image/png" || init1["media_category"] != "tweet_image" {
		t.Errorf("init1 = %v", init1)
	}
	if init1["total_bytes"].(float64) != float64(len("png-bytes")) {
		t.Errorf("init1 total_bytes = %v", init1["total_bytes"])
	}
	if f.appends[0].contentType != "application/octet-stream" || string(f.appends[0].data) != "png-bytes" {
		t.Errorf("append1 = %q %q", f.appends[0].contentType, f.appends[0].data)
	}

	media, ok := f.tweetBodies[0]["media"].(map[string]any)
	if !ok {
		t.Fatalf("tweet media = %v", f.tweetBodies[0])
	}
	ids := media["media_ids"].([]any)
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m2" {
		t.Errorf("media_ids = %v, want [m1 m2]", ids)
	}
	if len(f.oauthFailures) != 0 {
		t.Errorf("oauth failures = %v", f.oauthFailures)
	}
}

// TestTwitterUsernameCache: the users/me lookup is cached for a week
// (fetch_username), so a second platform instance skips it; a lookup failure
// is swallowed and the i/web/status URL form is used.
func TestTwitterUsernameCache(t *testing.T) {
	f, srv := newFakeTwitter(t)
	cache := newFakeTokenCache()

	if _, err := newTwitterPlatform(srv, cache).Post(t.Context(), twitterCfg(), PostInput{Text: "one"}); err != nil {
		t.Fatalf("first Post: %v", err)
	}
	postURL, err := newTwitterPlatform(srv, cache).Post(t.Context(), twitterCfg(), PostInput{Text: "two"})
	if err != nil {
		t.Fatalf("second Post: %v", err)
	}
	if f.usersMeCalls != 1 {
		t.Errorf("users/me calls = %d, want 1 (cached)", f.usersMeCalls)
	}
	if want := "https://x.com/tester/status/t2"; postURL != want {
		t.Errorf("second url = %q, want %q", postURL, want)
	}
}

func TestTwitterUsernameFailureFallsBack(t *testing.T) {
	f, srv := newFakeTwitter(t)
	f.usersMeCode = http.StatusInternalServerError
	p := newTwitterPlatform(srv, newFakeTokenCache())

	postURL, err := p.Post(t.Context(), twitterCfg(), PostInput{Text: "x"})
	if err != nil {
		t.Fatalf("Post: %v (username failure must not abort the post)", err)
	}
	if want := "https://x.com/i/web/status/t1"; postURL != want {
		t.Errorf("post url = %q, want %q", postURL, want)
	}
	// Failures are not cached (skip_nil): a retry looks the username up again.
	if _, err := p.Post(t.Context(), twitterCfg(), PostInput{Text: "y"}); err != nil {
		t.Fatalf("second Post: %v", err)
	}
	if f.usersMeCalls != 2 {
		t.Errorf("users/me calls = %d, want 2 (failure not cached)", f.usersMeCalls)
	}
}

func TestTwitterQuoteTweet(t *testing.T) {
	f, srv := newFakeTwitter(t)
	p := newTwitterPlatform(srv, newFakeTokenCache())

	source := "https://x.com/someone/status/12345"
	in := PostInput{
		Text:      "my take\n" + source, // dispatcher's BuildContent appends the source last
		SourceURL: source,
		Images:    []Image{{Filename: "a.png", ContentType: "image/png", Data: []byte("d")}},
	}
	if _, err := p.Post(t.Context(), twitterCfg(), in); err != nil {
		t.Fatalf("Post: %v", err)
	}
	body := f.tweetBodies[0]
	if body["quote_tweet_id"] != "12345" {
		t.Errorf("quote_tweet_id = %v, want 12345", body["quote_tweet_id"])
	}
	if body["text"] != "my take" {
		t.Errorf("text = %q, want the source reference stripped", body["text"])
	}
	if _, ok := body["media"]; ok {
		t.Errorf("quote tweets must not carry media: %v", body["media"])
	}
	if len(f.initBodies) != 0 {
		t.Error("quote tweets must skip media uploads entirely")
	}
}

func TestTwitterQuoteTweetIDParsing(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		{"https://x.com/user/status/12345", "12345"},
		{"https://twitter.com/user/status/999", "999"},
		{"https://mobile.twitter.com/user/status/7", "7"},
		{"https://x.com/i/web/status/42", "42"},
		{"https://x.com/i/status/43", "43"},
		{"https://x.com/statuses/44", "44"},
		{"https://x.com/user/status/55/photo/1", "55"},
		{"x.com/user/status/66", "66"},
		{"HTTPS://TWITTER.COM/user/STATUS/77", "77"},
		{"  https://x.com/user/status/88  ", "88"},
		{"https://mastodon.social/@user/12345", ""},
		{"https://x.com.evil.com/user/status/1", ""},
		{"https://not-x.com/user/status/1", ""},
		{"https://x.com/user/status/", ""},
		{"https://x.com/user", ""},
		{"", ""},
		{"   ", ""},
		{"://bad url", ""},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			if got := twitterQuoteTweetID(tt.source); got != tt.want {
				t.Errorf("twitterQuoteTweetID(%q) = %q, want %q", tt.source, got, tt.want)
			}
		})
	}
}

// TestTwitterGIFPostsAlone: an animated GIF replaces the whole image set
// (limit_twitter_media_attachments) and uploads as tweet_gif.
func TestTwitterGIFPostsAlone(t *testing.T) {
	f, srv := newFakeTwitter(t)
	p := newTwitterPlatform(srv, newFakeTokenCache())

	in := PostInput{
		Text: "gif post",
		Images: []Image{
			{Filename: "a.png", ContentType: "image/png", Data: []byte("png")},
			{Filename: "b.gif", ContentType: "image/gif", Data: []byte("gif-data")},
			{Filename: "c.jpg", ContentType: "image/jpeg", Data: []byte("jpg")},
		},
	}
	if _, err := p.Post(t.Context(), twitterCfg(), in); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(f.initBodies) != 1 {
		t.Fatalf("uploads = %d, want 1 (GIF alone)", len(f.initBodies))
	}
	init := f.initBodies[0]
	if init["media_category"] != "tweet_gif" || init["media_type"] != "image/gif" {
		t.Errorf("gif init = %v", init)
	}
	ids := f.tweetBodies[0]["media"].(map[string]any)["media_ids"].([]any)
	if len(ids) != 1 {
		t.Errorf("media_ids = %v, want exactly the GIF", ids)
	}
}

// TestTwitterGIFByExtension: a remote image keeps only its URL extension as
// the GIF-alone selection signal (remote_gif_attachable_url?); the upload
// parameters still follow the content type (media_type_for_file).
func TestTwitterGIFByExtension(t *testing.T) {
	f, srv := newFakeTwitter(t)
	p := newTwitterPlatform(srv, newFakeTokenCache())

	in := PostInput{
		Text: "gif by name",
		Images: []Image{
			{Filename: "a.png", ContentType: "image/png", Data: []byte("png")},
			{Filename: "clip.GIF", ContentType: "application/octet-stream", Data: []byte("gif")},
		},
	}
	if _, err := p.Post(t.Context(), twitterCfg(), in); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(f.initBodies) != 1 {
		t.Fatalf("uploads = %d, want 1 (GIF selected alone via its extension)", len(f.initBodies))
	}
	// content_type 不是 image/gif → 按 Rails 归一为 jpg/tweet_image 上传。
	init := f.initBodies[0]
	if init["media_category"] != "tweet_image" || init["media_type"] != "image/jpeg" {
		t.Errorf("init = %v, want content-type-driven tweet_image/image/jpeg", init)
	}
}

// TestTwitterMediaErrorFallsBackToText: a 2xx tweet response whose error
// mentions media triggers a text-only retry (try_text_only_tweet), keeping
// the quote id.
func TestTwitterMediaErrorFallsBackToText(t *testing.T) {
	f, srv := newFakeTwitter(t)
	f.tweetScripts = []tweetScript{
		{http.StatusOK, `{"errors":[{"message":"Invalid media ids"}]}`},
		{http.StatusCreated, `{"data":{"id":"fallback-1","text":"ok"}}`},
	}
	p := newTwitterPlatform(srv, newFakeTokenCache())

	source := "https://twitter.com/a/status/314"
	in := PostInput{
		Text:      "quote with media?\n" + source,
		SourceURL: "", // media path: images present, no quote
		Images:    []Image{{Filename: "a.png", ContentType: "image/png", Data: []byte("d")}},
	}
	postURL, err := p.Post(t.Context(), twitterCfg(), in)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if want := "https://x.com/tester/status/fallback-1"; postURL != want {
		t.Errorf("post url = %q, want %q", postURL, want)
	}
	if len(f.tweetBodies) != 2 {
		t.Fatalf("tweets = %d, want 2 (media + text-only retry)", len(f.tweetBodies))
	}
	if _, ok := f.tweetBodies[0]["media"]; !ok {
		t.Error("first tweet must carry media")
	}
	if _, ok := f.tweetBodies[1]["media"]; ok {
		t.Error("fallback tweet must be text-only")
	}
	if f.tweetBodies[1]["text"] != in.Text {
		t.Errorf("fallback text = %v", f.tweetBodies[1]["text"])
	}
}

// TestTwitterNonMediaErrorIsPermanent: a 2xx failure without "media" in the
// message is not retried.
func TestTwitterNonMediaErrorIsPermanent(t *testing.T) {
	f, srv := newFakeTwitter(t)
	f.tweetScripts = []tweetScript{
		{http.StatusOK, `{"errors":[{"message":"duplicate content"}]}`},
	}
	p := newTwitterPlatform(srv, newFakeTokenCache())

	in := PostInput{Text: "x", Images: []Image{{Filename: "a.png", ContentType: "image/png", Data: []byte("d")}}}
	_, err := p.Post(t.Context(), twitterCfg(), in)
	if err == nil || IsTransient(err) {
		t.Fatalf("error = %v, want permanent", err)
	}
	if !strings.Contains(err.Error(), "duplicate content") {
		t.Errorf("error = %v, want the API message", err)
	}
	if len(f.tweetBodies) != 1 {
		t.Errorf("tweets = %d, want 1 (no fallback without a media error)", len(f.tweetBodies))
	}
}

// TestTwitterUploadFailureSkipsImage: a permanent upload failure drops the
// image and the tweet goes out text-only (upload returns nil in Rails).
func TestTwitterUploadFailureSkipsImage(t *testing.T) {
	f, srv := newFakeTwitter(t)
	f.initCode = http.StatusBadRequest
	p := newTwitterPlatform(srv, newFakeTokenCache())

	in := PostInput{Text: "x", Images: []Image{{Filename: "a.png", ContentType: "image/png", Data: []byte("d")}}}
	postURL, err := p.Post(t.Context(), twitterCfg(), in)
	if err != nil {
		t.Fatalf("Post: %v (permanent upload failure must skip the image)", err)
	}
	if postURL == "" {
		t.Fatal("want a posted url")
	}
	if _, ok := f.tweetBodies[0]["media"]; ok {
		t.Errorf("failed upload must yield a text-only tweet: %v", f.tweetBodies[0]["media"])
	}
}

func TestTwitterTweetErrorClassification(t *testing.T) {
	tests := []struct {
		name      string
		code      int
		body      string
		transient bool
	}{
		{"429 is transient", http.StatusTooManyRequests, `{"errors":[{"message":"Too Many Requests"}]}`, true},
		{"500 is transient", http.StatusInternalServerError, `{"errors":[{"message":"Internal error"}]}`, true},
		{"503 is transient", http.StatusServiceUnavailable, `{"title":"Service Unavailable","detail":"down"}`, true},
		{"400 is permanent", http.StatusBadRequest, `{"errors":[{"message":"bad request"}]}`, false},
		{"403 is permanent", http.StatusForbidden, `{"error":"forbidden"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, srv := newFakeTwitter(t)
			f.tweetScripts = []tweetScript{{tt.code, tt.body}}
			p := newTwitterPlatform(srv, newFakeTokenCache())
			_, err := p.Post(t.Context(), twitterCfg(), PostInput{Text: "x"})
			if err == nil {
				t.Fatal("want error")
			}
			if IsTransient(err) != tt.transient {
				t.Errorf("IsTransient = %v, want %v (err = %v)", IsTransient(err), tt.transient, err)
			}
		})
	}
}

// TestTwitterUploadTransientAbortsPost: a 503 during INIT propagates as
// transient so the job retries the whole post.
func TestTwitterUploadTransientAbortsPost(t *testing.T) {
	f, srv := newFakeTwitter(t)
	f.initCode = http.StatusServiceUnavailable
	p := newTwitterPlatform(srv, newFakeTokenCache())

	in := PostInput{Text: "x", Images: []Image{{Filename: "a.png", ContentType: "image/png", Data: []byte("d")}}}
	_, err := p.Post(t.Context(), twitterCfg(), in)
	if err == nil || !IsTransient(err) {
		t.Fatalf("error = %v, want transient", err)
	}
	if len(f.tweetBodies) != 0 {
		t.Error("no tweet must be attempted when an upload fails transiently")
	}
}

// TestTwitterAwaitProcessing: a pending FINALIZE polls STATUS until
// succeeded; a failed state drops the image.
func TestTwitterAwaitProcessing(t *testing.T) {
	f, srv := newFakeTwitter(t)
	f.finalizeState = "pending"
	f.statusState = "succeeded"
	p := newTwitterPlatform(srv, newFakeTokenCache())

	in := PostInput{Text: "x", Images: []Image{{Filename: "a.png", ContentType: "image/png", Data: []byte("d")}}}
	if _, err := p.Post(t.Context(), twitterCfg(), in); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if f.statusPolls != 1 {
		t.Errorf("status polls = %d, want 1", f.statusPolls)
	}
	ids := f.tweetBodies[0]["media"].(map[string]any)["media_ids"].([]any)
	if len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("media_ids = %v, want [m1]", ids)
	}
}

func TestTwitterProcessingFailedSkipsImage(t *testing.T) {
	f, srv := newFakeTwitter(t)
	f.finalizeState = "pending"
	f.statusState = "failed"
	p := newTwitterPlatform(srv, newFakeTokenCache())

	in := PostInput{Text: "x", Images: []Image{{Filename: "a.png", ContentType: "image/png", Data: []byte("d")}}}
	postURL, err := p.Post(t.Context(), twitterCfg(), in)
	if err != nil {
		t.Fatalf("Post: %v (failed processing is permanent → image skipped)", err)
	}
	if postURL == "" {
		t.Fatal("want a posted url")
	}
	if _, ok := f.tweetBodies[0]["media"]; ok {
		t.Errorf("processing-failed image must be dropped: %v", f.tweetBodies[0]["media"])
	}
}

func TestSplitChunks(t *testing.T) {
	tests := []struct {
		size      int
		chunkSize int
		wantLens  []int
	}{
		{0, 5, nil},
		{3, 5, []int{3}},
		{5, 5, []int{5}},
		{6, 5, []int{5, 1}},
		{11, 5, []int{5, 5, 1}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d/%d", tt.size, tt.chunkSize), func(t *testing.T) {
			chunks := splitChunks(make([]byte, tt.size), tt.chunkSize)
			if len(chunks) != len(tt.wantLens) {
				t.Fatalf("chunks = %d, want %d", len(chunks), len(tt.wantLens))
			}
			for i, want := range tt.wantLens {
				if len(chunks[i]) != want {
					t.Errorf("chunk %d = %d bytes, want %d", i, len(chunks[i]), want)
				}
			}
		})
	}
}

// TestTwitterMultiChunkUpload drives the chunk helpers directly to exercise
// multi-segment APPEND ordering (Post never produces one: chunk size equals
// the size limit).
func TestTwitterMultiChunkUpload(t *testing.T) {
	f, srv := newFakeTwitter(t)
	p := newTwitterPlatform(srv, newFakeTokenCache())

	data := make([]byte, twitterImageChunkSize+10)
	client := p.signingClient(twitterCfg())

	// INIT → APPEND 0 → APPEND 1 → FINALIZE.
	id, err := p.mediaUploadInit(t.Context(), client, "image/png", "tweet_image", len(data))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	for i, chunk := range splitChunks(data, twitterImageChunkSize) {
		if err := p.mediaUploadAppend(t.Context(), client, id, i, chunk); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if _, err := p.mediaUploadFinalize(t.Context(), client, id); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	wantLog := []string{"INIT m1", "APPEND m1 0", "APPEND m1 1", "FINALIZE m1"}
	if strings.Join(f.requestLog, ",") != strings.Join(wantLog, ",") {
		t.Errorf("request log = %v, want %v", f.requestLog, wantLog)
	}
	if got := len(f.appends[0].data); got != twitterImageChunkSize {
		t.Errorf("first chunk = %d bytes, want %d", got, twitterImageChunkSize)
	}
	if got := len(f.appends[1].data); got != 10 {
		t.Errorf("second chunk = %d bytes, want 10", got)
	}
	if len(f.oauthFailures) != 0 {
		t.Errorf("oauth failures = %v", f.oauthFailures)
	}
}

func TestTwitterRegistered(t *testing.T) {
	if Get("twitter") == nil {
		t.Fatal("twitter platform must self-register via init")
	}
	if Get("twitter").Name() != "twitter" {
		t.Errorf("name = %q", Get("twitter").Name())
	}
}
