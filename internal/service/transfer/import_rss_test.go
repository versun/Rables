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
	"testing"
	"time"

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
		{"ipv6 loopback", "http://blog.example/", []string{"::1"}, false},
		{"ipv6 ula", "http://blog.example/", []string{"fd00::1"}, false},
		{"ipv6 link local", "http://blog.example/", []string{"fe80::1"}, false},
		{"ipv6 public", "http://blog.example/", []string{"2606:4700::1111"}, true},
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
		{"http://[fe80::1]/feed", false},
		{"http://10.1.2.3/feed", false},
		{"http://93.184.216.34/feed", true},
		{"http://[2606:4700::1111]/feed", true},
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

// TestRSSImportJobEndToEnd drives an RSS import through job_runs and checks
// the activity rows mirror ImportFromRssJob.
func TestRSSImportJobEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, rssTestFeed)
	}))
	defer srv.Close()

	database, dataDir := newTestDB(t)
	worker := jobs.NewWorker(database)
	RegisterImportHandlers(worker, database, dataDir)

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
