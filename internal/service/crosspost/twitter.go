package crosspost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/dghubble/oauth1"

	"rables/internal/db/query"
)

// twitterDefaultBaseURL mirrors X::Client::DEFAULT_BASE_URL.
const twitterDefaultBaseURL = "https://api.twitter.com/2/"

// twitterUsernameCacheTTL mirrors the fetch_username Rails.cache TTL (1.week).
const twitterUsernameCacheTTL = 7 * 24 * time.Hour

// twitterPlatform ports TwitterService: OAuth1.0a-signed posting to the X API
// (v2 endpoints, like the bundled x-0.19.0 gem). The zero value is the
// registered instance; tests inject baseURL/client/cache/clock/sleep.
type twitterPlatform struct {
	baseURL string       // "" → twitterDefaultBaseURL
	client  *http.Client // nil → defaultHTTPClient (its Transport/Timeout are reused)
	tokens  tokenCache   // nil → sharedTokens
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error // nil → ctxSleep
}

func init() { RegisterPlatform(twitterPlatform{}) }

func (twitterPlatform) Name() string { return "twitter" }

func (p twitterPlatform) http() *http.Client {
	if p.client != nil {
		return p.client
	}
	return defaultHTTPClient
}

func (p twitterPlatform) base() string {
	if p.baseURL != "" {
		return strings.TrimSuffix(p.baseURL, "/")
	}
	return strings.TrimSuffix(twitterDefaultBaseURL, "/")
}

func (p twitterPlatform) clock() func() time.Time {
	if p.now != nil {
		return p.now
	}
	return time.Now
}

func (p twitterPlatform) cache() tokenCache {
	if p.tokens != nil {
		return p.tokens
	}
	return sharedTokens
}

func (p twitterPlatform) wait(ctx context.Context, d time.Duration) error {
	if p.sleep != nil {
		return p.sleep(ctx, d)
	}
	return ctxSleep(ctx, d)
}

// signingClient builds an OAuth1.0a HMAC-SHA1 signing HTTP client for the
// four credential parts (X::Client.new with api_key/api_key_secret/
// access_token/access_token_secret).
func (p twitterPlatform) signingClient(cfg query.Crosspost) *http.Client {
	config := oauth1.NewConfig(cfg.ApiKey.String, cfg.ApiKeySecret.String)
	token := oauth1.NewToken(cfg.AccessToken.String, cfg.AccessTokenSecret.String)
	base := p.http()
	// The context client only lends its Transport to the oauth1.Transport base.
	ctx := context.WithValue(oauth1.NoContext, oauth1.HTTPClient, base)
	client := config.Client(ctx, token)
	client.Timeout = base.Timeout
	return client
}

// Verify ports TwitterService#verify: all four credentials are required, then
// GET /2/users/me must return data.id.
func (p twitterPlatform) Verify(ctx context.Context, cfg query.Crosspost) error {
	if cfg.AccessTokenSecret.String == "" || cfg.AccessToken.String == "" ||
		cfg.ApiKey.String == "" || cfg.ApiKeySecret.String == "" {
		return errors.New("Please fill in all information")
	}
	body, err := p.doJSON(ctx, p.signingClient(cfg), http.MethodGet, p.base()+"/users/me", nil, "")
	if err != nil {
		return fmt.Errorf("Twitter verification failed: %w", err)
	}
	if id := xDataID(body); id != "" {
		return nil
	}
	raw, _ := json.Marshal(body)
	return fmt.Errorf("Twitter verification failed: %s", raw)
}

// Post ports TwitterService#post: fetch the username (cached), upload images
// (skipped for quote tweets, GIF posts alone), create the tweet, and fall
// back to a text-only tweet when X rejects the media. The returned URL feeds
// social_media_posts via the dispatcher.
func (p twitterPlatform) Post(ctx context.Context, cfg query.Crosspost, in PostInput) (string, error) {
	if cfg.Enabled != 1 {
		return "", nil
	}
	client := p.signingClient(cfg)

	quoteTweetID := twitterQuoteTweetID(in.SourceURL)
	text := in.Text
	if quoteTweetID != "" {
		// The quote branch builds content without the source reference; the
		// dispatcher's BuildContent always appends "\n"+source_url last.
		text = strings.TrimSuffix(in.Text, "\n"+in.SourceURL)
	}

	username := p.fetchUsername(ctx, client, cfg.AccessToken.String)

	images := in.Images
	if quoteTweetID != "" { // tweet_images_for_article: no media on quotes
		images = nil
	}
	images = limitTwitterMediaAttachments(images)

	var mediaIDs []string
	for _, img := range images {
		mediaID, err := p.uploadMedia(ctx, client, img)
		if err != nil {
			if IsTransient(err) {
				return "", err
			}
			slog.Warn("twitter: image upload failed", "filename", img.Filename, "error", err)
			continue
		}
		if mediaID != "" {
			mediaIDs = append(mediaIDs, mediaID)
		}
	}

	resp, err := p.createTweet(ctx, client, buildTweetData(text, quoteTweetID, mediaIDs))
	if err != nil {
		return "", err
	}
	return p.handleTweetResponse(ctx, client, resp, text, quoteTweetID, mediaIDs, username)
}

