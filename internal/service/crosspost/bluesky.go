package crosspost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // decode only; a GIF is flattened to its first frame like vips
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/disintegration/imaging"

	"rables/internal/db/query"
)

const (
	// blueskyDefaultServer is BlueskyService's default @server_url.
	blueskyDefaultServer = "https://bsky.social/xrpc"
	// blueskyPublicAPI hosts the unauthenticated XRPC endpoints.
	blueskyPublicAPI = "https://public.api.bsky.app"
	// blueskyTokenCacheKey mirrors BlueskyService::TOKEN_CACHE_KEY; entries
	// are per account ("<key>:<username>").
	blueskyTokenCacheKey = "bluesky_token_data"
	// blueskyTokenCacheTTL mirrors TOKEN_CACHE_TTL (1.hour).
	blueskyTokenCacheTTL = time.Hour
	// blueskyDIDCacheTTL mirrors the resolveHandle Rails.cache TTL (1.day).
	blueskyDIDCacheTTL = 24 * time.Hour
	// blueskyMaxImageSize mirrors BlueskyService::MAX_IMAGE_SIZE
	// (950.kilobytes, just under the 976.56KB protocol limit).
	blueskyMaxImageSize = 950 * 1024
)

// blueskyPlatform ports BlueskyService with hand-rolled XRPC calls (plan §6
// decision: no indigo). The zero value is the registered instance; tests
// inject client/cache/clock.
type blueskyPlatform struct {
	client *http.Client // nil → defaultHTTPClient
	tokens tokenCache   // nil → sharedTokens
	now    func() time.Time
	// publicBase overrides blueskyPublicAPI (the unauthenticated XRPC host);
	// tests point it at an httptest server.
	publicBase string
}

func init() { RegisterPlatform(blueskyPlatform{}) }

func (blueskyPlatform) Name() string { return "bluesky" }

func (p blueskyPlatform) http() *http.Client {
	if p.client != nil {
		return p.client
	}
	return defaultHTTPClient
}

func (p blueskyPlatform) clock() func() time.Time {
	if p.now != nil {
		return p.now
	}
	return time.Now
}

func (p blueskyPlatform) cache() tokenCache {
	if p.tokens != nil {
		return p.tokens
	}
	return sharedTokens
}

// publicAPI hosts the unauthenticated XRPC endpoints (getPostThread,
// resolveHandle).
func (p blueskyPlatform) publicAPI() string {
	if p.publicBase != "" {
		return p.publicBase
	}
	return blueskyPublicAPI
}

// blueskySession carries the token state BlueskyService keeps on the instance.
type blueskySession struct {
	token        string
	refreshToken string
	did          string
	expiresAt    time.Time
}

// Verify ports BlueskyService#verify: a fresh createSession with the given
// credentials. The session cache is never read or written — Rails restores
// the prior cache entry afterwards, so the net effect is no cache write.
func (p blueskyPlatform) Verify(ctx context.Context, cfg query.Crosspost) error {
	if cfg.Username.String == "" || cfg.AppPassword.String == "" {
		return errors.New("App Password and username are required")
	}
	server := cfg.ServerUrl.String
	if server == "" {
		server = blueskyDefaultServer
	}
	if _, err := p.createSession(ctx, server, cfg.Username.String, cfg.AppPassword.String); err != nil {
		return fmt.Errorf("Bluesky verification failed: %w", err)
	}
	return nil
}

// Post ports BlueskyService#post + #skeet: ensure a session, upload images
// as an app.bsky.embed.images embed, then createRecord with link facets.
func (p blueskyPlatform) Post(ctx context.Context, cfg query.Crosspost, in PostInput) (string, error) {
	if cfg.Enabled != 1 {
		return "", nil
	}
	server := cfg.ServerUrl.String
	if server == "" {
		server = blueskyDefaultServer
	}
	sess, err := p.ensureSession(ctx, server, cfg)
	if err != nil {
		return "", err
	}

	var embed map[string]any
	if len(in.Images) > 0 {
		embed, err = p.uploadImagesEmbed(ctx, server, &sess, in.Images)
		if err != nil {
			return "", err // transient only; permanent failures skip the image
		}
	}

	record := map[string]any{
		"text":      in.Text,
		"createdAt": p.clock()().Format(time.RFC3339),
		"facets":    linkFacets(in.Text),
	}
	if embed != nil {
		record["embed"] = embed
	}
	resp, err := p.postRequest(ctx, server+"/com.atproto.repo.createRecord", map[string]any{
		"repo":       sess.did,
		"collection": "app.bsky.feed.post",
		"record":     record,
	}, sess.token)
	if err != nil {
		return "", err
	}
	uri, _ := resp["uri"].(string)
	if uri == "" {
		return "", nil
	}
	rkey := uri[strings.LastIndex(uri, "/")+1:]
	return "https://bsky.app/profile/" + cfg.Username.String + "/post/" + rkey, nil
}

