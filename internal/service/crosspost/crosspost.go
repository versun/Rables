// Package crosspost ports the Rails social crosspost services
// (MastodonService / BlueskyService / CrosspostArticleJob) behind a platform
// registry. Platforms register themselves from init(); the job dispatcher
// (kind=crosspost) iterates the requested platforms, skips posts whose URL is
// already recorded (CrosspostArticleJob url_recorded_since?), and records
// successful post URLs in social_media_posts.
package crosspost

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"crypto/tls"
	"crypto/x509"

	"golang.org/x/net/html"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	"rables/internal/kv"
	"rables/internal/service/activity"
	"rables/internal/service/media"
)

// Platform is one crosspost target (mastodon, bluesky, ...). Implementations
// live in their own files and self-register via RegisterPlatform from init,
// so adding a platform never touches the dispatcher.
type Platform interface {
	Name() string
	// Verify checks credentials like the Rails service #verify: nil on
	// success, otherwise an error carrying the Rails failure message.
	Verify(ctx context.Context, cfg query.Crosspost) error
	// Post publishes in and returns the post URL. Permanent failures return
	// a non-nil error; transient network failures return a TransientError so
	// the caller retries (TransientNetworkErrors + CrosspostArticleJob
	// retry_on). ("", nil) means "not posted, nothing to retry" (disabled
	// config, unusable server URL), mirroring the Rails nil return.
	Post(ctx context.Context, cfg query.Crosspost, in PostInput) (postURL string, err error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Platform{}
)

// RegisterPlatform installs p, replacing any previous registration with the
// same name (tests use this to substitute fakes).
func RegisterPlatform(p Platform) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[p.Name()] = p
}

// Get returns the registered platform, or nil when the name is unknown or
// the platform is not implemented yet.
func Get(name string) Platform {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[name]
}

// Image is one resolved post image (an all_image_attachments entry), bytes
// already read from local storage or downloaded from the remote URL.
type Image struct {
	Filename    string
	ContentType string
	Data        []byte
}

// PostInput carries everything a platform needs for one post: the
// ContentBuilder result plus up to 4 images in body order.
type PostInput struct {
	ArticleID int64
	Title     string
	Slug      string
	Text      string
	Images    []Image
	// SourceURL is article.source_url (has_source?). The twitter platform
	// uses it for quote-tweet detection (quote_tweet_id_for_article).
	SourceURL string
}

// TransientError wraps failures that are safe to retry
// (TransientNetworkErrors::TRANSIENT_ERRORS plus
// TransientNetworkErrors::TransientServerError for 5xx/429 API responses).
type TransientError struct{ Err error }

func (e TransientError) Error() string { return e.Err.Error() }
func (e TransientError) Unwrap() error { return e.Err }

// IsTransient reports whether err is a retryable failure.
func IsTransient(err error) bool {
	var te TransientError
	return errors.As(err, &te)
}

// transientNetError maps Go network errors onto the TransientNetworkErrors
// whitelist: Timeout::Error, SocketError and SystemCallError (net.Error,
// os.SyscallError, syscall.Errno), EOFError (io.EOF & co.) and
// OpenSSL::SSL::SSLError (TLS record/certificate errors). Anything else is
// returned unchanged.
func transientNetError(err error) error {
	if err == nil {
		return nil
	}
	var netErr net.Error // timeouts, *net.OpError socket errors, *net.DNSError
	if errors.As(err, &netErr) {
		return TransientError{Err: err}
	}
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		return TransientError{Err: err}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return TransientError{Err: err}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return TransientError{Err: err}
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return TransientError{Err: err}
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return TransientError{Err: err}
	}
	var authErr x509.UnknownAuthorityError
	if errors.As(err, &authErr) {
		return TransientError{Err: err}
	}
	return err
}

// defaultHTTPClient bounds every platform API call; the Rails services use
// ~5s open/read timeouts (HttpRedirectHandler allows 10s).
var defaultHTTPClient = &http.Client{Timeout: 15 * time.Second}

// downloadClient fetches remote images, mirroring HttpRedirectHandler:
// MAX_REDIRECTS hops, http(s) only, and no https -> http downgrade.
var downloadClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many HTTP redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("refusing to fetch non-http(s) URL: %s", req.URL)
		}
		if from := via[len(via)-1].URL; from.Scheme == "https" && req.URL.Scheme == "http" {
			return fmt.Errorf("refusing redirect that downgrades https to http: %s", req.URL)
		}
		return nil
	},
}

// maxDownloadBytes mirrors HttpRedirectHandler::MAX_DOWNLOAD_BYTES.
const maxDownloadBytes = 20 * 1024 * 1024

