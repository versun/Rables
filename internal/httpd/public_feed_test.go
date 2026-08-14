package httpd

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
)

// rssProbe decodes just enough of an RSS document to assert structure and
// item counts; xml.Unmarshal also validates well-formedness.
type rssProbe struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Title string `xml:"title"`
		Link  string `xml:"link"`
		Items []struct {
			Title   string `xml:"title"`
			Link    string `xml:"link"`
			PubDate string `xml:"pubDate"`
		} `xml:"item"`
	} `xml:"channel"`
}

// sitemapProbe decodes the sitemap urlset.
type sitemapProbe struct {
	XMLName xml.Name `xml:"urlset"`
	URLs    []struct {
		Loc string `xml:"loc"`
	} `xml:"url"`
}

// TestPublicFeed covers the RSS feed: 10-item default, all published articles
// with feed_all_articles, valid XML, publish-only.
func TestPublicFeed(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	for i := int64(1); i <= 55; i++ {
		seedArticle(t, s, seedArticleOpts{slug: "feed-" + pad2(i), title: "Feed " + pad2(i), status: 1, createdAt: i})
	}
	seedArticle(t, s, seedArticleOpts{slug: "feed-draft", title: "Feed Draft", status: 0, createdAt: 100})
	setSiteURL(t, s, "https://blog.example.com")

	rec := get(t, h, "/feed.xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc rssProbe
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("feed is not valid XML: %v", err)
	}
	if n := len(doc.Channel.Items); n != publicFeedRecentLimit {
		t.Errorf("items = %d, want %d (default feed limit)", n, publicFeedRecentLimit)
	}
	// Newest first: the first item is the highest created_at.
	if len(doc.Channel.Items) > 0 && !strings.HasSuffix(doc.Channel.Items[0].Link, "/feed-55") {
		t.Errorf("first item link = %q, want newest /feed-55", doc.Channel.Items[0].Link)
	}
	if strings.Contains(rec.Body.String(), "feed-draft") {
		t.Error("draft article leaked into the feed")
	}
	if len(doc.Channel.Items) > 0 && !strings.HasPrefix(doc.Channel.Items[0].Link, "https://blog.example.com/") {
		t.Errorf("item link not absolute: %q", doc.Channel.Items[0].Link)
	}

	t.Run("all articles with feed_all_articles enabled", func(t *testing.T) {
		setFeedAllArticles(t, s, true)
		defer setFeedAllArticles(t, s, false)
		rec := get(t, h, "/feed.xml")
		var doc rssProbe
		if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("feed is not valid XML: %v", err)
		}
		if n := len(doc.Channel.Items); n != 55 {
			t.Errorf("items = %d, want 55 (all published)", n)
		}
		if len(doc.Channel.Items) > 0 && !strings.HasSuffix(doc.Channel.Items[0].Link, "/feed-55") {
			t.Errorf("first item link = %q, want newest /feed-55", doc.Channel.Items[0].Link)
		}
	})

	t.Run("CDATA with forbidden sequence stays valid", func(t *testing.T) {
		seedArticle(t, s, seedArticleOpts{slug: "cdata-trap", title: "Trap", content: "<p>a]]>b</p>", status: 1, createdAt: 200})
		rec := get(t, h, "/feed.xml")
		var doc rssProbe
		if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("feed with ]]> in content is invalid XML: %v", err)
		}
	})

	t.Run("title fallback chain", func(t *testing.T) {
		// No title: falls back to the first 20 squished plain-text runes.
		seedArticle(t, s, seedArticleOpts{slug: "untitled", content: "<p>hello   world this is a long body</p>", status: 1, createdAt: 300})
		rec := get(t, h, "/feed.xml")
		var doc rssProbe
		if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		var found string
		for _, item := range doc.Channel.Items {
			if strings.HasSuffix(item.Link, "/untitled") {
				found = item.Title
			}
		}
		if found != "hello world this is " {
			t.Errorf("fallback title = %q, want first 20 runes of squished text", found)
		}
	})
}