// ensureSession ports verify_tokens: reuse a valid cached token, refresh an
// expiring one (falling back to a full login), or log in fresh.
func (p blueskyPlatform) ensureSession(ctx context.Context, server string, cfg query.Crosspost) (blueskySession, error) {
	sess := p.cachedSession(ctx, cfg.Username.String)
	if sess.token == "" {
		return p.login(ctx, server, cfg)
	}
	if sess.expiresAt.Before(p.clock()().Add(60 * time.Second)) { // @token_expires_at < now + 60
		return p.refreshOrLogin(ctx, server, cfg, sess)
	}
	return sess, nil
}

// cachedSession reads the per-account session cache (Rails.cache with a 1h
// TTL; kv has no TTL, so the read timestamp rides inside the value).
// Corrupt, expired or unreadable entries are a miss.
func (p blueskyPlatform) cachedSession(ctx context.Context, username string) blueskySession {
	cache := p.cache()
	if cache == nil {
		return blueskySession{}
	}
	raw, found, err := cache.Get(ctx, blueskyTokenCacheKey+":"+username)
	if err != nil {
		slog.Warn("bluesky: read session cache", "error", err)
		return blueskySession{}
	}
	if !found {
		return blueskySession{}
	}
	var entry struct {
		AccessJWT  string `json:"access_jwt"`
		RefreshJWT string `json:"refresh_jwt"`
		DID        string `json:"did"`
		CachedAt   int64  `json:"cached_at"`
	}
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return blueskySession{}
	}
	if age := p.clock()().Unix() - entry.CachedAt; age < 0 || age >= int64(blueskyTokenCacheTTL/time.Second) {
		return blueskySession{}
	}
	expiresAt, err := jwtExpiry(entry.AccessJWT)
	if err != nil {
		return blueskySession{}
	}
	return blueskySession{
		token:        entry.AccessJWT,
		refreshToken: entry.RefreshJWT,
		did:          entry.DID,
		expiresAt:    expiresAt,
	}
}

// storeSession mirrors store_token_data (Rails.cache.write, 1h expiry).
func (p blueskyPlatform) storeSession(ctx context.Context, username string, sess blueskySession) {
	cache := p.cache()
	if cache == nil {
		return
	}
	entry, err := json.Marshal(map[string]any{
		"access_jwt":  sess.token,
		"refresh_jwt": sess.refreshToken,
		"did":         sess.did,
		"cached_at":   p.clock()().Unix(),
	})
	if err != nil {
		return
	}
	if err := cache.Set(ctx, blueskyTokenCacheKey+":"+username, string(entry)); err != nil {
		slog.Warn("bluesky: write session cache", "error", err)
	}
}

// login ports generate_tokens: createSession + cache store.
func (p blueskyPlatform) login(ctx context.Context, server string, cfg query.Crosspost) (blueskySession, error) {
	sess, err := p.createSession(ctx, server, cfg.Username.String, cfg.AppPassword.String)
	if err != nil {
		return blueskySession{}, err
	}
	p.storeSession(ctx, cfg.Username.String, sess)
	return sess, nil
}

func (p blueskyPlatform) createSession(ctx context.Context, server, username, password string) (blueskySession, error) {
	resp, err := p.postRequest(ctx, server+"/com.atproto.server.createSession", map[string]any{
		"identifier": username,
		"password":   password,
	}, "")
	if err != nil {
		return blueskySession{}, err
	}
	return p.processTokens(resp)
}

// refreshOrLogin ports refresh_or_regenerate_tokens: one refresh attempt,
// then one full login when the refresh token was rejected.
func (p blueskyPlatform) refreshOrLogin(ctx context.Context, server string, cfg query.Crosspost, sess blueskySession) (blueskySession, error) {
	resp, err := p.postRequest(ctx, server+"/com.atproto.server.refreshSession", nil, sess.refreshToken)
	if err == nil {
		refreshed, perr := p.processTokens(resp)
		if perr == nil {
			p.storeSession(ctx, cfg.Username.String, refreshed)
			return refreshed, nil
		}
		err = perr
	}
	slog.Warn("bluesky: token refresh failed, falling back to login", "error", err)
	return p.login(ctx, server, cfg)
}

