package httpd

import (
	"database/sql"
	"errors"
	"html/template"
	"net/http"

	"rables/internal/domain"
	"rables/internal/templates"
)

// publicPageData feeds public_page.html.
type publicPageData struct {
	Flash       templates.Flash
	Chrome      siteChrome
	Title       string
	ContentHTML template.HTML
	Comments    commentsSectionData
}

// publicPageShow renders GET /pages/{slug}, mirroring PagesController#show:
// publish/shared are public, other statuses require authentication. A page
// with a redirect_url answers 302 instead of rendering (plan section 4.12).
func (s *Server) publicPageShow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := slugParam(r)
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
	if public {
		// Must stay private: the comment form embeds a per-session captcha token.
		w.Header().Set("Cache-Control", "private, max-age=86400")
	} else {
		w.Header().Set("Cache-Control", "private, no-cache")
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
	s.render(w, http.StatusOK, "public_page", publicPageData{
		Flash:       PopFlash(r, w),
		Chrome:      chrome,
		Title:       page.Title.String,
		ContentHTML: s.renderCache().fetch("page", page.ID, page.UpdatedAt, page.ContentHtml.String),
		Comments:    section,
	})
}
