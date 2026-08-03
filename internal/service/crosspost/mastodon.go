package crosspost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"rables/internal/db/query"
)

// mastodonPlatform ports MastodonService: bearer-token posting to a Mastodon
// server. The zero value is the registered instance; tests inject a client.
type mastodonPlatform struct {
	client *http.Client // nil → defaultHTTPClient
}

func init() { RegisterPlatform(mastodonPlatform{}) }

func (mastodonPlatform) Name() string { return "mastodon" }

func (p mastodonPlatform) http() *http.Client {
	if p.client != nil {
		return p.client
	}
	return defaultHTTPClient
}

// Verify ports MastodonService#verify: GET
// /api/v1/accounts/verify_credentials with the bearer token.
func (p mastodonPlatform) Verify(ctx context.Context, cfg query.Crosspost) error {
	token := cfg.AccessToken.String
	if token == "" {
		return errors.New("Access token are required")
	}
	u, err := mastodonAPIURL(cfg.ServerUrl.String, "/api/v1/accounts/verify_credentials")
	if err != nil {
		return errors.New("Server URL must be a valid http(s) URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("Mastodon verification failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.http().Do(req)
	if err != nil {
		return fmt.Errorf("Mastodon verification failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 == 2 {
		return nil
	}
	return fmt.Errorf("Verification failed: %s", resp.Status)
}

// Post ports MastodonService#post: upload each image (/api/v2/media), then
// create the status (/api/v1/statuses, form fields status/visibility=public/
// media_ids[]). A 429 backs off separately (plan §4.8); other non-2xx
// responses are permanent (Rails logs and returns nil).
func (p mastodonPlatform) Post(ctx context.Context, cfg query.Crosspost, in PostInput) (string, error) {
	if cfg.Enabled != 1 {
		return "", nil
	}
	token := cfg.AccessToken.String

	var mediaIDs []string
	for _, img := range in.Images {
		id, err := p.uploadMedia(ctx, cfg, token, img)
		if err != nil {
			if IsTransient(err) {
				return "", err
			}
			slog.Warn("mastodon: image upload failed", "filename", img.Filename, "error", err)
			continue
		}
		mediaIDs = append(mediaIDs, id)
	}

	u, err := mastodonAPIURL(cfg.ServerUrl.String, "/api/v1/statuses")
	if err != nil {
		return "", nil // Rails: return nil unless uri (misconfiguration, no retry)
	}
	form := url.Values{}
	form.Set("status", in.Text)
	form.Set("visibility", "public")
	for _, id := range mediaIDs {
		form.Add("media_ids[]", id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.http().Do(req)
	if err != nil {
		return "", transientNetError(err)
	}
	defer resp.Body.Close()
	logMastodonRateLimit(resp.Header)
	switch {
	case resp.StatusCode/100 == 2:
		var out struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("mastodon: decode status response: %w", err)
		}
		return out.URL, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", mastodonRateLimitError(resp.Header)
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("mastodon: create status: %s", resp.Status)
	}
}

// uploadMedia ports MastodonService#upload_image: multipart POST
// /api/v2/media with the file in the "file" part. Permanent failures skip
// the image (Rails returns nil); transient failures abort the post so the
// job retries.
func (p mastodonPlatform) uploadMedia(ctx context.Context, cfg query.Crosspost, token string, img Image) (string, error) {
	u, err := mastodonAPIURL(cfg.ServerUrl.String, "/api/v2/media")
	if err != nil {
		return "", nil
	}
	contentType := img.ContentType
	if contentType == "" {
		contentType = "image/jpeg"
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(img.Filename)))
	header.Set("Content-Type", contentType)
	part, err := mw.CreatePart(header)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(img.Data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.http().Do(req)
	if err != nil {
		return "", transientNetError(err)
	}
	defer resp.Body.Close()
	logMastodonRateLimit(resp.Header)
	switch {
	case resp.StatusCode/100 == 2:
		var out struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("mastodon: decode media response: %w", err)
		}
		return out.ID, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", mastodonRateLimitError(resp.Header)
	default:
		return "", fmt.Errorf("mastodon: upload media: %s", resp.Status)
	}
}

// escapeQuotes mirrors the Rails multipart filename escaping: CR/LF stripped,
// double quotes backslash-escaped, so the filename cannot break out of the
// Content-Disposition header.
func escapeQuotes(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// mastodonRateLimit ports parse_rate_limit_headers.
type mastodonRateLimit struct {
	Limit     int // -1 when the header is absent
	Remaining int
	ResetAt   time.Time
	HasReset  bool
}

func parseMastodonRateLimit(h http.Header) mastodonRateLimit {
	rl := mastodonRateLimit{Limit: -1, Remaining: -1}
	if v := h.Get("X-RateLimit-Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Limit = n
		}
	}
	if v := h.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Remaining = n
		}
	}
	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.ResetAt = time.Unix(n, 0).UTC()
			rl.HasReset = true
		}
	}
	return rl
}

// logMastodonRateLimit mirrors log_rate_limit_status (warn < 10, info < 50).
func logMastodonRateLimit(h http.Header) {
	rl := parseMastodonRateLimit(h)
	if rl.Remaining < 0 {
		return
	}
	switch {
	case rl.Remaining < 10:
		slog.Warn("mastodon: rate limit low", "remaining", rl.Remaining, "limit", rl.Limit, "reset_at", rl.ResetAt)
	case rl.Remaining < 50:
		slog.Info("mastodon: rate limit status", "remaining", rl.Remaining, "limit", rl.Limit)
	}
}

// mastodonRateLimitError builds the transient 429 error
// (handle_rate_limit_exceeded; the reset hint rides in the message).
func mastodonRateLimitError(h http.Header) error {
	rl := parseMastodonRateLimit(h)
	wait := time.Duration(0)
	if rl.HasReset {
		wait = max(time.Until(rl.ResetAt), 0)
	}
	return TransientError{Err: fmt.Errorf("mastodon: rate limited (429), reset in %ds", int(wait.Seconds()))}
}

// mastodonAPIURL ports mastodon_api_uri + normalized_server_uri: the server
// URL must be http(s) with a host and no userinfo; the path is normalized to
// leading/trailing slashes and the endpoint is joined onto it (URI.join).
func mastodonAPIURL(serverURL, endpoint string) (string, error) {
	raw := strings.TrimSpace(serverURL)
	if raw == "" {
		return "", errors.New("mastodon: blank server url")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("mastodon: invalid server url %q", serverURL)
	}

	host := u.Hostname()
	if port := u.Port(); port != "" && !isDefaultMastodonPort(u.Scheme, port) {
		host += ":" + port
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	base := &url.URL{Scheme: u.Scheme, Host: host, Path: path}
	ref := &url.URL{Path: strings.TrimPrefix(endpoint, "/")}
	return base.ResolveReference(ref).String(), nil
}

func isDefaultMastodonPort(scheme, port string) bool {
	return scheme == "http" && port == "80" || scheme == "https" && port == "443"
}