// tokenCache is the kv-backed slice of Rails.cache the bluesky platform needs.
type tokenCache interface {
	Get(ctx context.Context, key string) (value string, found bool, err error)
	Set(ctx context.Context, key, value string) error
}

// sharedTokens backs bluesky session/DID caching once
// RegisterCrosspostHandlers wires the job worker. Verify never touches it
// (Rails BlueskyService#verify restores the cache entry afterwards, so the
// net effect is no cache write).
var sharedTokens tokenCache

// Dispatcher executes kind=crosspost jobs (CrosspostArticleJob).
type Dispatcher struct {
	DB    *sql.DB
	Media *media.Service
	Log   *slog.Logger

	// RoutePrefix is ARTICLE_ROUTE_PREFIX for the Read-more link; defaults
	// to the environment value, like config.Load.
	RoutePrefix string
	// HTTPClient downloads remote images; nil uses downloadClient.
	HTTPClient *http.Client

	q   *query.Queries
	now func() time.Time
}

// NewDispatcher builds a Dispatcher rooted at the data directory.
func NewDispatcher(db *sql.DB, dataDir string) *Dispatcher {
	return &Dispatcher{
		DB:          db,
		Media:       media.New(db, dataDir),
		Log:         slog.Default(),
		RoutePrefix: os.Getenv("ARTICLE_ROUTE_PREFIX"),
		q:           query.New(db),
		now:         time.Now,
	}
}

// RegisterCrosspostHandlers installs the kind=crosspost job handler
// (CrosspostArticleJob). The integrator wires it in cmd/server/main.go next
// to the other Register*Handlers calls.
func RegisterCrosspostHandlers(w *jobs.Worker, db *sql.DB, dataDir string) {
	d := NewDispatcher(db, dataDir)
	sharedTokens = kv.NewStore(db)
	w.Register(jobs.KindCrosspost, d.Handle)
}

// jobPayload accepts both enqueue shapes in use: {"article_id", "platform",
// "requested_at"} from the article save / batch paths, and {"article_id",
// "platforms": [...]} from the scheduled-publish worker (T14).
type jobPayload struct {
	ArticleID   int64    `json:"article_id"`
	Platform    string   `json:"platform"`
	Platforms   []string `json:"platforms"`
	RequestedAt int64    `json:"requested_at"`
}

func (p jobPayload) platformList() []string {
	if len(p.Platforms) > 0 {
		return p.Platforms
	}
	if p.Platform != "" {
		return []string{p.Platform}
	}
	return nil
}

// Handle runs one crosspost job. Transient failures abort the run so the
// worker retries with its backoff ladder (5 attempts, like retry_on
// attempts: 5); platforms that already succeeded are then skipped via their
// recorded URLs. Permanent failures are logged and never block the remaining
// platforms, matching the Rails service rescues.
func (d *Dispatcher) Handle(ctx context.Context, raw json.RawMessage) error {
	var p jobPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode crosspost payload: %w", err)
	}
	article, err := d.q.GetAdminArticleByID(ctx, p.ArticleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Article.find_by + return unless article
	}
	if err != nil {
		return fmt.Errorf("load article %d: %w", p.ArticleID, err)
	}

	posted := map[string]string{}
	for _, name := range p.platformList() {
		platform := Get(name)
		if platform == nil {
			d.Log.Warn("crosspost: platform not implemented", "platform", name)
			continue
		}
		cfg, err := d.q.GetCrosspostByPlatform(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			continue // Crosspost.for creates a disabled row; posting is a no-op
		}
		if err != nil {
			return fmt.Errorf("load crosspost config %q: %w", name, err)
		}
		if cfg.Enabled != 1 {
			continue
		}
		skip, err := d.urlRecordedSince(ctx, article.ID, name, p.RequestedAt)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		in, err := d.buildInput(ctx, article, cfg)
		if err != nil {
			return fmt.Errorf("build content for %q: %w", name, err)
		}
		postURL, err := platform.Post(ctx, cfg, in)
		if err != nil {
			if IsTransient(err) {
				return err
			}
			d.logActivity(ctx, "error", "failed", name, article, "", err.Error())
			continue
		}
		if postURL == "" {
			continue
		}
		if err := d.recordURL(ctx, article.ID, name, postURL); err != nil {
			return err
		}
		posted[name] = postURL
		d.logActivity(ctx, "info", "posted", name, article, postURL, "")
	}
	if len(posted) > 0 {
		// CrosspostArticleJob's own success row (platforms list).
		names := make([]string, 0, len(posted))
		for name := range posted {
			names = append(names, name)
		}
		sort.Strings(names)
		activity.Log(ctx, d.DB, "info", "posted", "crosspost",
			fmt.Sprintf("platforms=%s title=%s slug=%s", strings.Join(names, ","),
				activity.Quote(article.Title.String), activity.Quote(article.Slug.String)))
	}
	return nil
}

