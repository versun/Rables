package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rables/internal/domain"
	"rables/internal/jobs"
)

// stubLookup returns a LookupIP stub: hosts in mapping resolve to the given
// literal addresses, every other host resolves to a public IP.
func stubLookup(mapping map[string][]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		ips, ok := mapping[host]
		if !ok {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		if ips == nil {
			return nil, errors.New("dns: no such host")
		}
		addrs := make([]netip.Addr, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, netip.MustParseAddr(ip))
		}
		return addrs, nil
	}
}

// TestRSSSafeRemoteURL covers the SSRF guard address by address (ImportRss
// BLOCKED_IP_RANGES), plus scheme/host/DNS failure handling.
func TestRSSSafeRemoteURL(t *testing.T) {
	for _, tt := range []struct {
		name string
		url  string
		ips  []string // nil = DNS failure
		want bool
	}{
		{"public http", "http://blog.example/feed", []string{"93.184.216.34"}, true},
		{"public https", "https://blog.example/feed", []string{"93.184.216.34"}, true},
		{"ftp scheme", "ftp://blog.example/feed", []string{"93.184.216.34"}, false},
		{"no scheme", "blog.example/feed", []string{"93.184.216.34"}, false},
		{"private 10.x", "http://blog.example/", []string{"10.0.0.5"}, false},
		{"private 172.16.x", "http://blog.example/", []string{"172.16.3.4"}, false},
		{"private 192.168.x", "http://blog.example/", []string{"192.168.1.1"}, false},
		{"loopback", "http://blog.example/", []string{"127.0.0.1"}, false},
		{"link local", "http://blog.example/", []string{"169.254.1.1"}, false},
		{"cgnat 100.64.x", "http://blog.example/", []string{"100.64.0.1"}, false},
		{"zero net", "http://blog.example/", []string{"0.1.2.3"}, false},
		{"multicast", "http://blog.example/", []string{"224.0.0.1"}, false},
		{"reserved 240.x", "http://blog.example/", []string{"240.0.0.1"}, false},
		{"192.0.0.x", "http://blog.example/", []string{"192.0.0.9"}, false},
		{"benchmark 198.18.x", "http://blog.example/", []string{"198.18.0.1"}, false},
		{"ipv6 unspecified", "http://blog.example/", []string{"::"}, false},
		{"ipv6 loopback", "http://blog.example/", []string{"::1"}, false},
		{"ipv6 ula", "http://blog.example/", []string{"fd00::1"}, false},
		{"ipv6 link local", "http://blog.example/", []string{"fe80::1"}, false},
		{"ipv6 public", "http://blog.example/", []string{"2606:4700::1111"}, true},
		// NAT64 well-known prefix (RFC 6052): the embedded IPv4 is checked
		// against the same blocklist (64:ff9b::a9fe:a9fe = 169.254.169.254).
		{"nat64 embeds link local", "http://blog.example/", []string{"64:ff9b::a9fe:a9fe"}, false},
		{"nat64 embeds loopback", "http://blog.example/", []string{"64:ff9b::7f00:1"}, false},
		{"nat64 embeds public", "http://blog.example/", []string{"64:ff9b::5db8:d822"}, true},
		{"nat64 local-use", "http://blog.example/", []string{"64:ff9b:1::1"}, false},
		{"one bad among many", "http://blog.example/", []string{"93.184.216.34", "192.168.0.1"}, false},
		{"dns failure", "http://gone.example/", nil, false},
		{"empty answer", "http://empty.example/", []string{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			imp := &RSSImporter{LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
				if tt.ips == nil {
					return nil, errors.New("dns: no such host")
				}
				addrs := make([]netip.Addr, 0, len(tt.ips))
				for _, ip := range tt.ips {
					addrs = append(addrs, netip.MustParseAddr(ip))
				}
				return addrs, nil
			}}
			if got := imp.safeRemoteURL(context.Background(), tt.url); got != tt.want {
				t.Errorf("safeRemoteURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

// TestRSSSafeRemoteURLLiterals exercises the default resolver path with IP
// literal hosts (no DNS involved, offline-safe).
func TestRSSSafeRemoteURLLiterals(t *testing.T) {
	imp := &RSSImporter{}
	for _, tt := range []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:8080/feed", false},
		{"http://[::1]/feed", false},
		{"http://[::]/feed", false},
		{"http://[fe80::1]/feed", false},
		{"http://10.1.2.3/feed", false},
		{"http://93.184.216.34/feed", true},
		{"http://[2606:4700::1111]/feed", true},
		// NAT64 literals: the embedded IPv4 decides (link-local blocked,
		// public allowed); the local-use range is blocked outright.
		{"http://[64:ff9b::a9fe:a9fe]/feed", false},
		{"http://[64:ff9b::5db8:d822]/feed", true},
		{"http://[64:ff9b:1::1]/feed", false},
		// Zoned literals: Prefix.Contains never matches them (the zone is
		// not part of the address bits), so they are refused outright.
		{"http://[::1%25lo0]/feed", false},
		{"http://[fe80::1%25eth0]/feed", false},
		{"http://[2606:4700::1111%25eth0]/feed", false},
	} {
		if got := imp.safeRemoteURL(context.Background(), tt.url); got != tt.want {
			t.Errorf("safeRemoteURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

const rssTestFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel>
<title>Example</title>
<item>
  <title>First Post</title>
  <link>https://blog.example/posts/first-post</link>
  <description>summary one</description>
  <content:encoded>&lt;p&gt;hello&lt;/p&gt;</content:encoded>
  <pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate>
</item>
<item>
  <title>No Link Entry</title>
  <content:encoded>&lt;p&gt;skipped&lt;/p&gt;</content:encoded>
</item>
<item>
  <title>Second Post</title>
  <link>https://blog.example/posts/second-post/</link>
  <content:encoded>&lt;p&gt;two&lt;/p&gt;</content:encoded>
</item>
<item>
  <title>Dupe Slug</title>
  <link>https://blog.example/posts/first-post</link>
  <content:encoded>&lt;p&gt;dupe&lt;/p&gt;</content:encoded>
</item>
</channel>
</rss>`

// TestRSSImportEntries covers the article creation semantics: slug from the
// link's last segment, publish status, summary as description, published
// timestamp as created_at; entries without a link are ignored and duplicate
// slugs count as failed without aborting the run.
func TestRSSImportEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, rssTestFeed)
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: stubLookup(nil)}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 2 || result.Failed != 1 {
		t.Errorf("result = %+v, want imported=2 failed=1", result)
	}

	var status, comment int64
	var slug, content, description, contentType string
	var createdAt int64
	err = database.QueryRow(`SELECT slug, status, comment, content_html, description, content_type, created_at FROM articles WHERE slug = 'first-post'`).
		Scan(&slug, &status, &comment, &content, &description, &contentType, &createdAt)
	if err != nil {
		t.Fatalf("query first-post: %v", err)
	}
	if status != 1 || comment != 0 || contentType != "rich_text" {
		t.Errorf("status/comment/content_type = %d/%d/%q, want 1/0/rich_text", status, comment, contentType)
	}
	if content != "<p>hello</p>" {
		t.Errorf("content = %q, want <p>hello</p>", content)
	}
	if description != "summary one" {
		t.Errorf("description = %q, want summary one", description)
	}
	// Mon, 02 Jan 2006 15:04:05 GMT
	if createdAt != 1136214245 {
		t.Errorf("created_at = %d, want 1136214245", createdAt)
	}
	// Trailing slash: the last segment rule drops it (Ruby split semantics).
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM articles WHERE slug = 'second-post'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("second-post count = %d, want 1", count)
	}
}

// TestRSSImportUnsafeFeedURL rejects feeds resolving to private addresses.
func TestRSSImportUnsafeFeedURL(t *testing.T) {
	database, dataDir := newTestDB(t)
	imp := &RSSImporter{
		DB:       database,
		DataDir:  dataDir,
		LookupIP: stubLookup(map[string][]string{"blog.example": {"10.0.0.5"}}),
	}
	_, err := imp.Import(context.Background(), "http://blog.example/feed", false)
	if err == nil || !strings.Contains(err.Error(), "unsafe feed URL") {
		t.Fatalf("error = %v, want unsafe feed URL", err)
	}
	if got := tableCount(t, database, "articles"); got != 0 {
		t.Errorf("articles = %d, want 0", got)
	}
}

// TestRSSImportRedirectToPrivate blocks a public feed URL redirecting to a
// host that resolves to a private address.
func TestRSSImportRedirectToPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://internal.evil/feed", http.StatusFound)
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{
		DB:       database,
		DataDir:  dataDir,
		LookupIP: stubLookup(map[string][]string{"internal.evil": {"169.254.1.1"}}),
	}
	_, err := imp.Import(context.Background(), srv.URL+"/feed", false)
	if err == nil {
		t.Fatal("expected redirect-to-private to fail")
	}
	if got := tableCount(t, database, "articles"); got != 0 {
		t.Errorf("articles = %d, want 0", got)
	}
}

// TestRSSImportSizeCap rejects feeds larger than the 20MB response limit.
func TestRSSImportSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		chunk := make([]byte, 1<<20)
		for i := 0; i < MaxFeedBodyBytes/(1<<20)+1; i++ {
			w.Write(chunk)
		}
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: stubLookup(nil)}
	_, err := imp.Import(context.Background(), srv.URL+"/feed", false)
	if err == nil || !strings.Contains(err.Error(), "20MB") {
		t.Fatalf("error = %v, want the 20MB limit", err)
	}
}

// TestRSSImportImages downloads safe remote images into the media store and
// rewrites their src; unsafe image URLs stay untouched.
func TestRSSImportImages(t *testing.T) {
	const pngHeader = "\x89PNG\r\n\x1a\n"
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>x</title>
<item>
  <title>With Images</title>
  <link>https://blog.example/posts/with-images</link>
  <content:encoded>&lt;p&gt;&lt;img src=%q alt="pic"/&gt;&lt;img src="http://private.internal/x.png"/&gt;&lt;/p&gt;</content:encoded>
</item>
</channel></rss>`, srvURL+"/img/pic.png")
	})
	mux.HandleFunc("/img/pic.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, pngHeader+"fake")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{
		DB:       database,
		DataDir:  dataDir,
		LookupIP: stubLookup(map[string][]string{"private.internal": {"192.168.0.1"}}),
	}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("imported = %d, want 1", result.Imported)
	}

	var content string
	if err := database.QueryRow(`SELECT content_html FROM articles WHERE slug = 'with-images'`).Scan(&content); err != nil {
		t.Fatalf("query article: %v", err)
	}
	if strings.Contains(content, srv.URL) {
		t.Errorf("content still references the remote image: %q", content)
	}
	if !strings.Contains(content, "/files/") {
		t.Errorf("content missing rewritten /files/ URL: %q", content)
	}
	if !strings.Contains(content, "http://private.internal/x.png") {
		t.Errorf("unsafe image src should stay untouched: %q", content)
	}
	if got := tableCount(t, database, "files"); got != 1 {
		t.Fatalf("files = %d, want 1 (one image stored)", got)
	}
	var key, filename string
	if err := database.QueryRow(`SELECT key, filename FROM files`).Scan(&key, &filename); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filename, "with-images-") || !strings.HasSuffix(filename, ".png") {
		t.Errorf("stored filename = %q, want with-images-<hex>.png", filename)
	}
	blob, err := os.ReadFile(filepath.Join(dataDir, "files", key[0:2], key[2:4], key))
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(blob) != pngHeader+"fake" {
		t.Errorf("blob = %q, want the served image bytes", blob)
	}
}

// TestRSSImportSanitizesContent stores feed content through the same
// sanitize/lazy-load write path as admin-saved articles: script markup and
// event-handler attributes must not survive into content_html.
func TestRSSImportSanitizesContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>x</title>
<item>
  <title>Evil</title>
  <link>https://blog.example/posts/evil</link>
  <content:encoded>&lt;p&gt;hi&lt;/p&gt;&lt;script&gt;alert(1)&lt;/script&gt;&lt;img src="https://blog.example/x.png" onerror="alert(2)"/&gt;</content:encoded>
</item>
</channel></rss>`)
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: stubLookup(nil)}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("imported = %d, want 1", result.Imported)
	}
	var content string
	if err := database.QueryRow(`SELECT content_html FROM articles WHERE slug = 'evil'`).Scan(&content); err != nil {
		t.Fatalf("query article: %v", err)
	}
	for _, banned := range []string{"<script", "onerror"} {
		if strings.Contains(content, banned) {
			t.Errorf("content_html contains %q: %q", banned, content)
		}
	}
	if !strings.Contains(content, "<p>hi</p>") {
		t.Errorf("content_html lost the legit paragraph: %q", content)
	}
	if !strings.Contains(content, `loading="lazy"`) {
		t.Errorf("content_html missing lazy loading on the image: %q", content)
	}
}

// TestRSSImportJobEndToEnd drives an RSS import through job_runs and checks
// the activity rows mirror ImportFromRssJob.
func TestRSSImportJobEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, rssTestFeed)
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	worker := jobs.NewWorker(database)
	RegisterImportHandlers(worker, database, dataDir, nil)

	// The worker-side handler uses the real resolver; enqueue a payload the
	// importer stub cannot see, so drive the importer through the handler
	// with a stubbed LookupIP by registering a custom handler instead.
	// Here we only verify the payload decode + activity flow with an URL
	// that fails the SSRF check (loopback host): the job completes and logs
	// the failure instead of retrying.
	enqueuer := jobs.NewEnqueuer(database)
	if _, err := enqueuer.Enqueue(context.Background(), jobs.KindImportRSS, ImportRSSPayload{URL: srv.URL + "/feed", ImportImages: true}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if !claimed {
		t.Fatal("no job claimed")
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM job_runs`).Scan(&status); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if status != "done" {
		t.Errorf("job status = %q, want done (failure logged, not retried)", status)
	}
	for _, action := range []string{"started", "failed"} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM activity_logs WHERE target = 'import' AND action = ?`, action).Scan(&count); err != nil {
			t.Fatalf("query activity: %v", err)
		}
		if count != 1 {
			t.Errorf("activity %q rows = %d, want 1", action, count)
		}
	}
	var description string
	if err := database.QueryRow(`SELECT description FROM activity_logs WHERE target = 'import' AND action = 'failed'`).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(description, `source="rss"`) || !strings.Contains(description, "unsafe feed URL") {
		t.Errorf("failed description = %q, want source=url and the SSRF error", description)
	}
}

// TestRSSImportDotOnlySlug rejects entries whose link's last segment is only
// dots ("." , "..", "..."): GenerateSlug strips the dots to a blank slug,
// which would otherwise insert an article no URL can reach.
func TestRSSImportDotOnlySlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>x</title>
<item>
  <title>Dot</title>
  <link>https://blog.example/posts/.</link>
  <content:encoded>&lt;p&gt;dot&lt;/p&gt;</content:encoded>
</item>
<item>
  <title>Dot Dot</title>
  <link>https://blog.example/posts/..</link>
  <content:encoded>&lt;p&gt;dotdot&lt;/p&gt;</content:encoded>
</item>
<item>
  <title>Triple Dot</title>
  <link>https://blog.example/posts/...</link>
  <content:encoded>&lt;p&gt;triple&lt;/p&gt;</content:encoded>
</item>
<item>
  <title>Fine</title>
  <link>https://blog.example/posts/fine</link>
  <content:encoded>&lt;p&gt;fine&lt;/p&gt;</content:encoded>
</item>
</channel></rss>`)
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: stubLookup(nil)}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 1 || result.Failed != 3 {
		t.Errorf("result = %+v, want imported=1 failed=3", result)
	}
	var blank int
	if err := database.QueryRow(`SELECT COUNT(*) FROM articles WHERE slug IS NULL OR TRIM(slug) = ''`).Scan(&blank); err != nil {
		t.Fatal(err)
	}
	if blank != 0 {
		t.Errorf("articles with blank slug = %d, want 0", blank)
	}
}