// buildTweetData ports build_tweet_data.
func buildTweetData(text, quoteTweetID string, mediaIDs []string) map[string]any {
	data := map[string]any{"text": text}
	if quoteTweetID != "" {
		data["quote_tweet_id"] = quoteTweetID
	}
	if len(mediaIDs) > 0 {
		data["media"] = map[string]any{"media_ids": mediaIDs}
	}
	return data
}

// createTweet ports create_tweet_with_retry without the in-process sleep
// ladder: a 429 surfaces as TransientError so the job retries with its own
// backoff instead of blocking the worker (see report).
func (p twitterPlatform) createTweet(ctx context.Context, client *http.Client, tweetData map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(tweetData)
	if err != nil {
		return nil, err
	}
	return p.doJSON(ctx, client, http.MethodPost, p.base()+"/tweets", bytes.NewReader(raw), "application/json; charset=utf-8")
}

// handleTweetResponse ports handle_tweet_response: a 2xx body with data.id is
// a success; otherwise, when media was attached and the error mentions
// "media", retry text-only (try_text_only_tweet).
func (p twitterPlatform) handleTweetResponse(ctx context.Context, client *http.Client, resp map[string]any, text, quoteTweetID string, mediaIDs []string, username string) (string, error) {
	if id := xDataID(resp); id != "" {
		return buildTweetURL(id, username), nil
	}
	errMsg := extractTweetErrorMessage(resp)
	if len(mediaIDs) > 0 && strings.Contains(strings.ToLower(errMsg), "media") {
		return p.tryTextOnlyTweet(ctx, client, text, quoteTweetID, errMsg, username)
	}
	return "", fmt.Errorf("twitter: tweet failed: %s", errMsg)
}

// tryTextOnlyTweet ports try_text_only_tweet: same tweet without media; a
// fallback failure reports the original media error too.
func (p twitterPlatform) tryTextOnlyTweet(ctx context.Context, client *http.Client, text, quoteTweetID, originalErr, username string) (string, error) {
	slog.Warn("twitter: media tweet failed, retrying text-only", "error", originalErr)
	resp, err := p.createTweet(ctx, client, buildTweetData(text, quoteTweetID, nil))
	if err != nil {
		if IsTransient(err) {
			return "", err
		}
		return "", fmt.Errorf("twitter: %s (fallback_failed: %s)", originalErr, err)
	}
	if id := xDataID(resp); id != "" {
		return buildTweetURL(id, username), nil
	}
	return "", fmt.Errorf("twitter: %s (fallback_failed: Fallback text tweet also failed)", originalErr)
}

// fetchUsername ports fetch_username: users/me cached for a week under a key
// bound to the access token; any failure yields "" (never cached, never
// transient) and the tweet URL falls back to the i/web/status form.
func (p twitterPlatform) fetchUsername(ctx context.Context, client *http.Client, accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	key := "twitter_username:" + hex.EncodeToString(sum[:])[:16]

	if cache := p.cache(); cache != nil {
		if raw, found, err := cache.Get(ctx, key); err == nil && found {
			var entry struct {
				Username string `json:"username"`
				CachedAt int64  `json:"cached_at"`
			}
			if err := json.Unmarshal([]byte(raw), &entry); err == nil && entry.Username != "" {
				if age := p.clock()().Unix() - entry.CachedAt; age >= 0 && age < int64(twitterUsernameCacheTTL/time.Second) {
					return entry.Username
				}
			}
		}
	}

	body, err := p.doJSON(ctx, client, http.MethodGet, p.base()+"/users/me", nil, "")
	if err != nil {
		slog.Warn("twitter: fetch username failed", "error", err)
		return ""
	}
	data, _ := body["data"].(map[string]any)
	username, _ := data["username"].(string)
	if username == "" {
		return ""
	}
	if cache := p.cache(); cache != nil {
		entry, err := json.Marshal(map[string]any{"username": username, "cached_at": p.clock()().Unix()})
		if err == nil {
			if err := cache.Set(ctx, key, string(entry)); err != nil {
				slog.Warn("twitter: write username cache", "error", err)
			}
		}
	}
	return username
}

