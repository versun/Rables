package httpd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	articlesvc "rables/internal/service/articles"
	tagsvc "rables/internal/service/tags"
	"rables/internal/templates"
)

// adminArticlesPerPage mirrors fetch_articles' will_paginate per_page.
const adminArticlesPerPage = 100

// RegisterArticlesAdminRoutes mounts the admin article UI, mirroring Rails
// namespace :admin: the admin root is the article list and resources
// :articles are served under /admin/posts. HTML forms cannot PATCH/DELETE,
// so update mirrors PATCH /admin/posts/:id as POST /admin/posts/{id},
// destroy mirrors DELETE as POST /admin/posts/{id}/destroy, and the publish /
// unpublish member PATCHes become POSTs. Wired into NewRouter by the
// integrator.
func RegisterArticlesAdminRoutes(r chi.Router, s *Server) {
	r.With(s.RequireAuth).Get("/admin", s.adminArticlesIndex)
	r.With(s.RequireAuth).Get("/admin/", s.adminArticlesIndex)
	r.Route("/admin/posts", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminArticlesIndex)
		r.Get("/new", s.adminArticlesNew)
		r.Post("/", s.adminArticlesCreate)
		r.Get("/drafts", s.adminArticlesDrafts)
		r.Get("/scheduled", s.adminArticlesScheduled)
		r.Post("/batch_destroy", s.adminArticlesBatchDestroy)
		r.Post("/batch_publish", s.adminArticlesBatchPublish)
		r.Post("/batch_unpublish", s.adminArticlesBatchUnpublish)
		r.Post("/batch_add_tags", s.adminArticlesBatchAddTags)
		r.Post("/batch_crosspost", s.adminArticlesBatchCrosspost)
		r.Post("/batch_newsletter", s.adminArticlesBatchNewsletter)
		r.Get("/{id}/edit", s.adminArticlesEdit)
		r.Post("/{id}", s.adminArticlesUpdate)
		r.Post("/{id}/destroy", s.adminArticlesDestroy)
		r.Post("/{id}/publish", s.adminArticlesPublish)
		r.Post("/{id}/unpublish", s.adminArticlesUnpublish)
		r.Post("/{id}/fetch_comments", s.adminArticlesFetchComments)
	})
}

// adminArticlesIndexData feeds admin_articles_index.html.
type adminArticlesIndexData struct {
	Flash    templates.Flash
	Rows     []adminArticleRow
	Q        string // current search term
	Status   string // current status filter ("all" when unset)
	Path     string // list base path (index / drafts / scheduled)
	Page     int
	Pages    int
	TimeZone string
}

// adminArticleRow is one list row with its preloaded associations resolved.
type adminArticleRow struct {
	Article        query.Article
	DisplayTitle   string
	Tags           string
	CommentCount   int64
	FetchPlatforms []string // social posts with a URL on a fetchable platform
}

// StatusName mirrors post.status (badge label).
func (row adminArticleRow) StatusName() string {
	return domain.Status(row.Article.Status).String()
}

// IsTrash switches the row action between trash and delete (Rails index).
func (row adminArticleRow) IsTrash() bool {
	return row.Article.Status == int64(domain.StatusTrash)
}

// adminArticlesIndex renders GET /admin and GET /admin/posts, mirroring
// Admin::ArticlesController#index.
func (s *Server) adminArticlesIndex(w http.ResponseWriter, r *http.Request) {
	s.adminArticlesList(w, r, 0, false, "/admin/posts")
}

// adminArticlesDrafts renders GET /admin/posts/drafts (Article.draft scope).
func (s *Server) adminArticlesDrafts(w http.ResponseWriter, r *http.Request) {
	s.adminArticlesList(w, r, int64(domain.StatusDraft), true, "/admin/posts/drafts")
}

// adminArticlesScheduled renders GET /admin/posts/scheduled
// (Article.scheduled scope).
func (s *Server) adminArticlesScheduled(w http.ResponseWriter, r *http.Request) {
	s.adminArticlesList(w, r, int64(domain.StatusSchedule), true, "/admin/posts/scheduled")
}