// processTokens ports process_tokens: pull accessJwt/refreshJwt/did out of
// the session response and decode the token expiry.
func (p blueskyPlatform) processTokens(resp map[string]any) (blueskySession, error) {
	token, _ := resp["accessJwt"].(string)
	if token == "" {
		return blueskySession{}, errors.New("bluesky: session response missing accessJwt")
	}
	expiresAt, err := jwtExpiry(token)
	if err != nil {
		return blueskySession{}, fmt.Errorf("bluesky: decode access token: %w", err)
	}
	refreshToken, _ := resp["refreshJwt"].(string)
	did, _ := resp["did"].(string)
	return blueskySession{token: token, refreshToken: refreshToken, did: did, expiresAt: expiresAt}, nil
}

// jwtExpiry ports decode_jwt_payload: the payload segment is unpadded
// base64url; the exp claim becomes the expiry time.
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, errors.New("malformed jwt")
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return time.Time{}, err
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return time.Time{}, err
	}
	return time.Unix(claims.Exp, 0).UTC(), nil
}

// postRequest ports BlueskyService#post_request: JSON POST with an optional
// bearer token ("" sends none, like auth_token: false). 5xx/429 raise the
// transient server error; other non-2xx responses raise plainly.
func (p blueskyPlatform) postRequest(ctx context.Context, endpoint string, body any, bearer string) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := p.http().Do(req)
	if err != nil {
		return nil, transientNetError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := fmt.Sprintf("%d response - %s", resp.StatusCode, raw)
		if resp.StatusCode/100 == 5 || resp.StatusCode == http.StatusTooManyRequests {
			return nil, TransientError{Err: errors.New(msg)}
		}
		return nil, errors.New(msg)
	}
	out := map[string]any{}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("bluesky: decode response: %w", err)
		}
	}
	return out, nil
}

// uploadImagesEmbed ports upload_images_embed: compress-and-upload each
// image, then build app.bsky.embed.images. Images that fail permanently are
// skipped; when none upload there is no embed (the post still goes out).
func (p blueskyPlatform) uploadImagesEmbed(ctx context.Context, server string, sess *blueskySession, images []Image) (map[string]any, error) {
	type uploaded struct {
		blob     map[string]any
		filename string
	}
	var uploadedImages []uploaded
	for _, img := range images {
		blob, err := p.uploadBlob(ctx, server, sess, img)
		if err != nil {
			if IsTransient(err) {
				return nil, err
			}
			slog.Warn("bluesky: image upload failed", "filename", img.Filename, "error", err)
			continue
		}
		if blob == nil {
			continue
		}
		uploadedImages = append(uploadedImages, uploaded{blob: blob, filename: img.Filename})
	}
	if len(uploadedImages) == 0 {
		return nil, nil
	}
	embedImages := make([]map[string]any, 0, len(uploadedImages))
	for _, u := range uploadedImages {
		embedImages = append(embedImages, map[string]any{"alt": u.filename, "image": u.blob})
	}
	return map[string]any{"$type": "app.bsky.embed.images", "images": embedImages}, nil
}