// TestRSSImportBatchSlug refuses entries whose link ends in an admin batch
// action: POST /admin/posts/batch_publish etc. are static chi routes, so the
// update of an article imported under such a slug would be swallowed by the
// batch handler (same reservation articles.Save enforces).
func TestRSSImportBatchSlug(t *testing.T) {
	var items strings.Builder
	for _, slug := range domain.AdminArticleBatchSlugs {
		fmt.Fprintf(&items, `
<item>
  <title>%s</title>
  <link>https://blog.example/posts/%s</link>
  <content:encoded>&lt;p&gt;body&lt;/p&gt;</content:encoded>
</item>`, slug, slug)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>x</title>%s
</channel></rss>`, items.String())
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: stubLookup(nil)}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 0 || result.Failed != len(domain.AdminArticleBatchSlugs) {
		t.Errorf("result = %+v, want imported=0 failed=%d", result, len(domain.AdminArticleBatchSlugs))
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("articles = %d, want 0", n)
	}
}

// TestRSSImportImagesDedup downloads a repeated image URL only once, even
// across entries: every img tag is rewritten to the same local file.
func TestRSSImportImagesDedup(t *testing.T) {
	const pngHeader = "\x89PNG\r\n\x1a\n"
	var srvURL string
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>x</title>
<item>
  <title>One</title>
  <link>https://blog.example/posts/one</link>
  <content:encoded>&lt;p&gt;&lt;img src=%q/&gt;&lt;img src=%[1]q/&gt;&lt;/p&gt;</content:encoded>
</item>
<item>
  <title>Two</title>
  <link>https://blog.example/posts/two</link>
  <content:encoded>&lt;p&gt;&lt;img src=%[1]q/&gt;&lt;/p&gt;</content:encoded>
</item>
</channel></rss>`, srvURL+"/img/pic.png")
	})
	mux.HandleFunc("/img/pic.png", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, pngHeader+"fake")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: stubLookup(nil)}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 2 {
		t.Fatalf("imported = %d, want 2", result.Imported)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("image downloads = %d, want 1", got)
	}
	if got := tableCount(t, database, "files"); got != 1 {
		t.Fatalf("files = %d, want 1 (one image stored)", got)
	}
	var key string
	if err := database.QueryRow(`SELECT key FROM files`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	local := "/files/" + key
	for slug, want := range map[string]int{"one": 2, "two": 1} {
		var content string
		if err := database.QueryRow(`SELECT content_html FROM articles WHERE slug = ?`, slug).Scan(&content); err != nil {
			t.Fatalf("query %s: %v", slug, err)
		}
		if got := strings.Count(content, local); got != want {
			t.Errorf("%s: %q count = %d, want %d: %q", slug, local, got, want, content)
		}
	}
}

