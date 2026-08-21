package httpd

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/service/comments"
	"rables/internal/service/htmlarchive"
	"rables/internal/templates"
)

// RegisterArticleRoutes mounts the public article index and show pages,
// mirroring the Rails root route plus the article-route-prefix scope (routes
// must stay last in NewRouter — /{p1} is the catch-all):
//
//	root "articles#index"                        GET /
//	scope path: article_route_prefix do
//	  get "/"      => articles#index             GET /{prefix}/
//	  get "/:slug" => articles#show              GET /{prefix}/{slug}
//
// The prefix is a request-time setting (s.routePrefix), so generic wildcard
// routes are registered and the handlers validate the prefix per request: a
// settings change takes effect without a restart or re-registration. chi
// requires one param name per path position, so the root wildcard is {p1}
// everywhere; the handlers interpret it as prefix or slug.
func RegisterArticleRoutes(r chi.Router, s *Server) {
	r.Get("/", s.publicArticleIndex)
	r.Get("/{p1}", s.publicArticleShowAtRoot)
	r.Get("/{p1}/", s.publicArticleIndexAtPrefix)
	r.Get("/{p1}/{slug}", s.publicArticleShowAtPrefix)
}

// routePrefix resolves the effective public article route prefix: the admin
// setting (settings.article_route_prefix) when set, otherwise the
// ARTICLE_ROUTE_PREFIX environment value. It is read per request — through
// the settings cache — so a settings change applies immediately.
func (s *Server) routePrefix(ctx context.Context) string {
	if st, err := s.Settings().Get(ctx); err == nil {
		if p := strings.Trim(st.ArticleRoutePrefix.String, "/"); p != "" {
			return p
		}
	}
	return strings.Trim(s.Cfg.ArticleRoutePrefix, "/")
}

// publicArticleShowAtRoot serves /{slug} only when no route prefix is
// configured; with a prefix the bare slug is not routed (Rails scope
// behavior) and answers the static 404.
func (s *Server) publicArticleShowAtRoot(w http.ResponseWriter, r *http.Request) {
	if s.routePrefix(r.Context()) != "" {
		s.publicNotFound(w)
		return
	}
	s.publicArticleShow(w, r, slugParam(r, "p1"))
}

// publicArticleIndexAtPrefix serves /{p1}/ as the article index only when p1
// matches the configured prefix.
func (s *Server) publicArticleIndexAtPrefix(w http.ResponseWriter, r *http.Request) {
	if p := s.routePrefix(r.Context()); p == "" || chi.URLParam(r, "p1") != p {
		s.publicNotFound(w)
		return
	}
	s.publicArticleIndex(w, r)
}

// publicArticleShowAtPrefix serves /{p1}/{slug} only when p1 matches the
// configured prefix.
func (s *Server) publicArticleShowAtPrefix(w http.ResponseWriter, r *http.Request) {
	if p := s.routePrefix(r.Context()); p == "" || chi.URLParam(r, "p1") != p {
		s.publicNotFound(w)
		return
	}
	s.publicArticleShow(w, r, slugParam(r, "slug"))
}

// publicIndexData feeds public_index.html.
type publicIndexData struct {
	Flash  templates.Flash
	Chrome siteChrome
	List   articleListData
}