// urlRecordedSince ports CrosspostArticleJob#url_recorded_since?: a post with
// a URL recorded at or after requestedAt blocks a re-post, so a retry never
// publishes a duplicate. A zero requestedAt is the legacy shape (enqueued
// without a timestamp), which conservatively skips on any recorded URL.
//
// Rails applies the guard only on retries (executions > 1); applying it
// unconditionally is equivalent here because a URL newer than requestedAt
// can only come from an earlier execution of this same request (or a racing
// job for the same article+platform).
func (d *Dispatcher) urlRecordedSince(ctx context.Context, articleID int64, platform string, requestedAt int64) (bool, error) {
	posts, err := d.q.ListFetchableSocialPostsByPlatform(ctx, query.ListFetchableSocialPostsByPlatformParams{
		ArticleID: articleID, Platform: platform,
	})
	if err != nil {
		return false, fmt.Errorf("check recorded url for %q: %w", platform, err)
	}
	if len(posts) == 0 {
		return false, nil
	}
	if requestedAt <= 0 {
		return true, nil
	}
	for _, post := range posts {
		if post.UpdatedAt >= requestedAt {
			return true, nil
		}
	}
	return false, nil
}

// recordURL mirrors social_media_posts.find_or_initialize_by(platform:).update!(url:).
func (d *Dispatcher) recordURL(ctx context.Context, articleID int64, platform, postURL string) error {
	now := d.now().Unix()
	if err := d.q.UpsertSocialMediaPost(ctx, query.UpsertSocialMediaPostParams{
		ArticleID: articleID, Platform: platform, Url: postURL,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("record %q post url: %w", platform, err)
	}
	return nil
}

// buildInput builds the PostInput: build_content via domain.BuildContent plus
// the article's images.
func (d *Dispatcher) buildInput(ctx context.Context, article query.Article, cfg query.Crosspost) (PostInput, error) {
	siteURL, err := d.siteURL(ctx)
	if err != nil {
		return PostInput{}, err
	}
	maxChars := 0
	if cfg.MaxCharacters.Valid {
		maxChars = int(cfg.MaxCharacters.Int64)
	}
	text := domain.BuildContent(domain.ContentInput{
		Slug:        article.Slug.String,
		Title:       article.Title.String,
		PlainText:   domain.PlainText(article.ContentHtml.String),
		Description: article.Description.String,
		SourceURL:   article.SourceUrl.String, // has_source? == source_url.present?
	}, domain.BuildOptions{
		MaxLength:           domain.EffectiveMaxCharacters(cfg.Platform, maxChars),
		CountNonASCIIDouble: domain.PlatformCountNonASCIIDouble(cfg.Platform),
		SiteURL:             siteURL,
		RoutePrefix:         d.RoutePrefix,
	})
	return PostInput{
		ArticleID: article.ID,
		Title:     article.Title.String,
		Slug:      article.Slug.String,
		Text:      text,
		Images:    d.collectImages(ctx, article),
		SourceURL: article.SourceUrl.String,
	}, nil
}

// siteURL is CacheableSettings.site_info[:url] ("" when unset; callers fall
// back like the Rails services).
func (d *Dispatcher) siteURL(ctx context.Context) (string, error) {
	settings, err := d.q.GetSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load settings: %w", err)
	}
	return settings.Url.String, nil
}

// collectImages ports Article#all_image_attachments(4): attached image files
// first (deduplicated by file id), then <img> srcs from the HTML — local
// /files/<key> reads or remote downloads. Failures skip the image, like the
// Rails per-image rescues.
func (d *Dispatcher) collectImages(ctx context.Context, article query.Article) []Image {
	const limit = 4
	var images []Image
	seen := map[int64]bool{}

	files, err := d.q.ListImageAttachmentsForArticle(ctx, article.ID)
	if err != nil {
		d.Log.Warn("crosspost: list article image attachments", "article_id", article.ID, "error", err)
	}
	for _, f := range files {
		if len(images) >= limit {
			break
		}
		seen[f.ID] = true
		if img, ok := d.loadLocalImage(f); ok {
			images = append(images, img)
		}
	}

	if len(images) < limit {
		for _, src := range imageSrcs(article.ContentHtml.String) {
			if len(images) >= limit {
				break
			}
			if f, ok := d.localFileBySrc(ctx, src); ok {
				if seen[f.ID] { // existing_blob_ids dedupe
					continue
				}
				seen[f.ID] = true
				if img, ok := d.loadLocalImage(f); ok {
					images = append(images, img)
				}
				continue
			}
			if img, ok := d.downloadRemoteImage(ctx, src); ok {
				images = append(images, img)
			}
		}
	}
	return images
}

