package httpd

import (
	"bytes"
	"net/http"
	"time"

	"rables/internal/service/comments"
)

// publicSitemap serves GET /sitemap.xml, mirroring SitemapController#index +
// sitemap/index.xml.builder: without a configured site URL the urlset stays
// empty (relative locs are invalid); otherwise the root URL comes first
// (lastmod = newest article update), then published pages and articles.
func (s *Server) publicSitemap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	articles, err := s.Q.ListPublishedArticleSitemap(ctx)
	if err != nil {
		s.listError(w, "list sitemap articles", err)
		return
	}
	pages, err := s.Q.ListPublishedPageSitemap(ctx)
	if err != nil {
		s.listError(w, "list sitemap pages", err)
		return
	}
	latest, err := s.Q.LatestPublishedArticleUpdate(ctx)
	if err != nil {
		s.listError(w, "latest article update", err)
		return
	}
	st, err := s.Settings().Get(ctx)
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}

	loc := tzLocation(st.TimeZone)
	siteURL := normalizeSiteURL(st.Url.String)

	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<urlset xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\">\n")
	if siteURL != "" {
		latestUnix, _ := latest.(int64) // MAX(updated_at); nil when no articles
		lastmod := time.Now().In(loc)
		if latestUnix > 0 {
			lastmod = time.Unix(latestUnix, 0).In(loc)
		}
		writeSitemapURL(&b, siteURL, lastmod, "daily", "1.0")
		for _, p := range pages {
			writeSitemapURL(&b, siteURL+comments.PagePath(p.Slug.String), time.Unix(p.UpdatedAt, 0).In(loc), "weekly", "0.8")
		}
		for _, a := range articles {
			writeSitemapURL(&b, siteURL+comments.ArticlePath(s.Cfg.ArticleRoutePrefix, a.Slug.String), time.Unix(a.UpdatedAt, 0).In(loc), "weekly", "0.8")
		}
	}
	b.WriteString("</urlset>\n")

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}

func writeSitemapURL(b *bytes.Buffer, loc string, lastmod time.Time, changefreq, priority string) {
	b.WriteString("<url>\n")
	writeXMLElement(b, "loc", loc)
	writeXMLElement(b, "lastmod", lastmod.Format("2006-01-02"))
	writeXMLElement(b, "changefreq", changefreq)
	writeXMLElement(b, "priority", priority)
	b.WriteString("</url>\n")
}
