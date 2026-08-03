package httpd

import (
	"bytes"
	"database/sql"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/service/comments"
)

// rssItem is one <item> of an RSS 2.0 feed (articles/index.rss.builder and
// tags/show.rss.builder share the shape).
type rssItem struct {
	Title       string
	Description string
	ContentHTML string // rendered inside <content:encoded> CDATA
	Link        string
	PubDateUnix int64
	Author      string
}

// publicFeed serves GET /feed.xml, mirroring ArticlesController#index (RSS
// format): the 50 newest published articles.
func (s *Server) publicFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	articles, err := s.Q.ListPublishedArticles(ctx, query.ListPublishedArticlesParams{Limit: publicFeedLimit, Offset: 0})
	if err != nil {
		s.listError(w, "list feed articles", err)
		return
	}
	st, err := s.Settings().Get(ctx)
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	siteURL := normalizeSiteURL(st.Url.String)
	channelLink := siteURL
	if channelLink == "" {
		channelLink = strings.TrimSpace(st.Url.String) // builder: site_url.presence || site_settings[:url]
	}

	items := make([]rssItem, 0, len(articles))
	for _, a := range articles {
		link := comments.ArticlePath(s.Cfg.ArticleRoutePrefix, a.Slug.String)
		if siteURL != "" {
			link = siteURL + link
		}
		// content_html is sanitized and lazy-loaded at write time; the source
		// reference is prepended like the RSS builder does.
		content := string(buildSourceReference(a.SourceAuthor.String, a.SourceContent.String, a.SourceUrl.String)) + a.ContentHtml.String
		items = append(items, rssItem{
			Title:       rssItemTitle(a, tzLocation(st.TimeZone)),
			Description: firstPresent(a.Description.String, a.Excerpt.String),
			ContentHTML: content,
			Link:        link,
			PubDateUnix: a.CreatedAt,
			Author:      st.Author.String,
		})
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=600, s-maxage=1800")
	_, _ = w.Write(rssDocument(st.Title.String, st.Description.String, channelLink, st.Author.String, items, tzLocation(st.TimeZone)))
}

// publicTagRSS serves GET /tags/{slug}.rss, mirroring TagsController#show
// (RSS format): the 50 newest published articles of the tag.
func (s *Server) publicTagRSS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tag, err := s.Q.GetPublicTagBySlug(ctx, slugParam(r))
	if errors.Is(err, sql.ErrNoRows) {
		s.publicNotFound(w)
		return
	}
	if err != nil {
		s.listError(w, "get tag by slug", err)
		return
	}
	articles, err := s.Q.ListPublishedArticlesByTag(ctx, query.ListPublishedArticlesByTagParams{TagID: tag.ID, Limit: publicFeedLimit, Offset: 0})
	if err != nil {
		s.listError(w, "list tag feed articles", err)
		return
	}
	st, err := s.Settings().Get(ctx)
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	siteURL := normalizeSiteURL(st.Url.String)
	channelLink := siteURL + "/tags/" + tag.Slug
	if siteURL == "" {
		channelLink = requestBaseURL(r) + "/tags/" + tag.Slug // builder: tag_url(@tag.slug)
	}

	items := make([]rssItem, 0, len(articles))
	for _, a := range articles {
		link := comments.ArticlePath(s.Cfg.ArticleRoutePrefix, a.Slug.String)
		if siteURL != "" {
			link = siteURL + link
		}
		items = append(items, rssItem{
			Title:       rssItemTitle(a, tzLocation(st.TimeZone)),
			Description: a.Description.String, // tag RSS has no excerpt fallback
			ContentHTML: a.ContentHtml.String,
			Link:        link,
			PubDateUnix: a.CreatedAt,
			Author:      st.Author.String,
		})
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write(rssDocument(
		"Articles tagged with "+tag.Name+" | "+st.Title.String,
		"Latest articles tagged with "+tag.Name+" from "+st.Title.String,
		channelLink, st.Author.String, items, tzLocation(st.TimeZone)))
}

// rssDocument renders the RSS 2.0 document shared by both builders.
func rssDocument(title, description, link, author string, items []rssItem, loc *time.Location) []byte {
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\"?>\n")
	b.WriteString("<rss version=\"2.0\" xmlns:content=\"http://purl.org/rss/1.0/modules/content/\">\n<channel>\n")
	writeXMLElement(&b, "title", title)
	writeXMLElement(&b, "description", description)
	writeXMLElement(&b, "link", link)
	writeXMLElement(&b, "author", author)
	for _, item := range items {
		b.WriteString("<item>\n")
		writeXMLElement(&b, "title", item.Title)
		writeXMLElement(&b, "description", item.Description)
		writeXMLCDATA(&b, "content:encoded", item.ContentHTML)
		writeXMLElement(&b, "pubDate", time.Unix(item.PubDateUnix, 0).In(loc).Format("Mon, 02 Jan 2006 15:04:05 -0700")) // rfc822
		writeXMLElement(&b, "link", item.Link)
		writeXMLElement(&b, "guid", item.Link)
		writeXMLElement(&b, "author", item.Author)
		b.WriteString("</item>\n")
	}
	b.WriteString("</channel>\n</rss>\n")
	return b.Bytes()
}

func writeXMLElement(b *bytes.Buffer, name, value string) {
	b.WriteByte('<')
	b.WriteString(name)
	b.WriteByte('>')
	_ = xml.EscapeText(b, []byte(value))
	b.WriteString("</")
	b.WriteString(name)
	b.WriteString(">\n")
}

// writeXMLCDATA wraps value in a CDATA section, splitting the forbidden "]]>"
// sequence like any CDATA serializer must.
func writeXMLCDATA(b *bytes.Buffer, name, value string) {
	value = strings.ReplaceAll(value, "]]>", "]]]]><![CDATA[>")
	b.WriteByte('<')
	b.WriteString(name)
	b.WriteString("><![CDATA[")
	b.WriteString(value)
	b.WriteString("]]></")
	b.WriteString(name)
	b.WriteString(">\n")
}

// rssItemTitle mirrors the builders' title fallback chain:
// title || plain_text_content.squish[0, 20] || created_at.strftime("%Y-%m-%d").
func rssItemTitle(a query.Article, loc *time.Location) string {
	if !domain.IsBlank(a.Title.String) {
		return a.Title.String
	}
	text := domain.Squish(domain.PlainText(a.ContentHtml.String))
	if runes := []rune(text); len(runes) > 20 {
		text = string(runes[:20])
	}
	if text != "" {
		return text
	}
	return time.Unix(a.CreatedAt, 0).In(loc).Format("2006-01-02")
}

// firstPresent returns the first non-blank string (Rails a.presence || b).
func firstPresent(values ...string) string {
	for _, v := range values {
		if !domain.IsBlank(v) {
			return v
		}
	}
	return ""
}

// requestBaseURL mirrors request.base_url (scheme + host) for absolute URL
// fallbacks when no site URL is configured.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
