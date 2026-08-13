// RSS import (plan T26, section 4.11): fetches a feed and creates one
// published article per entry, mirroring ImportRss (app/models/import_rss.rb).
// The SSRF guard is ported 1:1: only http(s) URLs whose host resolves to
// public IPs pass, DNS answers are checked address by address, redirects to
// blocked targets are refused, and DNS failures count as unsafe. The Go
// hardening per the plan's decision record caps feed bodies at 20MB.
package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/media"
	"rables/internal/ssrf"
)

// MaxFeedBodyBytes caps RSS response bodies (plan decision record: 20MB).
const MaxFeedBodyBytes = 20 << 20

// MaxImportImages caps how many distinct images one importer downloads; img
// tags past the cap keep their original src, like failed downloads.
const MaxImportImages = 200

// MaxImportItems caps how many feed entries one import processes. The 20MB
// body cap only bounds bytes: a feed of minimal entries (~200 bytes each)
// could otherwise insert ~100k published articles in one run. Entries past
// the cap count as failed so the overflow shows up in the result.
const MaxImportItems = 5000

// MaxItemContentBytes caps a single feed entry's content for the image
// rewrite pass: html.ParseFragment bounds nesting depth but not the total
// node count, so a huge item would explode into millions of nodes (same
// rationale as domain's maxHTMLParseBytes). Over the cap the walk is
// skipped: imgs keep their original src, and the content is still sanitized
// downstream in importEntry.
const MaxItemContentBytes = 5 << 20

// ImportRSSPayload is the job_runs payload for kind "import_rss".
type ImportRSSPayload struct {
	URL          string `json:"url"`
	ImportImages bool   `json:"import_images"`
}

// RSSImporter imports one feed into articles (ImportRss).
type RSSImporter struct {
	DB      *sql.DB
	DataDir string
	// Media stores downloaded images; nil builds one from DB/DataDir.
	Media *media.Service
	// LookupIP resolves a host for the SSRF check (tests stub it); nil uses
	// the system resolver. The check result is what matters - the HTTP
	// client still dials the original host.
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
	// Now overrides the clock; nil uses time.Now.
	Now func() time.Time

	resolved map[string][]netip.Addr // per-host memo, like resolved_addresses_for
	images   map[string]string      // image src → local /files/ URL memo; "" marks a failed download
	seenSrcs map[string]struct{}    // distinct img srcs that reached the SSRF check, capped at MaxImportImages
}

// RSSImportResult counts imported and failed entries.
type RSSImportResult struct {
	Imported int
	Failed   int
}

// Import fetches and imports the feed, mirroring ImportRss#import_data:
// entries without a link are skipped, per-entry failures are counted and do
// not abort the run, and at most MaxImportItems entries are processed.
func (r *RSSImporter) Import(ctx context.Context, feedURL string, importImages bool) (*RSSImportResult, error) {
	if !r.safeRemoteURL(ctx, feedURL) {
		return nil, fmt.Errorf("import rss: unsafe feed URL: %s", feedURL)
	}
	body, err := r.fetch(ctx, feedURL)
	if err != nil {
		return nil, err
	}
	feed, err := gofeed.NewParser().ParseString(string(body))
	if err != nil {
		return nil, fmt.Errorf("import rss: parse feed: %w", err)
	}

	result := &RSSImportResult{}
	items := feed.Items
	if len(items) > MaxImportItems {
		slog.Default().Warn("import rss: item count over the cap, extra entries skipped", "count", len(items), "cap", MaxImportItems)
		result.Failed += len(items) - MaxImportItems
		items = items[:MaxImportItems]
	}
	for _, item := range items {
		if item.Link == "" {
			continue
		}
		if err := r.importEntry(ctx, item, importImages); err != nil {
			result.Failed++
			slog.Default().Warn("import rss: entry failed", "url", item.Link, "error", err)
			continue
		}
		result.Imported++
	}
	return result, nil
}

