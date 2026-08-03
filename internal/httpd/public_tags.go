package httpd

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/templates"
)

// RegisterPublicRoutes mounts the non-article public pages (feeds, sitemap,
// tags, pages). It must be wired before RegisterArticleRoutes so the static
// paths win over the /{slug} catch-all. /tags/{slug}.rss is registered before
// /tags/{slug} because chi matches same-position param edges in registration
// order and {slug} alone would swallow the ".rss" suffix.
func RegisterPublicRoutes(r chi.Router, s *Server) {
	r.Get("/feed.xml", s.publicFeed)
	r.Get("/sitemap.xml", s.publicSitemap)
	r.Get("/tags", s.publicTagsIndex)
	r.Get("/tags/{slug}.rss", s.publicTagRSS)
	r.Get("/tags/{slug}", s.publicTagShow)
	r.Get("/pages/{slug}", s.publicPageShow)
}

// tagCount is a tag with its published article count (TagsController#index).
type tagCount struct {
	query.Tag
	Count int64
}

// publicTagsData feeds public_tags.html.
type publicTagsData struct {
	Flash  templates.Flash
	Chrome siteChrome
	Tags   []tagCount
}

// publicTagsIndex renders GET /tags, mirroring TagsController#index: all tags
// alphabetically with their published article counts.
func (s *Server) publicTagsIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tags, err := s.Q.ListPublicTags(ctx)
	if err != nil {
		s.listError(w, "list tags", err)
		return
	}
	counts, err := s.Q.ListPublishedCountsByTag(ctx)
	if err != nil {
		s.listError(w, "count published articles by tag", err)
		return
	}
	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	byTag := make(map[int64]int64, len(counts))
	for _, row := range counts {
		byTag[row.TagID] = row.PublishedCount
	}
	data := publicTagsData{Flash: PopFlash(r, w), Chrome: chrome, Tags: make([]tagCount, 0, len(tags))}
	for _, t := range tags {
		data.Tags = append(data.Tags, tagCount{Tag: t, Count: byTag[t.ID]})
	}
	s.render(w, http.StatusOK, "public_tags", data)
}

// publicTagData feeds public_tag.html.
type publicTagData struct {
	Flash     templates.Flash
	Chrome    siteChrome
	Tag       query.Tag
	Total     int64
	List      articleListData
	Subscribe subscribeFormData // inline form preselecting this tag (T16)
}

// publicTagShow renders GET /tags/{slug}, mirroring TagsController#show (HTML
// format): the tag's published articles, 20 per page.
func (s *Server) publicTagShow(w http.ResponseWriter, r *http.Request) {
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
	page, ok := parseStrictPage(r.URL.Query())
	if !ok {
		s.publicNotFound(w)
		return
	}
	articles, err := s.Q.ListPublishedArticlesByTag(ctx, query.ListPublishedArticlesByTagParams{
		TagID:  tag.ID,
		Limit:  publicTagPerPage,
		Offset: (page - 1) * publicTagPerPage,
	})
	if err != nil {
		s.listError(w, "list tag articles", err)
		return
	}
	total, err := s.Q.CountPublishedArticlesByTag(ctx, tag.ID)
	if err != nil {
		s.listError(w, "count tag articles", err)
		return
	}
	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	items, err := s.listItems(ctx, articles)
	if err != nil {
		s.listError(w, "list article tags", err)
		return
	}
	s.render(w, http.StatusOK, "public_tag", publicTagData{
		Flash:  PopFlash(r, w),
		Chrome: chrome,
		Tag:    tag,
		Total:  total,
		List: articleListData{
			Items:    items,
			TimeZone: chrome.TimeZone,
			Page:     buildPagination(page, total, publicTagPerPage, pageURLFunc(r)),
		},
		Subscribe: s.subscribeInlineForm(ctx, tag.ID),
	})
}