// publicArticleIndex renders GET / (and GET /{prefix}/), mirroring
// ArticlesController#index (HTML format): published articles, newest first,
// 10 per page, optional ?q= search.
func (s *Server) publicArticleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, ok := parseIndexPage(r.URL.Query())
	if !ok {
		s.publicNotFound(w)
		return
	}
	q := firstRunes(r.URL.Query().Get("q"), publicSearchMaxRunes)
	offset := (page - 1) * publicArticlesPerPage

	var articles []query.Article
	var total int64
	if domain.IsBlank(q) {
		var err error
		if articles, err = s.Q.ListPublishedArticles(ctx, query.ListPublishedArticlesParams{Limit: publicArticlesPerPage, Offset: offset}); err != nil {
			s.listError(w, "list published articles", err)
			return
		}
		if total, err = s.Q.CountPublishedArticles(ctx); err != nil {
			s.listError(w, "count published articles", err)
			return
		}
	} else {
		term := sql.NullString{String: likePattern(q), Valid: true}
		var err error
		if articles, err = s.Q.SearchPublishedArticles(ctx, query.SearchPublishedArticlesParams{
			Title: term, Slug: term, Description: term, ContentHtml: term,
			Limit: publicArticlesPerPage, Offset: offset,
		}); err != nil {
			s.listError(w, "search published articles", err)
			return
		}
		if total, err = s.Q.CountSearchPublishedArticles(ctx, query.CountSearchPublishedArticlesParams{
			Title: term, Slug: term, Description: term, ContentHtml: term,
		}); err != nil {
			s.listError(w, "count search results", err)
			return
		}
	}

	chrome, err := s.chrome(ctx, q)
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	items, err := s.listItems(ctx, articles)
	if err != nil {
		s.listError(w, "list article tags", err)
		return
	}
	// A present flash cookie makes PopFlash emit a Set-Cookie that clears it,
	// and a rendered flash is one-time, per-user content; such a response
	// must not be stored by shared caches. The check is cookie presence, not
	// the popped value: a malformed cookie pops a zero Flash yet the
	// Set-Cookie header still goes out.
	_, flashCookieErr := r.Cookie(flashCookieName)
	flash := s.PopFlash(r, w)
	cacheControl := "public, max-age=300, s-maxage=900"
	if flashCookieErr == nil {
		cacheControl = "private, no-cache"
	}
	w.Header().Set("Cache-Control", cacheControl)
	data := publicIndexData{
		Flash:  flash,
		Chrome: chrome,
		List: articleListData{
			Items:    items,
			IsHome:   r.URL.Path == "/",
			TimeZone: chrome.TimeZone,
			Page:     buildPagination(page, total, publicArticlesPerPage, pageURLFunc(r)),
		},
	}
	s.render(w, http.StatusOK, "public_index", data)
}

// publicArticleData feeds public_article.html.
type publicArticleData struct {
	Flash           templates.Flash
	Chrome          siteChrome
	Title           string
	DateUnix        int64
	UpdatedUnix     int64
	ContentHTML     template.HTML
	ArchiveURL      string // html_archive iframe src; "" renders ContentHTML
	MetaTitle       string
	MetaDescription string
	MetaImage       string
	FullURL         string
	Tags            []query.Tag
	SourceRef       template.HTML
	SocialPosts     []socialPostLink
	Comments        commentsSectionData
}

// socialPostLink is one "also posted on" entry: the platform display name and
// the recorded post URL (articles/show.html.erb social_media_posts links).
type socialPostLink struct {
	Name string
	URL  string
}

// socialPostPlatformNames maps crosspost platform keys to display names.
var socialPostPlatformNames = map[string]string{
	"mastodon":    "Mastodon",
	"twitter":     "Twitter",
	"bluesky":     "Bluesky",
	"xiaohongshu": "Xiaohongshu",
}