// importEntry mirrors ImportRss#import_entry: slug from the last path segment
// of the unescaped link, title falling back to the published timestamp,
// status publish, description from the summary.
func (r *RSSImporter) importEntry(ctx context.Context, item *gofeed.Item, importImages bool) error {
	decodedLink, err := url.QueryUnescape(item.Link) // CGI.unescape
	if err != nil {
		decodedLink = item.Link
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	published := item.PublishedParsed
	title := item.Title
	if title == "" && published != nil {
		title = published.UTC().Format("2006-01-02 15:04:05 MST") // Time#to_s
	}
	content := item.Content
	if importImages && content != "" {
		content = r.rewriteImages(ctx, content, title)
	}
	// Feed content is untrusted: content_html is served verbatim
	// (template.HTML) on the public pages, so it goes through the same
	// sanitize/lazy-load write path as articles saved in the admin.
	content = domain.AddLazyLoading(domain.SanitizeHTML(content))

	q := query.New(r.DB)
	exists := func(candidate string) bool {
		_, err := q.ImportArticleIDBySlug(ctx, sql.NullString{String: candidate, Valid: true})
		return err == nil
	}
	slug := domain.GenerateSlug(lastURLSegment(decodedLink), title, now, exists)
	// A last segment of only dots ("..", "...") strips to a blank slug,
	// which would insert an article no URL can reach.
	if domain.IsBlank(slug) {
		return fmt.Errorf("link %q yields a blank slug", decodedLink)
	}
	if domain.IsReservedSlug(slug) || slices.Contains(domain.AdminArticleBatchSlugs, slug) {
		return fmt.Errorf("slug %q is reserved", slug)
	}
	if exists(slug) {
		return fmt.Errorf("slug %q already exists", slug)
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("content blank")
	}

	createdAt := now.Unix()
	if published != nil {
		createdAt = published.Unix()
	}
	_, err = q.ImportInsertArticle(ctx, query.ImportInsertArticleParams{
		Title:                       sql.NullString{String: title, Valid: title != ""},
		Slug:                        sql.NullString{String: slug, Valid: true},
		ContentHtml:                 sql.NullString{String: content, Valid: true},
		ContentType:                 string(domain.ContentTypeRichText),
		Description:                 sql.NullString{String: item.Description, Valid: item.Description != ""},
		Status:                      int64(domain.StatusPublish),
		Comment:                     0,
		ScheduledCrosspostPlatforms: "[]",
		ScheduledSendNewsletter:     0,
		CreatedAt:                   createdAt,
		UpdatedAt:                   now.Unix(),
	})
	return err
}

// lastURLSegment mimics Ruby's "a/b/".split("/").last (trailing empty
// segments are dropped).
func lastURLSegment(s string) string {
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// safeRemoteURL mirrors ImportRss#safe_remote_url?: only http(s) URLs whose
// host resolves to at least one address and none blocked pass. DNS failures
// are unsafe.
func (r *RSSImporter) safeRemoteURL(ctx context.Context, rawurl string) bool {
	return ssrf.SafeRemoteURL(ctx, rawurl, r.resolveHost)
}

// resolveHost memoizes DNS answers per host (resolved_addresses_for);
// resolution errors are not cached and may be retried on the next call.
func (r *RSSImporter) resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if r.resolved == nil {
		r.resolved = map[string][]netip.Addr{}
	}
	if addrs, ok := r.resolved[host]; ok {
		return addrs, nil
	}
	lookup := r.LookupIP
	if lookup == nil {
		lookup = ssrf.LookupIP
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	r.resolved[host] = addrs
	return addrs, nil
}

// httpClient builds the fetch client: every redirect target must pass the
// SSRF check again, so a public URL cannot bounce the request to a private
// one.
func (r *RSSImporter) httpClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if !r.safeRemoteURL(req.Context(), req.URL.String()) {
				return fmt.Errorf("import rss: redirect to unsafe URL blocked: %s", req.URL)
			}
			return nil
		},
	}
}