// doJSON performs one OAuth1-signed request and decodes a JSON body. Error
// classification mirrors TransientNetworkErrors plus the x gem: network
// failures and 429/5xx responses are transient, other non-2xx are permanent
// and carry the extracted API error message (X::HTTPError#error_message).
func (p twitterPlatform) doJSON(ctx context.Context, client *http.Client, method, rawURL string, body io.Reader, contentType string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, transientNetError(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, transientNetError(err)
	}
	if resp.StatusCode/100 == 2 {
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, nil // Net::HTTPNoContent → nil
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, nil // ResponseParser rescues JSON::ParserError → nil
		}
		return out, nil
	}
	apiErr := fmt.Errorf("twitter: %s", xErrorMessage(resp, data))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5 {
		return nil, TransientError{Err: apiErr}
	}
	return nil, apiErr
}

// xErrorMessage ports X::HTTPError#error_message: a JSON errors array joins
// the messages, a title/detail pair combines, an "error" key passes through,
// otherwise the HTTP status text stands in (response.message).
func xErrorMessage(resp *http.Response, data []byte) string {
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err == nil {
		if errs, ok := parsed["errors"].([]any); ok {
			messages := make([]string, 0, len(errs))
			for _, e := range errs {
				if m, ok := e.(map[string]any)["message"].(string); ok {
					messages = append(messages, m)
				}
			}
			if len(messages) > 0 {
				return strings.Join(messages, ", ")
			}
		}
		title, hasTitle := parsed["title"].(string)
		detail, hasDetail := parsed["detail"].(string)
		if hasTitle && hasDetail {
			return title + ": " + detail
		}
		if e, ok := parsed["error"].(string); ok {
			return e
		}
	}
	return resp.Status
}

// xDataID ports successful_tweet_response?: response.dig("data", "id") as a
// string, tolerating both JSON string and number ids.
func xDataID(body map[string]any) string {
	data, ok := body["data"].(map[string]any)
	if !ok {
		return ""
	}
	switch id := data["id"].(type) {
	case string:
		return id
	case float64:
		return fmt.Sprintf("%.0f", id)
	default:
		return ""
	}
}

// extractTweetErrorMessage ports extract_error_message:
// response.dig("errors").first.dig("message") || "Unknown error".
func extractTweetErrorMessage(resp map[string]any) string {
	if errs, ok := resp["errors"].([]any); ok && len(errs) > 0 {
		if msg, ok := errs[0].(map[string]any)["message"].(string); ok && msg != "" {
			return msg
		}
	}
	return "Unknown error"
}

// buildTweetURL ports build_tweet_url.
func buildTweetURL(tweetID, username string) string {
	if tweetID == "" {
		return ""
	}
	if username != "" {
		return "https://x.com/" + username + "/status/" + tweetID
	}
	return "https://x.com/i/web/status/" + tweetID
}

// twitterStatusPathRe ports extract_tweet_id_from_path.
var twitterStatusPathRe = regexp.MustCompile(`(?i)^/(?:i/(?:web/)?status|[^/]+/status|statuses)/(\d+)(?:/.*)?$`)

var twitterSchemeRe = regexp.MustCompile(`(?i)^https?://`)

// twitterQuoteTweetID ports quote_tweet_id_for_article: the article's
// source_url pointing at an x.com/twitter.com status yields the tweet id.
func twitterQuoteTweetID(sourceURL string) string {
	u := normalizeTweetURL(sourceURL)
	if u == nil {
		return ""
	}
	if !isTwitterHost(strings.ToLower(u.Hostname())) {
		return ""
	}
	m := twitterStatusPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return ""
	}
	return m[1]
}

// normalizeTweetURL ports normalize_url: prepend https:// when the scheme is
// missing; unparseable URLs yield nil (URI::InvalidURIError rescue).
func normalizeTweetURL(raw string) *url.URL {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !twitterSchemeRe.MatchString(raw) {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	return u
}

// isTwitterHost ports twitter_host?.
func isTwitterHost(host string) bool {
	if host == "" {
		return false
	}
	for _, domain := range []string{"x.com", "twitter.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// ctxSleep sleeps d or until ctx is done (RateLimiter/await processing waits).
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