// TestPublicTagFeed covers /tags/{slug}.rss.
func TestPublicTagFeed(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	goID := seedTag(t, s, "Go", "go")
	rustID := seedTag(t, s, "Rust", "rust")
	a1 := seedArticle(t, s, seedArticleOpts{slug: "go-post", title: "Go Post", status: 1})
	a2 := seedArticle(t, s, seedArticleOpts{slug: "go-draft", title: "Go Draft", status: 0})
	a3 := seedArticle(t, s, seedArticleOpts{slug: "rust-post", title: "Rust Post", status: 1})
	tagArticle(t, s, a1, goID)
	tagArticle(t, s, a2, goID)
	tagArticle(t, s, a3, rustID)
	setSiteURL(t, s, "https://blog.example.com")

	rec := get(t, h, "/tags/go.rss")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc rssProbe
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("tag feed invalid XML: %v", err)
	}
	if n := len(doc.Channel.Items); n != 1 {
		t.Errorf("items = %d, want 1 (publish only, this tag only)", n)
	}
	if want := "Articles tagged with Go | "; !strings.HasPrefix(doc.Channel.Title, want) {
		t.Errorf("channel title = %q, want prefix %q", doc.Channel.Title, want)
	}
	if doc.Channel.Link != "https://blog.example.com/tags/go" {
		t.Errorf("channel link = %q", doc.Channel.Link)
	}

	t.Run("unknown tag feed 404", func(t *testing.T) {
		if rec := get(t, h, "/tags/nope.rss"); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("tag html page still html", func(t *testing.T) {
		rec := get(t, h, "/tags/go")
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("/tags/go Content-Type = %q, want html (param edge order)", ct)
		}
	})

	t.Run("tag feed honors feed_all_articles", func(t *testing.T) {
		manyID := seedTag(t, s, "Many", "many")
		for i := int64(1); i <= 15; i++ {
			a := seedArticle(t, s, seedArticleOpts{slug: "many-" + pad2(i), title: "Many " + pad2(i), status: 1, createdAt: i})
			tagArticle(t, s, a, manyID)
		}

		rec := get(t, h, "/tags/many.rss")
		var doc rssProbe
		if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("tag feed invalid XML: %v", err)
		}
		if n := len(doc.Channel.Items); n != publicFeedRecentLimit {
			t.Errorf("items = %d, want %d (default feed limit)", n, publicFeedRecentLimit)
		}
		if len(doc.Channel.Items) > 0 && !strings.HasSuffix(doc.Channel.Items[0].Link, "/many-15") {
			t.Errorf("first item link = %q, want newest /many-15", doc.Channel.Items[0].Link)
		}

		setFeedAllArticles(t, s, true)
		defer setFeedAllArticles(t, s, false)
		rec = get(t, h, "/tags/many.rss")
		doc = rssProbe{}
		if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("tag feed invalid XML: %v", err)
		}
		if n := len(doc.Channel.Items); n != 15 {
			t.Errorf("items = %d, want 15 (all published)", n)
		}
	})
}

// TestPublicSitemap covers sitemap.xml: empty without a site URL, absolute
// locs with one, publish-only entries.
func TestPublicSitemap(t *testing.T) {
	s, h := newPublicTestServer(t, "")
	seedArticle(t, s, seedArticleOpts{slug: "sm-post", title: "SM", status: 1, updatedAt: 1700000000})
	seedArticle(t, s, seedArticleOpts{slug: "sm-draft", title: "Draft", status: 0, updatedAt: 1700000001})
	seedPage(t, s, seedPageOpts{slug: "sm-page", title: "Page", status: 1})

	t.Run("empty urlset without site url", func(t *testing.T) {
		rec := get(t, h, "/sitemap.xml")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var doc sitemapProbe
		if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("sitemap invalid XML: %v", err)
		}
		if len(doc.URLs) != 0 {
			t.Errorf("urls = %d, want 0 without a configured site URL", len(doc.URLs))
		}
	})

	t.Run("entries with site url", func(t *testing.T) {
		setSiteURL(t, s, "blog.example.com") // scheme is added like normalized_site_url
		rec := get(t, h, "/sitemap.xml")
		var doc sitemapProbe
		if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("sitemap invalid XML: %v", err)
		}
		locs := make([]string, 0, len(doc.URLs))
		for _, u := range doc.URLs {
			locs = append(locs, u.Loc)
		}
		joined := strings.Join(locs, "\n")
		for _, want := range []string{
			"https://blog.example.com",               // root entry
			"https://blog.example.com/sm-post",       // published article
			"https://blog.example.com/pages/sm-page", // published page
		} {
			found := false
			for _, loc := range locs {
				if loc == want {
					found = true
				}
			}
			if !found {
				t.Errorf("sitemap missing loc %q\ngot:\n%s", want, joined)
			}
		}
		if strings.Contains(joined, "sm-draft") {
			t.Error("draft article leaked into the sitemap")
		}
	})
}

