package httpd

import (
	"database/sql"
	"errors"
	"html/template"
	"net/http"

	"rables/internal/domain"
	"rables/internal/service/htmlarchive"
	"rables/internal/templates"
)

// publicPageData feeds public_page.html.
type publicPageData struct {
	Flash       templates.Flash
	Chrome      siteChrome
	Title       string
	ContentHTML template.HTML
	ArchiveURL  string // html_archive iframe src; "" renders ContentHTML
	Comments    commentsSectionData
}

// publicPageShow renders GET /pages/{slug}, mirroring PagesController#show:
// publish/shared are public, other statuses require authentication. A page
// with a redirect_url answers 302 instead of rendering (plan section 4.12).
func (s *Server) publicPageShow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := slugParam(r, "slug")
	page, err := s.Q.GetPublicPageBySlug(ctx, sql.NullString{String: slug, Valid: true})
	if errors.Is(err, sql.ErrNoRows) {
		s.publicNotFound(w)
		return
	}
	if err != nil {
		s.listError(w, "get page by slug", err)
		return
	}
	public := page.Status == int64(domain.StatusPublish) || page.Status == int64(domain.StatusShared)
	if !public && !s.authenticated(r) {
		s.publicNotFound(w)
		return
	}
	if !domain.IsBlank(page.RedirectUrl.String) {
		http.Redirect(w, r, page.RedirectUrl.String, http.StatusFound)
		return
	}

	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	section, err := s.commentsSection(ctx, "Page", page.ID, slug, page.Comment, chrome)
	if err != nil {
		s.listError(w, "list comments", err)
		return
	}
	// Must stay private: the comment form embeds a per-session captcha token.
	// A present flash cookie means the page renders a one-time flash, so the
	// response must not be cached (same check as publicArticleIndex). Set only
	// after every fallible query, or http.Error would send a cached 500.
	_, flashCookieErr := r.Cookie(flashCookieName)
	cacheControl := "private, no-cache"
	if public && flashCookieErr != nil {
		cacheControl = "private, max-age=86400"
	}
	w.Header().Set("Cache-Control", cacheControl)
	s.render(w, http.StatusOK, "public_page", publicPageData{
		Flash:       s.PopFlash(r, w),
		Chrome:      chrome,
		Title:       page.Title.String,
		ContentHTML: s.renderCache().fetch("page", page.ID, page.UpdatedAt, page.ContentHtml.String),
		ArchiveURL:  publicArchiveURL(page.ContentType, htmlarchive.Page, page.ID),
		Comments:    section,
	})
}
