package httpd

import (
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/service/comments"
	"rables/internal/templates"
)

// RegisterArticleRoutes mounts the public article index and show pages,
// mirroring the Rails root route plus the ARTICLE_ROUTE_PREFIX scope (routes
// must stay last in NewRouter — /{slug} is the catch-all):
//
//	root "articles#index"                        GET /
//	scope path: article_route_prefix do
//	  get "/"      => articles#index             GET /{prefix}/
//	  get "/:slug" => articles#show              GET /{prefix}/{slug}
//
// Without a prefix the scoped routes sit at / and /{slug}. The Rails root
// route serves the index at "/" even when a prefix is configured, so both
// index paths are registered then.
func RegisterArticleRoutes(r chi.Router, s *Server) {
	prefix := strings.Trim(s.Cfg.ArticleRoutePrefix, "/")
	r.Get("/", s.publicArticleIndex)
	if prefix == "" {
		r.Get("/{slug}", s.publicArticleShow)
		return
	}
	r.Get("/"+prefix+"/", s.publicArticleIndex)
	r.Get("/"+prefix+"/{slug}", s.publicArticleShow)
}

// publicIndexData feeds public_index.html.
type publicIndexData struct {
	Flash     templates.Flash
	Chrome    siteChrome
	List      articleListData
	Subscribe subscribeFormData // navbar form, populated on "/" only (T16)
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
	q := r.URL.Query().Get("q")
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
	w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=900")
	data := publicIndexData{
		Flash:  PopFlash(r, w),
		Chrome: chrome,
		List: articleListData{
			Items:    items,
			IsHome:   r.URL.Path == "/",
			TimeZone: chrome.TimeZone,
			Page:     buildPagination(page, total, publicArticlesPerPage, pageURLFunc(r)),
		},
	}
	// The navbar subscription form shows only on the root page
	// (current_page?(root_path) in the Rails _nav_bar partial).
	if r.URL.Path == "/" {
		data.Subscribe = s.subscribeInlineForm(ctx, 0)
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
	MetaTitle       string
	MetaDescription string
	MetaImage       string
	FullURL         string
	Tags            []query.Tag
	SourceRef       template.HTML
	Comments        commentsSectionData
}

// publicArticleShow renders GET /{slug} (or /{prefix}/{slug}), mirroring
// ArticlesController#show: publish/shared are public, other statuses require
// authentication, anything else is the static 404.
func (s *Server) publicArticleShow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := slugParam(r)
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
	if public {
		// Must stay private: the comment form embeds a per-session captcha token.
		w.Header().Set("Cache-Control", "private, max-age=3600")
	} else {
		w.Header().Set("Cache-Control", "private, no-cache")
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

	s.render(w, http.StatusOK, "public_article", publicArticleData{
		Flash:           PopFlash(r, w),
		Chrome:          chrome,
		Title:           article.Title.String,
		DateUnix:        article.CreatedAt,
		UpdatedUnix:     article.UpdatedAt,
		ContentHTML:     s.renderCache().fetch("article", article.ID, article.UpdatedAt, article.ContentHtml.String),
		MetaTitle:       metaTitle,
		MetaDescription: metaDescription,
		MetaImage:       metaImage,
		FullURL:         chrome.SiteURL + comments.ArticlePath(s.Cfg.ArticleRoutePrefix, slug),
		Tags:            tags,
		SourceRef:       buildSourceReference(article.SourceAuthor.String, article.SourceContent.String, article.SourceUrl.String),
		Comments:        section,
	})
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