// TestRSSImportImagesCap downloads at most MaxImportImages distinct images
// per import; img tags past the cap keep their original src.
func TestRSSImportImagesCap(t *testing.T) {
	var srvURL string
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		var imgs strings.Builder
		for i := 0; i < MaxImportImages+5; i++ {
			fmt.Fprintf(&imgs, "&lt;img src=%q/&gt;", fmt.Sprintf("%s/img/%d.png", srvURL, i))
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>x</title>
<item>
  <title>Many Images</title>
  <link>https://blog.example/posts/many-images</link>
  <content:encoded>&lt;p&gt;%s&lt;/p&gt;</content:encoded>
</item>
</channel></rss>`, imgs.String())
	})
	mux.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "\x89PNG\r\n\x1a\nfake")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: stubLookup(nil)}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("imported = %d, want 1", result.Imported)
	}
	if got := atomic.LoadInt32(&hits); got != MaxImportImages {
		t.Errorf("image downloads = %d, want %d", got, MaxImportImages)
	}
	if got := tableCount(t, database, "files"); got != MaxImportImages {
		t.Errorf("files = %d, want %d", got, MaxImportImages)
	}
	var content string
	if err := database.QueryRow(`SELECT content_html FROM articles WHERE slug = 'many-images'`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(content, "/files/"); got != MaxImportImages {
		t.Errorf("rewritten srcs = %d, want %d", got, MaxImportImages)
	}
	overflow := fmt.Sprintf("%s/img/%d.png", srvURL, MaxImportImages)
	if !strings.Contains(content, overflow) {
		t.Errorf("img past the cap should keep its original src %q", overflow)
	}
}

// TestRSSImportImagesDNSCap puts MaxImportImages+5 imgs on distinct
// wildcard-style hosts; the seen-src cap must bound the SSRF check's DNS
// lookups to MaxImportImages, and imgs past the cap keep their original
// src. The img hosts resolve to a private address so no download runs.
func TestRSSImportImagesDNSCap(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		var imgs strings.Builder
		for i := 0; i < MaxImportImages+5; i++ {
			fmt.Fprintf(&imgs, "&lt;img src=%q/&gt;", fmt.Sprintf("http://img-%d.evil.example/pic.png", i))
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>x</title>
<item>
  <title>DNS Amplification</title>
  <link>https://blog.example/posts/dns-amplification</link>
  <content:encoded>&lt;p&gt;%s&lt;/p&gt;</content:encoded>
</item>
</channel></rss>`, imgs.String())
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var lookups int32
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if strings.HasSuffix(host, ".evil.example") {
			atomic.AddInt32(&lookups, 1)
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: lookup}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("imported = %d, want 1", result.Imported)
	}
	if got := atomic.LoadInt32(&lookups); got != MaxImportImages {
		t.Errorf("img host DNS lookups = %d, want %d", got, MaxImportImages)
	}
	var content string
	if err := database.QueryRow(`SELECT content_html FROM articles WHERE slug = 'dns-amplification'`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	overflow := fmt.Sprintf("http://img-%d.evil.example/pic.png", MaxImportImages)
	if !strings.Contains(content, overflow) {
		t.Errorf("img past the cap should keep its original src %q", overflow)
	}
}

