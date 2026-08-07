package httpd

import (
	"context"
	"database/sql"
	"html"
	"html/template"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	xhtml "golang.org/x/net/html"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/service/captcha"
	"rables/internal/service/comments"
	"rables/internal/settings"
	"rables/internal/templates"
)

// Public list sizes (plan section 4.12).
const (
	publicArticlesPerPage = 10
	publicTagPerPage      = 20
	publicFeedLimit       = 50
)

// renderCacheTTL mirrors the 7-day Rails.cache expiry of rendered_content
// (Article#rendered_content / Page#rendered_content, plan section 4.4).
const renderCacheTTL = 7 * 24 * time.Hour

const renderCacheExtKey = "httpd.render_cache"

type renderCacheEntry struct {
	html     template.HTML
	storedAt time.Time
}

// renderCache is the process-wide rendered-content cache. The key carries the
// row's updated_at, so any write (including Tag#touch_articles) invalidates
// the entry exactly like the Rails cache_key_with_version.
type renderCache struct {
	mu   sync.Mutex
	now  func() time.Time // overridable in tests
	data map[string]renderCacheEntry
}

func newRenderCache() *renderCache {
	return &renderCache{now: time.Now, data: make(map[string]renderCacheEntry)}
}

// renderCache returns the shared cache, creating it on first use.
func (s *Server) renderCache() *renderCache {
	v, _ := s.Ext.LoadOrStore(renderCacheExtKey, newRenderCache())
	return v.(*renderCache)
}

// fetch returns the cached rendered HTML for the row version, or stores and
// returns rawHTML on a miss. content_html is sanitized and lazy-loaded at
// write time (plan section 4.4), so rendering is only the template.HTML wrap.
func (c *renderCache) fetch(kind string, id, updatedAt int64, rawHTML string) template.HTML {
	key := kind + ":" + strconv.FormatInt(id, 10) + ":" + strconv.FormatInt(updatedAt, 10)
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.data[key]; ok && c.now().Sub(e.storedAt) < renderCacheTTL {
		return e.html
	}
	// Drop expired entries so versioned keys cannot grow the map forever.
	for k, e := range c.data {
		if c.now().Sub(e.storedAt) >= renderCacheTTL {
			delete(c.data, k)
		}
	}
	rendered := template.HTML(rawHTML) //nolint:gosec // content_html is sanitized at write time
	c.data[key] = renderCacheEntry{html: rendered, storedAt: c.now()}
	return rendered
}

// authenticated mirrors Rails' authenticated?: a valid session cookie exists.
// Unlike RequireAuth it never redirects.
func (s *Server) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	_, err = s.Q.GetSessionByToken(r.Context(), cookie.Value)
	return err == nil
}

// publicNotFound mirrors ApplicationController#render_not_found: the static
// 404 body with the short public cache lifetime the Rails controllers set.
func (s *Server) publicNotFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "<!DOCTYPE html>\n<html><head><title>404 Not Found</title></head><body><h1>404 Not Found</h1></body></html>\n")
}

// normalizeSiteURL mirrors ApplicationHelper#normalized_site_url: trimmed,
// one trailing slash chomped, https:// prepended when no http(s) scheme; ""
// when unset.
func normalizeSiteURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(u, "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return u
}

// tzLocation resolves settings.time_zone, falling back to UTC like
// templates.FormatTime.
func tzLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// likeEscaper mirrors ActiveRecord::Base.sanitize_sql_like: each of \, % and
// _ is prefixed with a backslash.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likePattern mirrors "%#{sanitize_sql_like(query)}%".
func likePattern(q string) string { return "%" + likeEscaper.Replace(q) + "%" }

// maxPageNumber caps page numbers so (page-1)*per_page cannot overflow,
// mirroring will_paginate's BIGINT offset guard (InvalidPage -> 404).
const maxPageNumber = math.MaxInt64 / 64

// parseIndexPage mirrors articles#index: params[:page].present? ? to_i : 1.
// Blank means page 1; Ruby's to_i parses a leading integer (0 when none);
// WillPaginate::InvalidPage (404 here) when the result is out of range.
func parseIndexPage(values url.Values) (int64, bool) {
	raw, present := values["page"]
	if !present {
		return 1, true
	}
	last := raw[len(raw)-1] // Rails params take the last of repeated keys
	if domain.IsBlank(last) {
		return 1, true
	}
	n := rubyToI(last)
	if n < 1 || n > maxPageNumber {
		return 0, false
	}
	return n, true
}