// loadLocalImage reads an attached/linked file from disk via media.Service.
func (d *Dispatcher) loadLocalImage(f query.File) (Image, bool) {
	if !media.ValidKey(f.Key) {
		return Image{}, false
	}
	data, err := os.ReadFile(d.Media.PathFor(f.Key))
	if err != nil {
		d.Log.Warn("crosspost: read image file", "key", f.Key, "error", err)
		return Image{}, false
	}
	contentType := f.ContentType.String
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	return Image{Filename: f.Filename, ContentType: contentType, Data: data}, true
}

// localFileBySrc resolves an <img src="/files/<key>"> to its files row; ok is
// false for any other src (which then falls back to a remote download, like
// RemoteImageWrapper).
func (d *Dispatcher) localFileBySrc(ctx context.Context, src string) (query.File, bool) {
	key, ok := strings.CutPrefix(src, "/files/")
	if !ok || !media.ValidKey(key) {
		return query.File{}, false
	}
	f, err := d.q.GetFileByKey(ctx, key)
	if err != nil {
		return query.File{}, false
	}
	if !strings.HasPrefix(f.ContentType.String, "image/") {
		return query.File{}, false
	}
	return f, true
}

// downloadRemoteImage ports HttpRedirectHandler#download_remote_image_with_redirect:
// relative URLs resolve against the site URL, bodies are capped at 20MB, and
// any failure skips the image (the Rails concern rescues everything,
// including timeouts).
func (d *Dispatcher) downloadRemoteImage(ctx context.Context, imageURL string) (Image, bool) {
	if imageURL == "" {
		return Image{}, false
	}
	if strings.HasPrefix(imageURL, "/") {
		siteURL, err := d.siteURL(ctx)
		if err != nil || siteURL == "" {
			siteURL = "http://localhost:3000"
		}
		imageURL = strings.TrimSuffix(siteURL, "/") + imageURL
	}
	u, err := url.Parse(imageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return Image{}, false
	}
	client := d.HTTPClient
	if client == nil {
		client = downloadClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return Image{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		d.Log.Warn("crosspost: remote image download failed", "url", imageURL, "error", err)
		return Image{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Image{}, false
	}
	if resp.ContentLength > maxDownloadBytes {
		return Image{}, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil || len(data) > maxDownloadBytes {
		return Image{}, false
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "image/jpeg"
	}
	filename := imageFilename(u)
	return Image{Filename: filename, ContentType: contentType, Data: data}, true
}

// imageFilename mirrors the Rails File.basename(URI.parse(url).path) with the
// "image.jpg" fallback.
func imageFilename(u *url.URL) string {
	name := u.Path[strings.LastIndex(u.Path, "/")+1:]
	if name == "" {
		return "image.jpg"
	}
	return name
}

// imageSrcs returns the src attribute of every <img> in the HTML fragment,
// in document order (Nokogiri css("img")), skipping blank ones.
func imageSrcs(rawHTML string) []string {
	if rawHTML == "" || !utf8.ValidString(rawHTML) {
		return nil
	}
	nodes, err := html.ParseFragment(strings.NewReader(rawHTML), nil)
	if err != nil {
		return nil
	}
	var srcs []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			for _, attr := range n.Attr {
				if attr.Key == "src" {
					if s := strings.TrimSpace(attr.Val); s != "" {
						srcs = append(srcs, s)
					}
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return srcs
}

// logActivity writes one per-platform crosspost activity row, mirroring the
// ActivityLog.log! calls inside the Rails services (title/slug/platform/url /
// error ride in the description as key=value pairs).
func (d *Dispatcher) logActivity(ctx context.Context, level, action, platform string, article query.Article, postURL, errMsg string) {
	desc := fmt.Sprintf("platform=%s title=%s slug=%s", platform,
		activity.Quote(article.Title.String), activity.Quote(article.Slug.String))
	if postURL != "" {
		desc += " url=" + postURL
	}
	if errMsg != "" {
		desc += " error=" + activity.Quote(errMsg)
	}
	activity.Log(ctx, d.DB, level, action, "crosspost", desc)
}