// TestRSSImportOversizedItemContent: an entry whose content exceeds
// MaxItemContentBytes skips the image-rewriting tree walk entirely — the img
// keeps its original src, its host is never resolved, nothing is downloaded —
// while the content is still sanitized on the way in.
func TestRSSImportOversizedItemContent(t *testing.T) {
	var cdnLookups int32
	lookup := func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "cdn.example" {
			atomic.AddInt32(&cdnLookups, 1)
		}
		return stubLookup(nil)(ctx, host)
	}
	// content:encoded is XML-escaped, so the feed body stays well under the
	// 20MB response limit while the decoded content clears the per-item cap.
	padding := strings.Repeat("&lt;p&gt;x&lt;/p&gt;", MaxItemContentBytes/8+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>x</title>
<item>
  <title>Big</title>
  <link>https://blog.example/posts/big</link>
  <content:encoded>&lt;p&gt;&lt;img src="http://cdn.example/pic.png"/&gt;&lt;script&gt;alert(1)&lt;/script&gt;%s&lt;/p&gt;</content:encoded>
</item>
</channel></rss>`, padding)
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: lookup}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("imported = %d, want 1", result.Imported)
	}
	var content string
	if err := database.QueryRow(`SELECT content_html FROM articles WHERE slug = 'big'`).Scan(&content); err != nil {
		t.Fatalf("query article: %v", err)
	}
	if !strings.Contains(content, `src="http://cdn.example/pic.png"`) {
		t.Errorf("oversized content should keep the original img src")
	}
	if strings.Contains(content, "<script") {
		t.Errorf("oversized content must still be sanitized")
	}
	if strings.Contains(content, `loading="lazy"`) {
		t.Errorf("oversized content should skip the lazy-loading pass")
	}
	if got := atomic.LoadInt32(&cdnLookups); got != 0 {
		t.Errorf("cdn.example DNS lookups = %d, want 0 (tree walk skipped)", got)
	}
	if got := tableCount(t, database, "files"); got != 0 {
		t.Errorf("files = %d, want 0 (no image downloaded)", got)
	}
}

// TestRSSImportItemsCap imports at most MaxImportItems entries per run;
// entries past the cap count as failed and never reach the articles table.
func TestRSSImportItemsCap(t *testing.T) {
	var items strings.Builder
	for i := 0; i < MaxImportItems+5; i++ {
		fmt.Fprintf(&items, `
<item>
  <title>Post %d</title>
  <link>https://blog.example/posts/post-%d</link>
  <content:encoded>&lt;p&gt;body&lt;/p&gt;</content:encoded>
</item>`, i, i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>x</title>%s
</channel></rss>`, items.String())
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	imp := &RSSImporter{DB: database, DataDir: dataDir, LookupIP: stubLookup(nil)}
	result, err := imp.Import(context.Background(), srv.URL+"/feed", false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != MaxImportItems || result.Failed != 5 {
		t.Errorf("result = %+v, want imported=%d failed=5", result, MaxImportItems)
	}
	if got := tableCount(t, database, "articles"); got != MaxImportItems {
		t.Errorf("articles = %d, want %d", got, MaxImportItems)
	}
	// The first entries win; one past the cap must not exist.
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM articles WHERE slug = ?`, fmt.Sprintf("post-%d", MaxImportItems)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("entry past the cap was imported (slug post-%d)", MaxImportItems)
	}
}