// fetch GETs rawurl and returns the body, capped at MaxFeedBodyBytes.
func (r *RSSImporter) fetch(ctx context.Context, rawurl string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, fmt.Errorf("import rss: %w", err)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("import rss: fetch %s: %w", rawurl, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("import rss: fetch %s: status %d", rawurl, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFeedBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("import rss: read %s: %w", rawurl, err)
	}
	if len(body) > MaxFeedBodyBytes {
		return nil, fmt.Errorf("import rss: %s exceeds the 20MB response limit", rawurl)
	}
	return body, nil
}

// rewriteImages mirrors ImportRss#import_images: each <img> with a safe
// remote src is downloaded, stored via the media service, and its src is
// rewritten to the local /files/<key> URL. Unsafe or failing images keep
// their original src (Rails logs and skips them). Repeated srcs download
// once, and distinct downloads are capped at MaxImportImages per importer;
// the same cap bounds distinct srcs reaching the SSRF check, so the DNS
// lookups it performs are capped too. Content over MaxItemContentBytes is
// returned unchanged (no tree walk, no downloads).
func (r *RSSImporter) rewriteImages(ctx context.Context, content, title string) string {
	if len(content) > MaxItemContentBytes {
		return content
	}
	nodes, err := html.ParseFragment(strings.NewReader(content), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
	if err != nil {
		return content
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			r.rewriteImage(ctx, n, title)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	var b strings.Builder
	for _, n := range nodes {
		if err := html.Render(&b, n); err != nil {
			return content
		}
	}
	return b.String()
}

func (r *RSSImporter) rewriteImage(ctx context.Context, img *html.Node, title string) {
	var src string
	for _, attr := range img.Attr {
		if attr.Key == "src" {
			src = attr.Val
			break
		}
	}
	if src == "" {
		return
	}
	// Cap distinct seen srcs before safeRemoteURL: the check costs one DNS
	// query per new host, and the images memo below only bounds downloads,
	// so without this a feed of wildcard-subdomain imgs could force
	// unbounded lookups past MaxImportImages.
	if r.seenSrcs == nil {
		r.seenSrcs = map[string]struct{}{}
	}
	if _, seen := r.seenSrcs[src]; !seen {
		if len(r.seenSrcs) >= MaxImportImages {
			return
		}
		r.seenSrcs[src] = struct{}{}
	}
	if !r.safeRemoteURL(ctx, src) {
		return
	}
	// A repeated src reuses the stored file; failed downloads and srcs past
	// MaxImportImages keep their original src (Rails logs and skips them).
	if r.images == nil {
		r.images = map[string]string{}
	}
	local, seen := r.images[src]
	if !seen {
		if len(r.images) >= MaxImportImages {
			return
		}
		r.images[src] = ""
		body, contentType, err := r.fetchImage(ctx, src)
		if err != nil {
			slog.Default().Warn("import rss: image download failed", "url", src, "error", err)
			return
		}
		// "<title-slug>-<8 hex>.<ext>", ext from the response content type.
		ext := "jpg"
		if ct := strings.SplitN(contentType, ";", 2)[0]; ct != "" {
			if i := strings.LastIndex(ct, "/"); i >= 0 && i+1 < len(ct) {
				ext = ct[i+1:]
			}
		}
		var rnd [4]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return
		}
		filename := fmt.Sprintf("%s-%s.%s", domain.Parameterize(title), hex.EncodeToString(rnd[:]), ext)
		store := r.Media
		if store == nil {
			store = media.New(r.DB, r.DataDir)
		}
		file, err := store.Store(ctx, bytes.NewReader(body), filename, contentType)
		if err != nil {
			slog.Default().Warn("import rss: image store failed", "url", src, "error", err)
			return
		}
		local = "/files/" + file.Key
		r.images[src] = local
	}
	if local == "" {
		return
	}
	for i, attr := range img.Attr {
		if attr.Key == "src" {
			img.Attr[i].Val = local
			break
		}
	}
}

// fetchImage downloads one image with the same SSRF checks and size cap.
func (r *RSSImporter) fetchImage(ctx context.Context, rawurl string) (body []byte, contentType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, MaxFeedBodyBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > MaxFeedBodyBytes {
		return nil, "", errors.New("image exceeds the 20MB response limit")
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// registerImportRSSHandler installs the kind "import_rss" job handler
// (ImportFromRssJob); called from RegisterImportHandlers.
func registerImportRSSHandler(w *jobs.Worker, db *sql.DB, dataDir string) {
	w.Register(jobs.KindImportRSS, func(ctx context.Context, payload json.RawMessage) error {
		var p ImportRSSPayload
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &p); err != nil {
				return fmt.Errorf("import_rss: decode payload: %w", err)
			}
		}
		if p.URL == "" {
			return fmt.Errorf("import_rss: url required")
		}
		activity.Log(ctx, db, "info", "started", "import", fmt.Sprintf("source=\"rss\" url=%s import_images=%t", activity.Quote(p.URL), p.ImportImages))
		result, err := (&RSSImporter{DB: db, DataDir: dataDir}).Import(ctx, p.URL, p.ImportImages)
		if err != nil {
			activity.Log(ctx, db, "error", "failed", "import", fmt.Sprintf("source=\"rss\" url=%s error=%s import_images=%t", activity.Quote(p.URL), activity.Quote(err.Error()), p.ImportImages))
			return nil // mirror ImportFromRssJob: failure is logged, not retried
		}
		activity.Log(ctx, db, "info", "completed", "import", fmt.Sprintf("source=\"rss\" url=%s failed_count=%d import_images=%t imported_count=%d", activity.Quote(p.URL), result.Failed, p.ImportImages, result.Imported))
		return nil
	})
}