// publicArticleShow renders GET /{slug} (or /{prefix}/{slug}), mirroring
// ArticlesController#show: publish/shared are public, other statuses require
// authentication, anything else is the static 404. The slug is resolved by
// the routing wrapper (the root wildcard is named {p1}, see
// RegisterArticleRoutes).
func (s *Server) publicArticleShow(w http.ResponseWriter, r *http.Request, slug string) {
	ctx := r.Context()
	article, err := s.Q.GetPublicArticleBySlug(ctx, sql.NullString{String: slug, Valid: true})
	if errors.Is(err, sql.ErrNoRows) {
		s.publicNotFound(w)
		return
	}
	if err != nil {
		s.listError(w, "get article by slug", err)
		return
	}
	public := article.Status == int64(domain.StatusPublish) || article.Status == int64(domain.StatusShared)
	if !public && !s.authenticated(r) {
		s.publicNotFound(w)
		return
	}

	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	tags, err := s.Q.ListTagsForArticle(ctx, article.ID)
	if err != nil {
		s.listError(w, "list article tags", err)
		return
	}
	socialPosts, err := s.socialPostLinks(ctx, article.ID)
	if err != nil {
		s.listError(w, "list article social posts", err)
		return
	}
	section, err := s.commentsSection(ctx, "Article", article.ID, slug, article.Comment, chrome)
	if err != nil {
		s.listError(w, "list comments", err)
		return
	}

	metaTitle := article.MetaTitle.String
	if domain.IsBlank(metaTitle) {
		metaTitle = article.Title.String // seo_meta_title
	}
	metaDescription := article.MetaDescription.String
	if domain.IsBlank(metaDescription) {
		metaDescription = seoDescription(article)
	}
	metaImage := article.MetaImage.String
	if domain.IsBlank(metaImage) {
		metaImage = firstImageSrc(article.ContentHtml.String)
	}
	metaImage = absoluteURL(chrome.SiteURL, metaImage)

	// Must stay private: the comment form embeds a per-session captcha token.
	// A present flash cookie means the page renders a one-time flash, so the
	// response must not be cached (same check as publicArticleIndex). Set only
	// after every fallible query, or http.Error would send a cached 500.
	_, flashCookieErr := r.Cookie(flashCookieName)
	cacheControl := "private, no-cache"
	if public && flashCookieErr != nil {
		cacheControl = "private, max-age=3600"
	}
	w.Header().Set("Cache-Control", cacheControl)

	s.render(w, http.StatusOK, "public_article", publicArticleData{
		Flash:           s.PopFlash(r, w),
		Chrome:          chrome,
		Title:           article.Title.String,
		DateUnix:        article.CreatedAt,
		UpdatedUnix:     article.UpdatedAt,
		ContentHTML:     s.renderCache().fetch("article", article.ID, article.UpdatedAt, article.ContentHtml.String),
		ArchiveURL:      publicArchiveURL(article.ContentType, htmlarchive.Article, article.ID),
		MetaTitle:       metaTitle,
		MetaDescription: metaDescription,
		MetaImage:       metaImage,
		FullURL:         chrome.SiteURL + comments.ArticlePath(s.routePrefix(ctx), slug),
		Tags:            tags,
		SourceRef:       buildSourceReference(article.SourceContent.String, article.SourceUrl.String),
		SocialPosts:     socialPosts,
		Comments:        section,
	})
}

// socialPostLinks builds the "also posted on" links of the article page:
// recorded crosspost-platform posts with a URL (Rails selects
// Crosspost::PLATFORMS and renders only url.present? entries).
func (s *Server) socialPostLinks(ctx context.Context, articleID int64) ([]socialPostLink, error) {
	posts, err := s.Q.ListSocialPostsByArticleID(ctx, articleID)
	if err != nil {
		return nil, err
	}
	var out []socialPostLink
	for _, post := range posts {
		if post.Url == "" || !isCrosspostPlatform(post.Platform) {
			continue
		}
		out = append(out, socialPostLink{Name: socialPostPlatformNames[post.Platform], URL: post.Url})
	}
	return out, nil
}

// publicArchiveURL returns the sandboxed iframe src for an html_archive
// record, "" for every other content type.
func publicArchiveURL(contentType string, kind htmlarchive.Kind, id int64) string {
	if contentType != string(domain.ContentTypeHTMLArchive) {
		return ""
	}
	return archiveURL(kind, id)
}

// seoDescription mirrors Article#seo_meta_description: the squished plain
// text, truncated past 160 characters (Ruby text[0..156] + "..."), falling
// back to the description.
func seoDescription(a query.Article) string {
	text := domain.Squish(domain.PlainText(a.ContentHtml.String))
	if text == "" {
		return a.Description.String
	}
	if runes := []rune(text); len(runes) > 160 {
		text = string(runes[:157]) + "..."
	}
	return text
}

// listError logs a query failure and answers 500.
func (s *Server) listError(w http.ResponseWriter, op string, err error) {
	s.Log.Error(op, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