// uploadBlob ports upload_blob/upload_remote_image: oversize images are
// compressed first (a failed compression skips the image), then the bytes go
// to com.atproto.repo.uploadBlob. Like the Rails raw Net::HTTP post, ANY
// non-2xx response skips the image (nil); only network-level failures are
// transient.
func (p blueskyPlatform) uploadBlob(ctx context.Context, server string, sess *blueskySession, img Image) (map[string]any, error) {
	data, contentType := img.Data, img.ContentType
	if len(data) > blueskyMaxImageSize {
		compressed, ok := compressImage(data, blueskyMaxImageSize)
		if !ok {
			return nil, nil // resize_image_if_needed → nil
		}
		data, contentType = compressed, "image/jpeg"
	}
	if contentType == "" {
		contentType = "image/jpeg"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/com.atproto.repo.uploadBlob", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+sess.token)

	resp, err := p.http().Do(req)
	if err != nil {
		return nil, transientNetError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		slog.Warn("bluesky: blob upload failed", "status", resp.Status)
		return nil, nil
	}
	var out struct {
		Blob map[string]any `json:"blob"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("bluesky: decode blob response: %w", err)
	}
	return out.Blob, nil
}

// compressImage ports ImageCompressor#compress_image with imaging in place of
// libvips: JPEG re-encode starting at quality 85, dropping by 10 down to 50,
// then scaling by sqrt(max/current)*0.95 (clamped to [0.5, 0.9], quality
// reset to 85) until the output fits maxSize. Images that would shrink below
// 100px give up (the Rails nil). EXIF autorotation is not applied (known gap,
// same as media variants); alpha is flattened onto white for JPEG.
func compressImage(data []byte, maxSize int) ([]byte, bool) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	img := flattenOnWhite(src)
	quality := 85
	for {
		var buf bytes.Buffer
		if err := imaging.Encode(&buf, img, imaging.JPEG, imaging.JPEGQuality(quality)); err != nil {
			return nil, false
		}
		if buf.Len() <= maxSize {
			return buf.Bytes(), true
		}
		if quality > 50 {
			quality -= 10
			continue
		}
		scale := math.Sqrt(float64(maxSize)/float64(buf.Len())) * 0.95
		scale = math.Min(scale, 0.9)
		scale = math.Max(scale, 0.5)
		w := int(float64(img.Bounds().Dx()) * scale)
		h := int(float64(img.Bounds().Dy()) * scale)
		if w < 100 || h < 100 { // MIN_IMAGE_DIMENSION
			return nil, false
		}
		img = imaging.Resize(img, w, h, imaging.Lanczos)
		quality = 85
	}
}

// flattenOnWhite composites the image over white (Vips flatten for JPEG).
func flattenOnWhite(src image.Image) image.Image {
	bounds := src.Bounds()
	dst := image.NewNRGBA(bounds)
	draw.Draw(dst, bounds, image.White, image.Point{}, draw.Src)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Over)
	return dst
}

// bskyLinkRe approximates the URI::RFC2396_PARSER.make_regexp(["http",
// "https"]) used by link_facets: an http(s) scheme followed by RFC 2396 uric
// characters plus the fragment marker (verified against the Ruby regexp: CJK
// and brackets stop the match, '#'/parens/','/';' are included, a bare '%'
// terminates it).
var bskyLinkRe = regexp.MustCompile(`https?://(?:[A-Za-z0-9\-_.!~*'();/?:@&=+$,#]|%[0-9A-Fa-f]{2})+`)

// linkFacets ports BlueskyService#link_facets: every http(s) URL in the text
// becomes an app.bsky.richtext.facet#link facet with BYTE offsets.
func linkFacets(message string) []map[string]any {
	var facets []map[string]any
	for _, loc := range bskyLinkRe.FindAllStringIndex(message, -1) {
		raw := message[loc[0]:loc[1]]
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue // next unless uri.scheme && uri.host
		}
		facets = append(facets, map[string]any{
			"index":    map[string]any{"byteStart": loc[0], "byteEnd": loc[1]},
			"features": []map[string]any{{"uri": raw, "$type": "app.bsky.richtext.facet#link"}},
		})
	}
	return facets
}

// ResolveHandle ports resolve_handle_to_did (public API, DID cached 1 day;
// failures return "" and are never cached — Rails skip_nil). T22's comment
// fetch resolves post author handles through this.
func (p blueskyPlatform) ResolveHandle(ctx context.Context, handle string) string {
	if did, ok := p.cachedDID(ctx, handle); ok {
		return did
	}
	endpoint := p.publicAPI() + "/xrpc/com.atproto.identity.resolveHandle?handle=" + url.QueryEscape(handle)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	resp, err := p.http().Do(req)
	if err != nil {
		slog.Warn("bluesky: handle resolution failed", "handle", handle, "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		slog.Warn("bluesky: handle resolution failed", "handle", handle, "status", resp.Status)
		return ""
	}
	var out struct {
		DID string `json:"did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.DID == "" {
		return ""
	}
	p.storeDID(ctx, handle, out.DID)
	return out.DID
}

// cachedDID reads the handle→DID cache (Rails.cache.fetch, 1.day).
func (p blueskyPlatform) cachedDID(ctx context.Context, handle string) (string, bool) {
	cache := p.cache()
	if cache == nil {
		return "", false
	}
	raw, found, err := cache.Get(ctx, "bluesky_did:"+handle)
	if err != nil || !found {
		return "", false
	}
	var entry struct {
		DID      string `json:"did"`
		CachedAt int64  `json:"cached_at"`
	}
	if err := json.Unmarshal([]byte(raw), &entry); err != nil || entry.DID == "" {
		return "", false
	}
	if age := p.clock()().Unix() - entry.CachedAt; age < 0 || age >= int64(blueskyDIDCacheTTL/time.Second) {
		return "", false
	}
	return entry.DID, true
}

func (p blueskyPlatform) storeDID(ctx context.Context, handle, did string) {
	cache := p.cache()
	if cache == nil {
		return
	}
	entry, err := json.Marshal(map[string]any{"did": did, "cached_at": p.clock()().Unix()})
	if err != nil {
		return
	}
	if err := cache.Set(ctx, "bluesky_did:"+handle, string(entry)); err != nil {
		slog.Warn("bluesky: write did cache", "error", err)
	}
}