// adminArticlesList is fetch_articles: optional status filter on top of the
// scope, search_content, created_at DESC, 100 per page. Invalid page params
// 404 like WillPaginate::InvalidPage.
func (s *Server) adminArticlesList(w http.ResponseWriter, r *http.Request, scopeStatus int64, hasScope bool, path string) {
	ctx := r.Context()

	statusName := r.URL.Query().Get("status")
	statusFilter, filtered := parseStatusFilter(statusName)

	// The drafts/scheduled scopes combine with filter_by_status; a mismatched
	// filter yields an empty page, exactly like the Rails ANDed scopes.
	effectiveStatus, statusFiltered := statusFilter, filtered
	empty := false
	if hasScope {
		if filtered && statusFilter != scopeStatus {
			empty = true
		} else {
			effectiveStatus, statusFiltered = scopeStatus, true
		}
	}

	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.NotFound(w, r)
			return
		}
		page = n
	}
	offset := int64(page-1) * adminArticlesPerPage

	term := r.URL.Query().Get("q")
	like := likeTerm(term)

	var rows []query.Article
	var total int64
	var err error
	switch {
	case empty:
		rows, total = []query.Article{}, 0
	case term != "" && statusFiltered:
		total, err = s.Q.CountSearchAdminArticlesByStatus(ctx, query.CountSearchAdminArticlesByStatusParams{
			Status: effectiveStatus, LIKE: like, LIKE_2: like, LIKE_3: like, LIKE_4: like,
		})
		if err == nil {
			rows, err = s.Q.SearchAdminArticlesByStatus(ctx, query.SearchAdminArticlesByStatusParams{
				Status: effectiveStatus, LIKE: like, LIKE_2: like, LIKE_3: like, LIKE_4: like,
				Limit: adminArticlesPerPage, Offset: offset,
			})
		}
	case term != "":
		total, err = s.Q.CountSearchAdminArticles(ctx, query.CountSearchAdminArticlesParams{
			LIKE: like, LIKE_2: like, LIKE_3: like, LIKE_4: like,
		})
		if err == nil {
			rows, err = s.Q.SearchAdminArticles(ctx, query.SearchAdminArticlesParams{
				LIKE: like, LIKE_2: like, LIKE_3: like, LIKE_4: like,
				Limit: adminArticlesPerPage, Offset: offset,
			})
		}
	case statusFiltered:
		total, err = s.Q.CountAdminArticlesByStatus(ctx, effectiveStatus)
		if err == nil {
			rows, err = s.Q.ListAdminArticlesByStatus(ctx, query.ListAdminArticlesByStatusParams{
				Status: effectiveStatus, Limit: adminArticlesPerPage, Offset: offset,
			})
		}
	default:
		total, err = s.Q.CountAdminArticles(ctx)
		if err == nil {
			rows, err = s.Q.ListAdminArticles(ctx, query.ListAdminArticlesParams{
				Limit: adminArticlesPerPage, Offset: offset,
			})
		}
	}
	if err != nil {
		s.Log.Error("list articles", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	listRows, err := s.buildArticleRows(ctx, rows)
	if err != nil {
		s.Log.Error("list articles: preload", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	st, err := s.Settings().Get(ctx)
	if err != nil {
		s.Log.Error("load settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if statusName == "" {
		statusName = "all"
	}
	s.render(w, http.StatusOK, "admin_articles_index", adminArticlesIndexData{
		Flash:    PopFlash(r, w),
		Rows:     listRows,
		Q:        term,
		Status:   statusName,
		Path:     path,
		Page:     page,
		Pages:    int((total + adminArticlesPerPage - 1) / adminArticlesPerPage),
		TimeZone: st.TimeZone,
	})
}

// parseStatusFilter mirrors filter_by_status: only the five known statuses
// filter; anything else (including "all") lists everything.
func parseStatusFilter(name string) (int64, bool) {
	switch name {
	case "publish":
		return int64(domain.StatusPublish), true
	case "schedule":
		return int64(domain.StatusSchedule), true
	case "shared":
		return int64(domain.StatusShared), true
	case "draft":
		return int64(domain.StatusDraft), true
	case "trash":
		return int64(domain.StatusTrash), true
	}
	return 0, false
}

// likeTerm wraps a search term for Article.search_content: %, _ and the
// escape character itself are escaped (sanitize_sql_like), then the term is
// wrapped in %...%. Blank terms stay empty (no search).
func likeTerm(term string) string {
	if term == "" {
		return ""
	}
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(term) + "%"
}

// buildArticleRows preloads tags, comment counts and social posts for one
// list page (apply_model_includes / load_comment_counts).
func (s *Server) buildArticleRows(ctx context.Context, articles []query.Article) ([]adminArticleRow, error) {
	rows := make([]adminArticleRow, 0, len(articles))
	if len(articles) == 0 {
		return rows, nil
	}
	ids := make([]int64, len(articles))
	nullIDs := make([]sql.NullInt64, len(articles))
	for i, a := range articles {
		ids[i] = a.ID
		nullIDs[i] = sql.NullInt64{Int64: a.ID, Valid: true}
	}

	tagRows, err := s.Q.ListArticleTagNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	tagsByArticle := map[int64][]string{}
	for _, tr := range tagRows {
		tagsByArticle[tr.ArticleID] = append(tagsByArticle[tr.ArticleID], tr.Name)
	}

	countRows, err := s.Q.CountCommentsForArticles(ctx, nullIDs)
	if err != nil {
		return nil, err
	}
	commentsByArticle := map[int64]int64{}
	for _, cr := range countRows {
		if cr.CommentableID.Valid {
			commentsByArticle[cr.CommentableID.Int64] = cr.CommentCount
		}
	}

	postRows, err := s.Q.ListSocialPostsForArticles(ctx, ids)
	if err != nil {
		return nil, err
	}
	postsByArticle := map[int64][]string{}
	for _, post := range postRows {
		if post.Url == "" || post.Platform == "xiaohongshu" || !isCrosspostPlatform(post.Platform) {
			continue
		}
		postsByArticle[post.ArticleID] = append(postsByArticle[post.ArticleID], post.Platform)
	}

	for _, a := range articles {
		rows = append(rows, adminArticleRow{
			Article:        a,
			DisplayTitle:   articleDisplayTitle(a),
			Tags:           strings.Join(tagsByArticle[a.ID], ", "),
			CommentCount:   commentsByArticle[a.ID],
			FetchPlatforms: postsByArticle[a.ID],
		})
	}
	return rows, nil
}

// articleDisplayTitle mirrors the index title fallback chain:
// title.presence || truncate(plain_text, 20) || slug.
func articleDisplayTitle(a query.Article) string {
	if a.Title.Valid && !domain.IsBlank(a.Title.String) {
		return a.Title.String
	}
	if plain := domain.PlainText(a.ContentHtml.String); !domain.IsBlank(plain) {
		return truncateRunes(plain, 20)
	}
	return a.Slug.String
}

func isCrosspostPlatform(platform string) bool {
	for _, p := range articlesvc.CrosspostPlatforms {
		if p == platform {
			return true
		}
	}
	return false
}

// adminArticleFormData feeds admin_articles_new.html and
// admin_articles_edit.html.
type adminArticleFormData struct {
	Flash              templates.Flash
	FormAction         string // /admin/posts or /admin/posts/{id}
	Errors             []string
	Form               adminArticleForm
	NewsletterEnabled  bool
	MastodonEnabled    bool
	TwitterEnabled     bool
	BlueskyEnabled     bool
	XiaohongshuEnabled bool
	TimeZone           string
}

// adminArticleForm carries every form field value so a validation failure
// re-renders with the submitted input, like the Rails form builder bound to
// the invalid record.
type adminArticleForm struct {
	Title            string
	Slug             string
	Status           string
	ContentType      string
	Content          string // rich_text body
	HTMLContent      string // html body
	Description      string
	MetaTitle        string
	MetaImage        string
	MetaDescription  string
	SourceURL        string
	SourceAuthor     string
	SourceContent    string
	TagList          string
	Comment          bool
	SendNewsletter   bool
	Crosspost        map[string]bool
	SocialURLs       map[string]string
	CreatedAtLocal   string
	ScheduledAtLocal string
}

// adminArticlesNew renders GET /admin/posts/new (Article.new(comment: true)).
func (s *Server) adminArticlesNew(w http.ResponseWriter, r *http.Request) {
	data, ok := s.newArticleFormData(w, r)
	if !ok {
		return
	}
	s.render(w, http.StatusOK, "admin_articles_new", data)
}

// newArticleFormData builds the form data for new/edit with set_form_options.
func (s *Server) newArticleFormData(w http.ResponseWriter, r *http.Request) (adminArticleFormData, bool) {
	ctx := r.Context()
	st, err := s.Settings().Get(ctx)
	if err != nil {
		s.Log.Error("load settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return adminArticleFormData{}, false
	}
	data := adminArticleFormData{
		Flash:      PopFlash(r, w),
		TimeZone:   st.TimeZone,
		FormAction: "/admin/posts",
		Form: adminArticleForm{
			Status:      "draft",
			ContentType: string(domain.ContentTypeRichText),
			Comment:     true,
			Crosspost:   map[string]bool{},
			SocialURLs:  map[string]string{},
		},
	}
	data.NewsletterEnabled = s.formNewsletterEnabled(ctx)
	data.MastodonEnabled = s.formCrosspostEnabled(ctx, "mastodon")
	data.TwitterEnabled = s.formCrosspostEnabled(ctx, "twitter")
	data.BlueskyEnabled = s.formCrosspostEnabled(ctx, "bluesky")
	data.XiaohongshuEnabled = s.formCrosspostEnabled(ctx, "xiaohongshu")
	return data, true
}

// formCrosspostEnabled is set_form_options' per-platform enabled check; a
// lookup failure degrades to disabled, like a missing Crosspost row.
func (s *Server) formCrosspostEnabled(ctx context.Context, platform string) bool {
	enabled, err := articlesvc.CrosspostEnabled(ctx, s.Q, platform)
	return err == nil && enabled
}

func (s *Server) formNewsletterEnabled(ctx context.Context) bool {
	enabled, err := articlesvc.NewsletterEnabled(ctx, s.Q)
	return err == nil && enabled
}

// adminArticlesCreate handles POST /admin/posts, mirroring
// Admin::ArticlesController#create.
func (s *Server) adminArticlesCreate(w http.ResponseWriter, r *http.Request) {
	data, ok := s.newArticleFormData(w, r)
	if !ok {
		return
	}
	params, err := s.parseArticleForm(r, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	article, validationErrs, err := articlesvc.Save(r.Context(), s.DB, nil, params)
	if err != nil {
		s.Log.Error("create article", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(validationErrs) > 0 {
		s.logArticleActivity(r.Context(), "article", "failed", 2,
			fmt.Sprintf("title=%s slug=%s errors=%s", activityQuote(params.Title), activityQuote(params.Slug), activityQuote(strings.Join(validationErrs, ", "))))
		data.Errors = validationErrs
		data.Form = articleFormFromParams(params, data.TimeZone)
		s.render(w, http.StatusUnprocessableEntity, "admin_articles_new", data)
		return
	}
	s.logArticleActivity(r.Context(), "article", "created", 0,
		fmt.Sprintf("title=%s slug=%s", activityQuote(article.Title.String), activityQuote(article.Slug.String)))
	SetFlash(w, templates.Flash{Notice: "Article was successfully created."})
	if r.PostFormValue("create_and_add_another") != "" {
		http.Redirect(w, r, "/admin/posts/new", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/posts", http.StatusFound)
}

// adminArticlesEdit renders GET /admin/posts/{id}/edit.
func (s *Server) adminArticlesEdit(w http.ResponseWriter, r *http.Request) {
	article, err := s.Q.GetAdminArticleBySlug(r.Context(), nullSlug(chi.URLParam(r, "id")))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get article", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data, ok := s.newArticleFormData(w, r)
	if !ok {
		return
	}
	data.FormAction = "/admin/posts/" + article.Slug.String
	form, err := s.articleFormFromArticle(r.Context(), article, data.TimeZone)
	if err != nil {
		s.Log.Error("load article form", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.Form = form
	s.render(w, http.StatusOK, "admin_articles_edit", data)
}

// adminArticlesUpdate handles POST /admin/posts/{id} (Rails PATCH
// /admin/posts/:id), mirroring Admin::ArticlesController#update.
func (s *Server) adminArticlesUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	existing, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug(chi.URLParam(r, "id")))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get article", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data, ok := s.newArticleFormData(w, r)
	if !ok {
		return
	}
	data.FormAction = "/admin/posts/" + chi.URLParam(r, "id")

	params, err := s.parseArticleForm(r, &existing)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	article, validationErrs, err := articlesvc.Save(ctx, s.DB, &existing, params)
	if err != nil {
		s.Log.Error("update article", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(validationErrs) > 0 {
		s.logArticleActivity(ctx, "article", "failed", 2,
			fmt.Sprintf("title=%s slug=%s errors=%s", activityQuote(params.Title), activityQuote(params.Slug), activityQuote(strings.Join(validationErrs, ", "))))
		data.Errors = validationErrs
		data.Form = articleFormFromParams(params, data.TimeZone)
		s.render(w, http.StatusUnprocessableEntity, "admin_articles_edit", data)
		return
	}
	s.logArticleActivity(ctx, "article", "updated", 0,
		fmt.Sprintf("title=%s slug=%s", activityQuote(article.Title.String), activityQuote(article.Slug.String)))
	SetFlash(w, templates.Flash{Notice: "Article was successfully updated."})
	http.Redirect(w, r, "/admin/posts", http.StatusFound)
}

// adminArticlesDestroy handles POST /admin/posts/{id}/destroy (Rails DELETE),
// mirroring the two-stage Admin::ArticlesController#destroy: non-trash moves
// to trash, trash is really deleted.
func (s *Server) adminArticlesDestroy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug(chi.URLParam(r, "id")))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get article", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	title := article.Title.String
	if article.Status != int64(domain.StatusTrash) {
		if _, err := articlesvc.TransitionStatus(ctx, s.DB, article.ID, domain.StatusTrash, time.Now()); err != nil {
			s.Log.Error("trash article", "error", err)
			s.logArticleActivity(ctx, "article", "failed", 2,
				fmt.Sprintf("title=%s slug=%s errors=%s", activityQuote(title), activityQuote(article.Slug.String), activityQuote(err.Error())))
			SetFlash(w, templates.Flash{Alert: "Failed to move article to trash."})
			http.Redirect(w, r, "/admin/posts", http.StatusSeeOther)
			return
		}
		s.logArticleActivity(ctx, "article", "trashed", 0,
			fmt.Sprintf("title=%s slug=%s", activityQuote(title), activityQuote(article.Slug.String)))
		SetFlash(w, templates.Flash{Notice: "Article was successfully moved to trash."})
		http.Redirect(w, r, "/admin/posts", http.StatusSeeOther)
		return
	}
	if err := articlesvc.Destroy(ctx, s.DB, article.ID); err != nil {
		s.Log.Error("delete article", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logArticleActivity(ctx, "article", "deleted", 0,
		fmt.Sprintf("title=%s slug=%s", activityQuote(title), activityQuote(article.Slug.String)))
	SetFlash(w, templates.Flash{Notice: "Article was successfully deleted."})
	http.Redirect(w, r, "/admin/posts", http.StatusSeeOther)
}

// adminArticlesPublish handles POST /admin/posts/{id}/publish (Rails member
// PATCH :publish), mirroring Admin::ArticlesController#publish.
func (s *Server) adminArticlesPublish(w http.ResponseWriter, r *http.Request) {
	s.transitionArticleStatus(w, r, domain.StatusPublish, "published", "Article was successfully published.", "Failed to publish article.")
}

// adminArticlesUnpublish handles POST /admin/posts/{id}/unpublish (Rails
// member PATCH :unpublish = back to draft).
func (s *Server) adminArticlesUnpublish(w http.ResponseWriter, r *http.Request) {
	s.transitionArticleStatus(w, r, domain.StatusDraft, "unpublished", "Article was successfully unpublished.", "Failed to unpublish article.")
}

// transitionArticleStatus is the shared find-by-slug + status update behind
// the publish/unpublish member actions.
func (s *Server) transitionArticleStatus(w http.ResponseWriter, r *http.Request, target domain.Status, action, notice, alert string) {
	ctx := r.Context()
	article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug(chi.URLParam(r, "id")))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get article", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := articlesvc.TransitionStatus(ctx, s.DB, article.ID, target, time.Now()); err != nil {
		s.Log.Error("transition article", "target", target.String(), "error", err)
		s.logArticleActivity(ctx, "article", "failed", 2,
			fmt.Sprintf("title=%s slug=%s errors=%s", activityQuote(article.Title.String), activityQuote(article.Slug.String), activityQuote(err.Error())))
		SetFlash(w, templates.Flash{Alert: alert})
		http.Redirect(w, r, "/admin/posts", http.StatusFound)
		return
	}
	s.logArticleActivity(ctx, "article", action, 0,
		fmt.Sprintf("title=%s slug=%s", activityQuote(article.Title.String), activityQuote(article.Slug.String)))
	SetFlash(w, templates.Flash{Notice: notice})
	http.Redirect(w, r, "/admin/posts", http.StatusFound)
}

// adminArticlesBatchDestroy handles POST /admin/posts/batch_destroy,
// mirroring the two-stage override in Admin::ArticlesController#batch_destroy
// (non-trash -> trash, trash -> real delete, both counted).
func (s *Server) adminArticlesBatchDestroy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ids := batchIDs(r)
	if len(ids) == 0 {
		SetFlash(w, templates.Flash{Alert: "请至少选择一个文章。"})
		http.Redirect(w, r, "/admin/posts", http.StatusFound)
		return
	}
	ctx := r.Context()
	var trashed, deleted int
	var errs []string
	for _, slug := range ids {
		article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug(slug))
		if err != nil {
			continue // find_by miss is skipped like Rails
		}
		label := batchArticleLabel(article)
		if article.Status != int64(domain.StatusTrash) {
			if _, err := articlesvc.TransitionStatus(ctx, s.DB, article.ID, domain.StatusTrash, time.Now()); err != nil {
				errs = append(errs, label+": "+err.Error())
				continue
			}
			trashed++
		} else {
			if err := articlesvc.Destroy(ctx, s.DB, article.ID); err != nil {
				errs = append(errs, label+": "+err.Error())
				continue
			}
			deleted++
		}
	}

	var messages []string
	if trashed > 0 {
		messages = append(messages, fmt.Sprintf("成功将 %d 篇文章移动到垃圾箱。", trashed))
	}
	if deleted > 0 {
		messages = append(messages, fmt.Sprintf("成功删除 %d 篇文章。", deleted))
	}
	if len(errs) > 0 {
		s.logArticleActivity(ctx, "article", "deleted", 1,
			fmt.Sprintf("trashed_count=%d deleted_count=%d error_count=%d errors=%s", trashed, deleted, len(errs), activityQuote(strings.Join(errs, "; "))))
		SetFlash(w, templates.Flash{Alert: strings.Join(messages, " ") + "错误: " + strings.Join(errs, "; ")})
	} else {
		s.logArticleActivity(ctx, "article", "deleted", 0,
			fmt.Sprintf("trashed_count=%d deleted_count=%d", trashed, deleted))
		SetFlash(w, templates.Flash{Notice: strings.Join(messages, " ")})
	}
	http.Redirect(w, r, "/admin/posts", http.StatusFound)
}

// adminArticlesBatchPublish handles POST /admin/posts/batch_publish
// (Admin::BaseController process_batch_action(action: :publish)).
func (s *Server) adminArticlesBatchPublish(w http.ResponseWriter, r *http.Request) {
	s.processBatchTransition(w, r, domain.StatusPublish, "published")
}

// adminArticlesBatchUnpublish handles POST /admin/posts/batch_unpublish
// (process_batch_action(action: :unpublish)).
func (s *Server) adminArticlesBatchUnpublish(w http.ResponseWriter, r *http.Request) {
	s.processBatchTransition(w, r, domain.StatusDraft, "unpublished")
}

// processBatchTransition mirrors Admin::BaseController#process_batch_action:
// missing slugs are skipped, the notice reports the count, and the first
// unexpected failure redirects with an alert.
func (s *Server) processBatchTransition(w http.ResponseWriter, r *http.Request, target domain.Status, pastTense string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	count := 0
	for _, slug := range batchIDs(r) {
		article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug(slug))
		if err != nil {
			continue
		}
		if _, err := articlesvc.TransitionStatus(ctx, s.DB, article.ID, target, time.Now()); err != nil {
			SetFlash(w, templates.Flash{Alert: fmt.Sprintf("Error processing %s for articles: %s", batchActionName(target), err)})
			http.Redirect(w, r, "/admin/posts", http.StatusFound)
			return
		}
		count++
	}
	SetFlash(w, templates.Flash{Notice: fmt.Sprintf("Successfully %s %d article(s).", pastTense, count)})
	http.Redirect(w, r, "/admin/posts", http.StatusFound)
}

func batchActionName(target domain.Status) string {
	if target == domain.StatusPublish {
		return "publish"
	}
	return "unpublish"
}

// adminArticlesBatchAddTags handles POST /admin/posts/batch_add_tags,
// mirroring Admin::ArticlesController#batch_add_tags: tags are created once
// and appended (never replacing existing ones).
func (s *Server) adminArticlesBatchAddTags(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	ids := batchIDs(r)
	if len(ids) == 0 {
		SetFlash(w, templates.Flash{Alert: "请至少选择一个文章。"})
		http.Redirect(w, r, "/admin/posts", http.StatusFound)
		return
	}
	tagNames := r.PostFormValue("tag_names")
	if domain.IsBlank(tagNames) {
		SetFlash(w, templates.Flash{Alert: "请输入至少一个标签。"})
		http.Redirect(w, r, "/admin/posts", http.StatusFound)
		return
	}
	tagIDs, err := tagsvc.FindOrCreateByNames(ctx, s.Q, articlesvc.SplitTagList(tagNames))
	if err != nil {
		s.Log.Error("batch add tags", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(tagIDs) == 0 {
		SetFlash(w, templates.Flash{Alert: "无法创建标签，请检查标签名称。"})
		http.Redirect(w, r, "/admin/posts", http.StatusFound)
		return
	}

	now := time.Now().Unix()
	count := 0
	var errs []string
	for _, slug := range ids {
		article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug(slug))
		if err != nil {
			continue
		}
		failed := false
		for _, tagID := range tagIDs {
			// find_or_create_by per pair: INSERT OR IGNORE keeps existing tags.
			if err := s.Q.InsertArticleTag(ctx, query.InsertArticleTagParams{
				ArticleID: article.ID, TagID: tagID, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				errs = append(errs, batchArticleLabel(article)+": "+err.Error())
				failed = true
				break
			}
		}
		if !failed {
			count++
		}
	}
	if len(errs) > 0 {
		s.logArticleActivity(ctx, "article", "updated", 1,
			fmt.Sprintf("count=%d error_count=%d tags=%s errors=%s", count, len(errs), activityQuote(tagNames), activityQuote(strings.Join(errs, "; "))))
		SetFlash(w, templates.Flash{Alert: fmt.Sprintf("成功添加标签到 %d 篇文章。错误: %s", count, strings.Join(errs, "; "))})
	} else {
		s.logArticleActivity(ctx, "article", "updated", 0,
			fmt.Sprintf("count=%d tags=%s", count, activityQuote(tagNames)))
		SetFlash(w, templates.Flash{Notice: fmt.Sprintf("成功添加标签到 %d 篇文章。", count)})
	}
	http.Redirect(w, r, "/admin/posts", http.StatusFound)
}

// adminArticlesBatchCrosspost handles POST /admin/posts/batch_crosspost,
// mirroring Admin::ArticlesController#batch_crosspost: only published
// articles qualify, and one crosspost job is enqueued per enabled platform.
func (s *Server) adminArticlesBatchCrosspost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	ids := batchIDs(r)
	if len(ids) == 0 {
		SetFlash(w, templates.Flash{Alert: "请至少选择一个文章。"})
		http.Redirect(w, r, "/admin/posts", http.StatusFound)
		return
	}
	platforms := r.PostForm["platforms"]
	if len(platforms) == 0 {
		platforms = r.PostForm["platforms[]"]
	}
	if len(platforms) == 0 {
		SetFlash(w, templates.Flash{Alert: "请至少选择一个平台。"})
		http.Redirect(w, r, "/admin/posts", http.StatusFound)
		return
	}

	var enabledPlatforms []string
	for _, platform := range platforms {
		enabled, err := articlesvc.CrosspostEnabled(ctx, s.Q, platform)
		if err != nil {
			s.Log.Error("batch crosspost: platform lookup", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if enabled {
			enabledPlatforms = append(enabledPlatforms, platform)
		}
	}

	now := time.Now().UTC()
	count := 0
	var errs []string
	for _, slug := range ids {
		article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug(slug))
		if err != nil {
			continue
		}
		if article.Status != int64(domain.StatusPublish) {
			errs = append(errs, batchArticleLabel(article)+": 文章未发布，无法进行跨平台发布")
			continue
		}
		queued := false
		for _, platform := range enabledPlatforms {
			if _, err := s.Enqueuer().Enqueue(ctx, jobs.KindCrosspost, articlesvc.CrosspostPayload{
				ArticleID: article.ID, Platform: platform, RequestedAt: now.Unix(),
			}, now); err != nil {
				errs = append(errs, batchArticleLabel(article)+": "+err.Error())
				queued = false
				break
			}
			queued = true
		}
		if queued {
			count++
		}
	}
	if len(errs) > 0 {
		s.logArticleActivity(ctx, "crosspost", "queued", 1,
			fmt.Sprintf("count=%d platforms=%s error_count=%d errors=%s", count, platformsListValue(platforms), len(errs), activityQuote(strings.Join(errs, "; "))))
		SetFlash(w, templates.Flash{Alert: fmt.Sprintf("成功提交 %d 篇文章进行跨平台发布。错误: %s", count, strings.Join(errs, "; "))})
	} else {
		s.logArticleActivity(ctx, "crosspost", "queued", 0,
			fmt.Sprintf("count=%d platforms=%s", count, platformsListValue(platforms)))
		SetFlash(w, templates.Flash{Notice: fmt.Sprintf("成功提交 %d 篇文章进行跨平台发布。", count)})
	}
	http.Redirect(w, r, "/admin/posts", http.StatusFound)
}

// adminArticlesBatchNewsletter handles POST /admin/posts/batch_newsletter,
// mirroring Admin::ArticlesController#batch_newsletter: only published
// articles qualify, and the newsletter must be enabled and configured.
func (s *Server) adminArticlesBatchNewsletter(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	ids := batchIDs(r)
	if len(ids) == 0 {
		SetFlash(w, templates.Flash{Alert: "请至少选择一个文章。"})
		http.Redirect(w, r, "/admin/posts", http.StatusFound)
		return
	}
	ready, err := articlesvc.NewsletterReady(ctx, s.Q)
	if err != nil {
		s.Log.Error("batch newsletter: settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()
	count := 0
	var errs []string
	for _, slug := range ids {
		article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug(slug))
		if err != nil {
			continue
		}
		if article.Status != int64(domain.StatusPublish) {
			errs = append(errs, batchArticleLabel(article)+": 文章未发布，无法发送邮件")
			continue
		}
		if !ready {
			errs = append(errs, batchArticleLabel(article)+": Newsletter未配置或未启用")
			continue
		}
		if _, err := s.Enqueuer().Enqueue(ctx, jobs.KindSendNewsletter, articlesvc.NewsletterPayload{ArticleID: article.ID}, now); err != nil {
			errs = append(errs, batchArticleLabel(article)+": "+err.Error())
			continue
		}
		count++
	}
	if len(errs) > 0 {
		s.logArticleActivity(ctx, "newsletter", "queued", 1,
			fmt.Sprintf("count=%d error_count=%d errors=%s", count, len(errs), activityQuote(strings.Join(errs, "; "))))
		SetFlash(w, templates.Flash{Alert: fmt.Sprintf("成功提交 %d 篇文章发送邮件。错误: %s", count, strings.Join(errs, "; "))})
	} else {
		s.logArticleActivity(ctx, "newsletter", "queued", 0,
			fmt.Sprintf("count=%d", count))
		SetFlash(w, templates.Flash{Notice: fmt.Sprintf("成功提交 %d 篇文章发送邮件。", count)})
	}
	http.Redirect(w, r, "/admin/posts", http.StatusFound)
}

// adminArticlesFetchComments handles POST /admin/posts/{id}/fetch_comments.
// Rails fetches synchronously from each platform service; the Go rewrite
// enqueues one fetch_social_comments job per social post instead (the
// platform fetchers run in the job worker), narrowed to this article.
func (s *Server) adminArticlesFetchComments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	article, err := s.Q.GetAdminArticleBySlug(ctx, nullSlug(chi.URLParam(r, "id")))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get article", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	platform := r.PostFormValue("platform")
	var posts []query.SocialMediaPost
	if platform != "" {
		posts, err = s.Q.ListFetchableSocialPostsByPlatform(ctx, query.ListFetchableSocialPostsByPlatformParams{
			ArticleID: article.ID, Platform: platform,
		})
	} else {
		posts, err = s.Q.ListFetchableSocialPosts(ctx, article.ID)
	}
	if err != nil {
		s.Log.Error("fetch comments: list posts", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(posts) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false,
			"message": "No social media posts found for this article",
		})
		return
	}

	now := time.Now().UTC()
	enqueued := 0
	var results []map[string]any
	for _, post := range posts {
		switch post.Platform {
		case "mastodon", "bluesky", "twitter":
		default:
			continue // no fetcher for this platform, like the Rails case/else
		}
		if _, err := s.Enqueuer().Enqueue(ctx, jobs.KindFetchSocialComments, articlesvc.FetchCommentsPayload{
			ArticleID: article.ID, Platform: post.Platform,
		}, now); err != nil {
			s.Log.Error("fetch comments: enqueue", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		enqueued++
		results = append(results, map[string]any{"platform": post.Platform, "queued": 1})
	}
	s.logArticleActivity(ctx, "fetch_comments", "fetched", 0,
		fmt.Sprintf("title=%s slug=%s count=%d", activityQuote(article.Title.String), activityQuote(article.Slug.String), enqueued))
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": fmt.Sprintf("Enqueued comment fetch for %d social post(s)", enqueued),
		"results": results,
	})
}

// parseArticleForm reads the article form into SaveParams, mirroring
// article_params. Rails' check_box submits a hidden "0" ahead of the "1"
// checkbox, so checkbox values take the LAST submitted value; a key absent
// from the submission (disabled control) falls back to the stored selection,
// like the missing attr_writer call.
func (s *Server) parseArticleForm(r *http.Request, existing *query.Article) (articlesvc.SaveParams, error) {
	if err := r.ParseForm(); err != nil {
		return articlesvc.SaveParams{}, err
	}
	status, ok := articlesvc.ParseStatus(r.PostFormValue("status"))
	if !ok {
		return articlesvc.SaveParams{}, fmt.Errorf("unknown status %q", r.PostFormValue("status"))
	}

	tz := "UTC"
	if st, err := s.Settings().Get(r.Context()); err == nil {
		tz = st.TimeZone
	}

	contentType := r.PostFormValue("content_type")
	raw := r.PostFormValue("content")
	if contentType == string(domain.ContentTypeHTML) {
		raw = r.PostFormValue("html_content")
	} else {
		contentType = string(domain.ContentTypeRichText)
	}

	snapshot := map[string]bool{}
	if existing != nil {
		for _, platform := range articlesvc.ParseScheduledPlatforms(existing.ScheduledCrosspostPlatforms) {
			snapshot[platform] = true
		}
	}
	crosspost := map[string]bool{}
	socialURLs := map[string]string{}
	for _, platform := range articlesvc.CrosspostPlatforms {
		if value, present := formCheckbox(r, "crosspost_"+platform); present {
			crosspost[platform] = value
		} else {
			crosspost[platform] = snapshot[platform]
		}
		if values, ok := r.PostForm["social_url_"+platform]; ok {
			socialURLs[platform] = values[len(values)-1]
		}
	}

	sendNewsletter, present := formCheckbox(r, "send_newsletter")
	if !present && existing != nil {
		sendNewsletter = existing.ScheduledSendNewsletter == 1
	}
	comment, _ := formCheckbox(r, "comment")

	params := articlesvc.SaveParams{
		Title:           r.PostFormValue("title"),
		Slug:            r.PostFormValue("slug"),
		ContentType:     contentType,
		ContentHTML:     raw,
		Description:     r.PostFormValue("description"),
		MetaTitle:       r.PostFormValue("meta_title"),
		MetaImage:       r.PostFormValue("meta_image"),
		MetaDescription: r.PostFormValue("meta_description"),
		SourceAuthor:    r.PostFormValue("source_author"),
		SourceURL:       r.PostFormValue("source_url"),
		SourceContent:   r.PostFormValue("source_content"),
		Status:          status,
		Comment:         comment,
		SendNewsletter:  sendNewsletter,
		Crosspost:       crosspost,
		TagList:         r.PostFormValue("tag_list"),
		SocialURLs:      socialURLs,
		Now:             time.Now().UTC(),
	}
	if t := parseFormTime(r.PostFormValue("created_at"), tz); t != nil {
		params.CreatedAt = *t
	}
	params.ScheduledAt = parseFormTime(r.PostFormValue("scheduled_at"), tz)
	return params, nil
}

// formCheckbox resolves a Rails check_box field (hidden "0" + checkbox "1"):
// the last submitted value wins. present is false when the key was not
// submitted at all (disabled control).
func formCheckbox(r *http.Request, name string) (value, present bool) {
	values, ok := r.PostForm[name]
	if !ok || len(values) == 0 {
		return false, false
	}
	v := values[len(values)-1]
	return v == "1" || v == "true" || v == "on", true
}

// parseFormTime parses a datetime-local value in the settings time zone
// (plan section 4.12). Blank or unparseable values yield nil, matching the
// ActiveModel datetime cast of an invalid string.
func parseFormTime(value, tzName string) *time.Time {
	if domain.IsBlank(value) {
		return nil
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		loc = time.UTC
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(value), loc); err == nil {
			return &t
		}
	}
	return nil
}

// articleFormFromArticle builds the edit form values from the stored record:
// crosspost/newsletter checkboxes fall back to the schedule snapshot
// (crosspost_selected? without an override).
func (s *Server) articleFormFromArticle(ctx context.Context, article query.Article, tzName string) (adminArticleForm, error) {
	tagRows, err := s.Q.ListArticleTagNames(ctx, []int64{article.ID})
	if err != nil {
		return adminArticleForm{}, err
	}
	var names []string
	for _, tr := range tagRows {
		names = append(names, tr.Name)
	}
	posts, err := s.Q.ListSocialPostsByArticleID(ctx, article.ID)
	if err != nil {
		return adminArticleForm{}, err
	}
	socialURLs := map[string]string{}
	for _, post := range posts {
		socialURLs[post.Platform] = post.Url
	}
	crosspost := map[string]bool{}
	for _, platform := range articlesvc.ParseScheduledPlatforms(article.ScheduledCrosspostPlatforms) {
		crosspost[platform] = true
	}
	content := article.ContentHtml.String
	return adminArticleForm{
		Title:            article.Title.String,
		Slug:             article.Slug.String,
		Status:           domain.Status(article.Status).String(),
		ContentType:      article.ContentType,
		Content:          content,
		HTMLContent:      content,
		Description:      article.Description.String,
		MetaTitle:        article.MetaTitle.String,
		MetaImage:        article.MetaImage.String,
		MetaDescription:  article.MetaDescription.String,
		SourceURL:        article.SourceUrl.String,
		SourceAuthor:     article.SourceAuthor.String,
		SourceContent:    article.SourceContent.String,
		TagList:          strings.Join(names, ", "),
		Comment:          article.Comment == 1,
		SendNewsletter:   article.ScheduledSendNewsletter == 1,
		Crosspost:        crosspost,
		SocialURLs:       socialURLs,
		CreatedAtLocal:   formatFormTime(article.CreatedAt, tzName),
		ScheduledAtLocal: formatFormTime(article.ScheduledAt.Int64, tzName),
	}, nil
}

// articleFormFromParams rebuilds the form values from a failed submission so
// the 422 re-render keeps the user's input (render :new/:edit with the
// invalid record).
func articleFormFromParams(p articlesvc.SaveParams, tzName string) adminArticleForm {
	form := adminArticleForm{
		Title:           p.Title,
		Slug:            p.Slug,
		Status:          p.Status.String(),
		ContentType:     p.ContentType,
		Description:     p.Description,
		MetaTitle:       p.MetaTitle,
		MetaImage:       p.MetaImage,
		MetaDescription: p.MetaDescription,
		SourceURL:       p.SourceURL,
		SourceAuthor:    p.SourceAuthor,
		SourceContent:   p.SourceContent,
		TagList:         p.TagList,
		Comment:         p.Comment,
		SendNewsletter:  p.SendNewsletter,
		Crosspost:       p.Crosspost,
		SocialURLs:      p.SocialURLs,
	}
	if p.ContentType == string(domain.ContentTypeHTML) {
		form.HTMLContent = p.ContentHTML
	} else {
		form.Content = p.ContentHTML
	}
	if !p.CreatedAt.IsZero() {
		form.CreatedAtLocal = formatFormTime(p.CreatedAt.Unix(), tzName)
	}
	if p.ScheduledAt != nil {
		form.ScheduledAtLocal = formatFormTime(p.ScheduledAt.Unix(), tzName)
	}
	return form
}

// formatFormTime renders a unix timestamp for a datetime-local input in the
// settings time zone.
func formatFormTime(unix int64, tzName string) string {
	if unix == 0 {
		return ""
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		loc = time.UTC
	}
	return time.Unix(unix, 0).In(loc).Format("2006-01-02T15:04")
}

// batchIDs reads the batch selection (Rails params[:ids]; the templates
// submit ids, the JS-less form variant ids[]).
func batchIDs(r *http.Request) []string {
	ids := r.PostForm["ids"]
	if len(ids) == 0 {
		ids = r.PostForm["ids[]"]
	}
	return ids
}

// batchArticleLabel is the error prefix of the Rails batch actions:
// title || slug || 'Unknown'.
func batchArticleLabel(article query.Article) string {
	if article.Title.Valid && article.Title.String != "" {
		return article.Title.String
	}
	if article.Slug.Valid && article.Slug.String != "" {
		return article.Slug.String
	}
	return "Unknown"
}

// nullSlug adapts the route slug for the nullable column lookup.
func nullSlug(slug string) sql.NullString {
	return sql.NullString{String: slug, Valid: true}
}

// platformsListValue mirrors ActivityLog.format_value for an Array of
// platform names.
func platformsListValue(platforms []string) string {
	quoted := make([]string, 0, len(platforms))
	for _, p := range platforms {
		quoted = append(quoted, activityQuote(p))
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// writeJSON renders a JSON response (the Rails fetch_comments JSON format).
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// logArticleActivity mirrors the ActivityLog.log! calls in
// Admin::ArticlesController (action/target/level/description). Like the Rails
// original, it never breaks the main flow. It writes raw SQL because the
// activity-logs feature (and its queries) belongs to a later task; swap for
// its shared helper once that lands.
func (s *Server) logArticleActivity(ctx context.Context, target, action string, level int64, description string) {
	now := time.Now().Unix()
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO activity_logs (level, action, target, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		level, action, target, description, now, now)
	if err != nil {
		s.Log.Warn("activity log", "error", err)
	}
}