// TestArticleRoutePrefix covers ARTICLE_ROUTE_PREFIX (plan section 4.12):
// routes, RSS links and sitemap locs all honor the prefix.
func TestArticleRoutePrefix(t *testing.T) {
	s, h := newPublicTestServer(t, "blog")
	seedArticle(t, s, seedArticleOpts{slug: "prefixed", title: "Prefixed", status: 1})
	setSiteURL(t, s, "https://blog.example.com")

	t.Run("index under prefix and at root", func(t *testing.T) {
		for _, target := range []string{"/blog/", "/"} {
			rec := get(t, h, target)
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s: status = %d, want 200", target, rec.Code)
			}
		}
	})

	t.Run("show under prefix", func(t *testing.T) {
		if rec := get(t, h, "/blog/prefixed"); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("no root catch-all with prefix", func(t *testing.T) {
		if rec := get(t, h, "/prefixed"); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("feed item links carry the prefix", func(t *testing.T) {
		body := get(t, h, "/feed.xml").Body.String()
		if !strings.Contains(body, "https://blog.example.com/blog/prefixed") {
			t.Error("feed item link missing route prefix")
		}
	})

	t.Run("sitemap locs carry the prefix", func(t *testing.T) {
		body := get(t, h, "/sitemap.xml").Body.String()
		if !strings.Contains(body, "<loc>https://blog.example.com/blog/prefixed</loc>") {
			t.Error("sitemap loc missing route prefix")
		}
	})

	t.Run("index list links carry the prefix", func(t *testing.T) {
		body := get(t, h, "/").Body.String()
		if !strings.Contains(body, `href="/blog/prefixed"`) {
			t.Error("index list link missing route prefix")
		}
	})
}

// setRoutePrefix writes settings.article_route_prefix directly, like the
// admin settings form would, and drops the cached row.
func setRoutePrefix(t *testing.T, s *Server, prefix string) {
	t.Helper()
	if _, err := s.Settings().Get(t.Context()); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	if _, err := s.DB.Exec(`UPDATE settings SET article_route_prefix = ? WHERE id = 1`, prefix); err != nil {
		t.Fatalf("set route prefix: %v", err)
	}
	s.Settings().Invalidate()
}

// setFeedAllArticles flips settings.feed_all_articles directly, like the admin
// settings form would, and drops the cached row.
func setFeedAllArticles(t *testing.T, s *Server, all bool) {
	t.Helper()
	if _, err := s.Settings().Get(t.Context()); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	var v int
	if all {
		v = 1
	}
	if _, err := s.DB.Exec(`UPDATE settings SET feed_all_articles = ? WHERE id = 1`, v); err != nil {
		t.Fatalf("set feed_all_articles: %v", err)
	}
	s.Settings().Invalidate()
}

// TestArticleRoutePrefixFromSettings covers the admin-configured prefix: it
// applies on the same Server instance (no restart) and overrides the
// environment fallback.
func TestArticleRoutePrefixFromSettings(t *testing.T) {
	s, h := newPublicTestServer(t, "envprefix")
	seedArticle(t, s, seedArticleOpts{slug: "prefixed", title: "Prefixed", status: 1})
	setSiteURL(t, s, "https://blog.example.com")

	// Environment fallback applies while the setting is empty.
	if rec := get(t, h, "/envprefix/prefixed"); rec.Code != http.StatusOK {
		t.Fatalf("env prefix show: status = %d, want 200", rec.Code)
	}

	// Saving the setting re-routes without a restart or re-registration.
	setRoutePrefix(t, s, "blog")

	t.Run("show under the configured prefix", func(t *testing.T) {
		if rec := get(t, h, "/blog/prefixed"); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("setting overrides the environment value", func(t *testing.T) {
		if rec := get(t, h, "/envprefix/prefixed"); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("no root catch-all with prefix", func(t *testing.T) {
		if rec := get(t, h, "/prefixed"); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("unknown first segment is 404", func(t *testing.T) {
		if rec := get(t, h, "/other/prefixed"); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("index under prefix and at root", func(t *testing.T) {
		for _, target := range []string{"/blog/", "/"} {
			if rec := get(t, h, target); rec.Code != http.StatusOK {
				t.Errorf("GET %s: status = %d, want 200", target, rec.Code)
			}
		}
	})

	t.Run("feed item links carry the prefix", func(t *testing.T) {
		body := get(t, h, "/feed.xml").Body.String()
		if !strings.Contains(body, "https://blog.example.com/blog/prefixed") {
			t.Error("feed item link missing route prefix")
		}
	})

	// Clearing the setting falls back to the environment value again.
	setRoutePrefix(t, s, "")
	if rec := get(t, h, "/envprefix/prefixed"); rec.Code != http.StatusOK {
		t.Errorf("env fallback after clearing: status = %d, want 200", rec.Code)
	}
}