// parseStrictPage mirrors paginate(page: params[:page]) (tags#show): an absent
// param means page 1, otherwise Ruby Integer() semantics — the whole string
// must be a decimal integer or WillPaginate::InvalidPage (404 here) is raised.
func parseStrictPage(values url.Values) (int64, bool) {
	raw, present := values["page"]
	if !present {
		return 1, true
	}
	n, ok := rubyInteger(raw[len(raw)-1])
	if !ok || n < 1 || n > maxPageNumber {
		return 0, false
	}
	return n, true
}

// rubyToI ports String#to_i: leading whitespace skipped, optional sign, then
// digits up to the first non-digit; no digits yields 0.
func rubyToI(s string) int64 {
	s = strings.TrimLeft(s, " \t\n\v\f\r")
	sign := int64(1)
	if rest, ok := strings.CutPrefix(s, "+"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "-"); ok {
		sign, s = -1, rest
	}
	n, digits := int64(0), 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		digits++
		if n > (maxPageNumber-9)/10 {
			n = maxPageNumber // saturate; the caller 404s on it
			continue
		}
		n = n*10 + int64(c-'0')
	}
	if digits == 0 {
		return 0
	}
	return sign * n
}

// rubyInteger ports Integer(str): surrounding whitespace and a sign are
// allowed, the rest must be decimal digits. Ruby's hex/underscore forms are
// rejected here (they 404 instead of parsing) — a deliberate simplification.
func rubyInteger(s string) (int64, bool) {
	t := strings.TrimSpace(s)
	neg := false
	if rest, ok := strings.CutPrefix(t, "+"); ok {
		t = rest
	} else if rest, ok := strings.CutPrefix(t, "-"); ok {
		neg, t = true, rest
	}
	if t == "" {
		return 0, false
	}
	n := int64(0)
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if n > (maxPageNumber-9)/10 {
			n = maxPageNumber
			continue
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}

// siteChrome carries the site-wide data the public layout partials need
// (site_settings + navbar_items of the Rails application layout).
type siteChrome struct {
	Title       string
	Description string
	SiteURL     string // normalized_site_url ("" when unset)
	TimeZone    string
	HeadCode    template.HTML // trusted admin-configured code, rendered raw
	CustomCSS   template.CSS  // trusted admin-configured CSS, rendered raw
	ToolCode    template.HTML
	Giscus      template.HTML
	SocialLinks []socialLink
	NavPages    []navPage
	HasTags     bool
	Query       string            // current ?q= term, prefilled in the nav search box
	Subscribe   subscribeFormData // navbar newsletter form; populated on "/" only (T16)
}

type socialLink struct {
	Platform string
	Name     string // titleized platform (Go SocialLink has no name field)
	URL      string
	Icon     string
}

type navPage struct {
	Title    string
	URL      string
	External bool
}

// chrome builds the shared public-layout context.
func (s *Server) chrome(ctx context.Context, query string) (siteChrome, error) {
	st, err := s.Settings().Get(ctx)
	if err != nil {
		return siteChrome{}, err
	}
	c := siteChrome{
		Title:       st.Title.String,
		Description: st.Description.String,
		SiteURL:     normalizeSiteURL(st.Url.String),
		TimeZone:    st.TimeZone,
		HeadCode:    template.HTML(st.HeadCode.String), //nolint:gosec // trusted admin content, raw like Rails
		CustomCSS:   template.CSS(st.CustomCss.String), //nolint:gosec // trusted admin content, raw like Rails
		ToolCode:    template.HTML(st.ToolCode.String), //nolint:gosec // trusted admin content, raw like Rails
		Giscus:      template.HTML(st.Giscus.String),   //nolint:gosec // trusted admin content, raw like Rails
		Query:       query,
	}
	links, err := settings.UnmarshalSocialLinks(st.SocialLinks.String)
	if err != nil {
		return siteChrome{}, err
	}
	for platform, link := range links {
		c.SocialLinks = append(c.SocialLinks, socialLink{
			Platform: platform,
			Name:     titleize(platform),
			URL:      link.URL,
			Icon:     link.Icon,
		})
	}
	sort.Slice(c.SocialLinks, func(i, j int) bool { return c.SocialLinks[i].Platform < c.SocialLinks[j].Platform })

	navRows, err := s.Q.ListNavbarPages(ctx)
	if err != nil {
		return siteChrome{}, err
	}
	for _, row := range navRows {
		item := navPage{Title: row.Title.String, URL: comments.PagePath(row.Slug.String)}
		if row.RedirectUrl.String != "" {
			item.URL = row.RedirectUrl.String
			item.External = true
		}
		c.NavPages = append(c.NavPages, item)
	}
	tagCount, err := s.Q.CountPublicTags(ctx)
	if err != nil {
		return siteChrome{}, err
	}
	c.HasTags = tagCount > 0
	return c, nil
}

// titleize mirrors String#titleize for a single lowercase word.
func titleize(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// pageLink is one element of the pagination nav.
type pageLink struct {
	Page    int
	URL     string
	Gap     bool
	Current bool
}

// pagination feeds the "pagination" partial; empty Links render nothing,
// mirroring will_paginate's early exit when total_pages <= 1.
type pagination struct {
	Links   []pageLink
	PrevURL string
	NextURL string
}

// buildPagination mirrors will_paginate: total_pages is at least 1, the nav
// renders only when there is more than one page, and previous/next links are
// disabled spans at the ends.
func buildPagination(current, total int64, perPage int64, urlFor func(page int64) string) pagination {
	totalPages := int64(1)
	if total > 0 {
		totalPages = (total + perPage - 1) / perPage
	}
	if totalPages <= 1 {
		return pagination{}
	}
	var p pagination
	for _, item := range templates.PaginationWindow(int(current), int(totalPages)) {
		link := pageLink{Page: item.Page, Gap: item.Gap, Current: item.Page == int(current)}
		if !item.Gap {
			link.URL = urlFor(int64(item.Page))
		}
		p.Links = append(p.Links, link)
	}
	if current > 1 {
		p.PrevURL = urlFor(current - 1)
	}
	if current < totalPages {
		p.NextURL = urlFor(current + 1)
	}
	return p
}

// pageURLFunc builds pagination URLs from the current request, preserving the
// query string (will_paginate keeps GET params).
func pageURLFunc(r *http.Request) func(page int64) string {
	return func(page int64) string {
		q := r.URL.Query()
		q.Set("page", strconv.FormatInt(page, 10))
		return r.URL.Path + "?" + q.Encode()
	}
}

// articleListItem is one entry of the public article list.
type articleListItem struct {
	ID          int64
	URL         string
	DateUnix    int64
	Title       string
	SummaryHTML template.HTML // simple_format(description || excerpt)
	ContentHTML template.HTML // rendered content, shown when there is no summary
	SourceRef   template.HTML // source reference for title-less entries
	Tags        []query.Tag
}

// articleListData feeds the "article_list" partial.
type articleListData struct {
	Items    []articleListItem
	IsHome   bool // request.path == root_path in the Rails partial
	TimeZone string
	Page     pagination
}

// listItems assembles the display entries, mirroring articles/_article_list:
// summary = description.presence || (title.present? ? excerpt : nil); a blank
// summary falls back to the rendered content; title-less entries show the
// source reference first.
func (s *Server) listItems(ctx context.Context, articles []query.Article) ([]articleListItem, error) {
	items := make([]articleListItem, 0, len(articles))
	for _, a := range articles {
		tags, err := s.Q.ListTagsForArticle(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		item := articleListItem{
			ID:       a.ID,
			URL:      comments.ArticlePath(s.Cfg.ArticleRoutePrefix, a.Slug.String),
			DateUnix: a.CreatedAt,
			Title:    a.Title.String,
			Tags:     tags,
		}
		summary := a.Description.String
		if domain.IsBlank(summary) && !domain.IsBlank(a.Title.String) {
			summary = a.Excerpt.String
		}
		if domain.IsBlank(summary) {
			item.ContentHTML = s.renderCache().fetch("article", a.ID, a.UpdatedAt, a.ContentHtml.String)
		} else {
			item.SummaryHTML = simpleFormat(summary)
		}
		if domain.IsBlank(a.Title.String) {
			item.SourceRef = buildSourceReference(a.SourceAuthor.String, a.SourceContent.String, a.SourceUrl.String)
		}
		items = append(items, item)
	}
	return items, nil
}

// multiNewlineRE splits simple_format paragraphs (two or more newlines).
var multiNewlineRE = regexp.MustCompile(`\n{2,}`)

// simpleFormat mirrors Rails' simple_format for plain-text summaries: the
// text is HTML-escaped, single newlines become <br>, blank-line-separated
// chunks become <p> elements.
func simpleFormat(text string) template.HTML {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = html.EscapeString(text)
	paras := multiNewlineRE.Split(text, -1)
	for i, p := range paras {
		paras[i] = "<p>" + strings.ReplaceAll(p, "\n", "<br>") + "</p>"
	}
	return template.HTML(strings.Join(paras, "\n\n")) //nolint:gosec // escaped above
}

// buildSourceReference renders articles/_source_reference.html.erb semantics:
// present only when source_url is set (Article#has_source?).
func buildSourceReference(author, content, rawURL string) template.HTML {
	if domain.IsBlank(rawURL) {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="source-reference" style="display: flex; align-items: flex-start; gap: 0.75rem; margin-bottom: 0.75rem;">`)
	b.WriteString(`<div style="flex: 1;">`)
	if !domain.IsBlank(author) {
		b.WriteString(`<span style="font-weight: 600; color: #495057; font-size: 0.95rem;">`)
		b.WriteString(html.EscapeString(author))
		b.WriteString(`</span>`)
	}
	b.WriteString(`</div></div>`)
	b.WriteString(`<blockquote class="source-reference__quote">`)
	if !domain.IsBlank(content) {
		b.WriteString(string(simpleFormat(content)))
	}
	b.WriteString(`<div class="source-reference__links" style="display: flex; flex-wrap: wrap; gap: 0.75rem; font-size: 0.85rem;">`)
	b.WriteString(`<a href="` + html.EscapeString(rawURL) + `" target="_blank" rel="noopener noreferrer" style="color: #007bff; text-decoration: none;"><small>Original</small></a>`)
	b.WriteString(`</div></blockquote>`)
	return template.HTML(b.String()) //nolint:gosec // parts escaped above
}

// commentsSectionData feeds the comments block of the show pages.
type commentsSectionData struct {
	Enabled bool
	Count   int
	Tree    []comments.Threaded
	Form    comments.FormData
	Giscus  template.HTML
}

// commentsSection assembles the comment display tree and form context for a
// commentable, mirroring the comments section of articles/show and
// pages/show. Comments render only when comment = 1 (the Rails comment? gate).
func (s *Server) commentsSection(ctx context.Context, commentableType string, commentableID int64, slug string, comment int64, chrome siteChrome) (commentsSectionData, error) {
	sec := commentsSectionData{Enabled: comment == 1, Giscus: chrome.Giscus}
	if !sec.Enabled {
		return sec, nil
	}
	list, err := s.Q.ListCommentsForCommentable(ctx, query.ListCommentsForCommentableParams{
		CommentableType: sql.NullString{String: commentableType, Valid: true},
		CommentableID:   sql.NullInt64{Int64: commentableID, Valid: true},
	})
	if err != nil {
		return sec, err
	}
	tree := comments.BuildTree(list)
	challenge, token := captcha.New(s.Cfg.HMACSecret, captcha.TTL).IssueChallenge()
	param := "article_id"
	if commentableType == "Page" {
		param = "page_id"
	}
	base := comments.FormData{
		Action:   "/comments?" + param + "=" + url.QueryEscape(slug),
		Question: challenge.Question,
		Token:    token,
		A:        challenge.A,
		B:        challenge.B,
		Op:       challenge.Op,
	}
	comments.PrepareDisplay(tree, chrome.TimeZone, base)
	sec.Tree = tree
	sec.Form = base
	sec.Count = comments.VisibleCount(list)
	return sec, nil
}

// firstImageSrc mirrors the RemoteImageWrapper branch of
// Article#first_image_attachment for stored HTML: the first <img src>.
func firstImageSrc(contentHTML string) string {
	nodes, err := xhtml.ParseFragment(strings.NewReader(contentHTML), &xhtml.Node{Type: xhtml.ElementNode, Data: "body"})
	if err != nil {
		return ""
	}
	var walk func(n *xhtml.Node) string
	walk = func(n *xhtml.Node) string {
		if n.Type == xhtml.ElementNode && n.Data == "img" {
			for _, attr := range n.Attr {
				if attr.Key == "src" {
					return attr.Val
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if src := walk(c); src != "" {
				return src
			}
		}
		return ""
	}
	for _, n := range nodes {
		if src := walk(n); src != "" {
			return src
		}
	}
	return ""
}

// absoluteURL mirrors ApplicationHelper#absolute_og_image: relative paths are
// resolved against the normalized site URL.
func absoluteURL(siteURL, path string) string {
	p := strings.TrimSpace(path)
	if p == "" || strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	if siteURL == "" {
		return p
	}
	if strings.HasPrefix(p, "/") {
		return siteURL + p
	}
	return siteURL + "/" + p
}

// slugParam reads the {slug} path parameter.
func slugParam(r *http.Request) string { return chi.URLParam(r, "slug") }
