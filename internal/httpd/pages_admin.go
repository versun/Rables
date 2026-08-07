package httpd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	"rables/internal/templates"
)

// adminPagesPerPage mirrors the fetch_articles per_page of the Rails index.
const adminPagesPerPage = 100

// RegisterPageAdminRoutes mounts the admin page CRUD behind RequireAuth,
// mirroring Rails' namespace :admin resources :pages (param: :slug via
// Page#to_param) with collection post :batch_destroy/:batch_publish/
// :batch_unpublish. HTML forms cannot PATCH/DELETE, so update maps Rails
// PATCH /admin/pages/:id to POST /admin/pages/{slug} and destroy maps DELETE
// to POST /admin/pages/{slug}/destroy. Wired into NewRouter by the
// integrator; no ordering constraints beyond chi's static-over-wildcard
// matching (batch_* and new must resolve before {slug}, which chi guarantees).
func RegisterPageAdminRoutes(r chi.Router, s *Server) {
	r.Route("/admin/pages", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminPagesIndex)
		r.Get("/new", s.adminPagesNew)
		r.Post("/", s.adminPagesCreate)
		r.Post("/batch_destroy", s.adminPagesBatchDestroy)
		r.Post("/batch_publish", s.adminPagesBatchPublish)
		r.Post("/batch_unpublish", s.adminPagesBatchUnpublish)
		r.Get("/{slug}/edit", s.adminPagesEdit)
		r.Post("/{slug}", s.adminPagesUpdate)
		r.Post("/{slug}/destroy", s.adminPagesDestroy)
	})
}

// adminPagesIndexData feeds admin_pages_index.html.
type adminPagesIndexData struct {
	Flash      templates.Flash
	Status     string // current filter: "", "publish", "draft", "schedule", "shared", "trash"
	Pages      []adminPageRow
	Page       int
	TotalPages int
	TimeZone   string
}

// adminPageRow is one list row; Page carries the record.
type adminPageRow struct {
	Page query.Page
}

// TruncatedRedirect mirrors truncate(page.redirect_url, length: 30).
func (row adminPageRow) TruncatedRedirect() string {
	return truncateRunes(row.Page.RedirectUrl.String, 30)
}

// adminPagesIndex renders GET /admin/pages, mirroring
// Admin::PagesController#index: optional status filter, page_order DESC,
// 100 per page. (load_comment_counts returns {} for Page, so no counts here.)
func (s *Server) adminPagesIndex(w http.ResponseWriter, r *http.Request) {
	var statusFilter *domain.Status
	statusName := ""
	switch r.URL.Query().Get("status") {
	case "publish":
		st := domain.StatusPublish
		statusFilter, statusName = &st, "publish"
	case "draft":
		st := domain.StatusDraft
		statusFilter, statusName = &st, "draft"
	case "schedule":
		st := domain.StatusSchedule
		statusFilter, statusName = &st, "schedule"
	case "shared":
		st := domain.StatusShared
		statusFilter, statusName = &st, "shared"
	case "trash":
		st := domain.StatusTrash
		statusFilter, statusName = &st, "trash"
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	offset := int64(page-1) * adminPagesPerPage

	var total int64
	var list []query.Page
	var err error
	if statusFilter == nil {
		total, err = s.Q.CountAdminPages(r.Context())
	} else {
		total, err = s.Q.CountAdminPagesByStatus(r.Context(), int64(*statusFilter))
	}
	if err != nil {
		s.Log.Error("count pages", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if statusFilter == nil {
		list, err = s.Q.ListAdminPages(r.Context(), query.ListAdminPagesParams{
			Limit: adminPagesPerPage, Offset: offset,
		})
	} else {
		list, err = s.Q.ListAdminPagesByStatus(r.Context(), query.ListAdminPagesByStatusParams{
			Status: int64(*statusFilter), Limit: adminPagesPerPage, Offset: offset,
		})
	}
	if err != nil {
		s.Log.Error("list pages", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	settings, err := s.Settings().Get(r.Context())
	if err != nil {
		s.Log.Error("load settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rows := make([]adminPageRow, 0, len(list))
	for _, p := range list {
		rows = append(rows, adminPageRow{Page: p})
	}

	s.render(w, http.StatusOK, "admin_pages_index", adminPagesIndexData{
		Flash:      PopFlash(r, w),
		Status:     statusName,
		Pages:      rows,
		Page:       page,
		TotalPages: int((total + adminPagesPerPage - 1) / adminPagesPerPage),
		TimeZone:   settings.TimeZone,
	})
}

// adminPageFormData feeds admin_pages_new.html and admin_pages_edit.html.
type adminPageFormData struct {
	Flash            templates.Flash
	Page             query.Page
	PathSlug         string   // persisted slug for the edit form actions; Page.Slug may hold an unsaved submitted value
	Errors           []string // validation messages, shown like the Rails form-errors block
	IsNew            bool
	StatusName       string // "" for new records (the prompt stays selected)
	RichContent      string // content_html shown in the rich_text textarea
	HTMLContent      string // content_html shown in the html textarea
	MarkdownContent  string // content_markdown source shown in the markdown textarea
	ScheduledAtValue string // datetime-local value in the site time zone
}

// adminPagesNew renders GET /admin/pages/new (Page.new(comment: true)).
func (s *Server) adminPagesNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "admin_pages_new", adminPageFormData{
		Flash: PopFlash(r, w),
		Page:  query.Page{Comment: 1, ContentType: string(domain.ContentTypeRichText)},
		IsNew: true,
	})
}

// adminPagesEdit renders GET /admin/pages/{slug}/edit.
func (s *Server) adminPagesEdit(w http.ResponseWriter, r *http.Request) {
	page, err := s.Q.GetAdminPageBySlug(r.Context(), pageSlugParam(r))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := adminPageFormData{
		Flash:      PopFlash(r, w),
		Page:       page,
		PathSlug:   page.Slug.String,
		StatusName: pageStatusName(page.Status),
	}
	if page.ContentType == string(domain.ContentTypeHTML) {
		data.HTMLContent = page.ContentHtml.String
	} else if page.ContentType == string(domain.ContentTypeMarkdown) {
		data.MarkdownContent = page.ContentMarkdown.String
	} else {
		data.RichContent = page.ContentHtml.String
	}
	data.ScheduledAtValue = s.formatScheduledAt(r, page.ScheduledAt)
	s.render(w, http.StatusOK, "admin_pages_edit", data)
}

// adminPagesCreate handles POST /admin/pages, mirroring
// Admin::PagesController#create.
func (s *Server) adminPagesCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	input, errs := s.parsePageForm(r, 0)
	if len(errs) > 0 {
		s.logPageActivity(r.Context(), "failed", 2, pageActivityDescription(input.Title.String, input.Slug.String, strings.Join(errs, ", ")))
		data := input.formData(true)
		data.Errors = errs
		s.render(w, http.StatusUnprocessableEntity, "admin_pages_new", data)
		return
	}
	now := time.Now().UTC().Unix()
	page, err := s.Q.CreatePage(r.Context(), query.CreatePageParams{
		Title:           input.Title,
		Slug:            input.Slug,
		ContentHtml:     input.StoredContent,
		ContentType:     input.ContentType,
		ContentMarkdown: input.MarkdownSource,
		RedirectUrl:     input.RedirectURL,
		PageOrder:       input.PageOrder,
		Status:          int64(input.Status),
		Comment:         input.Comment,
		ScheduledAt:     input.ScheduledAt,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		if isUniqueViolation(err) { // slug race lost between check and insert
			data := input.formData(true)
			data.Errors = []string{"Slug has already been taken"}
			s.render(w, http.StatusUnprocessableEntity, "admin_pages_new", data)
			return
		}
		s.Log.Error("create page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.schedulePagePublication(r.Context(), page.ID, input.Status, input.ScheduledAt)
	s.logPageActivity(r.Context(), "created", 0, pageActivityDescription(page.Title.String, page.Slug.String, ""))
	SetFlash(w, templates.Flash{Notice: "Page was successfully created."})
	http.Redirect(w, r, "/admin/pages", http.StatusFound)
}

// adminPagesUpdate handles POST /admin/pages/{slug} (Rails PATCH
// /admin/pages/:id), mirroring Admin::PagesController#update. The slug itself
// may change; the row is located by the path slug (find_by!(slug: params[:id])).
func (s *Server) adminPagesUpdate(w http.ResponseWriter, r *http.Request) {
	page, err := s.Q.GetAdminPageBySlug(r.Context(), pageSlugParam(r))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	input, errs := s.parsePageForm(r, page.ID)
	if len(errs) > 0 {
		s.logPageActivity(r.Context(), "failed", 2, pageActivityDescription(input.Title.String, input.Slug.String, strings.Join(errs, ", ")))
		data := input.formData(false)
		data.Errors = errs
		data.Page.ID = page.ID
		data.PathSlug = page.Slug.String
		s.render(w, http.StatusUnprocessableEntity, "admin_pages_edit", data)
		return
	}
	updated, err := s.Q.UpdatePage(r.Context(), query.UpdatePageParams{
		Title:           input.Title,
		Slug:            input.Slug,
		ContentHtml:     input.StoredContent,
		ContentType:     input.ContentType,
		ContentMarkdown: input.MarkdownSource,
		RedirectUrl:     input.RedirectURL,
		PageOrder:       input.PageOrder,
		Status:          int64(input.Status),
		Comment:         input.Comment,
		ScheduledAt:     input.ScheduledAt,
		UpdatedAt:       time.Now().UTC().Unix(),
		ID:              page.ID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			data := input.formData(false)
			data.Errors = []string{"Slug has already been taken"}
			data.Page.ID = page.ID
			data.PathSlug = page.Slug.String
			s.render(w, http.StatusUnprocessableEntity, "admin_pages_edit", data)
			return
		}
		s.Log.Error("update page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.schedulePagePublication(r.Context(), updated.ID, input.Status, input.ScheduledAt)
	s.logPageActivity(r.Context(), "updated", 0, pageActivityDescription(updated.Title.String, updated.Slug.String, ""))
	SetFlash(w, templates.Flash{Notice: "Page was successfully updated."})
	http.Redirect(w, r, "/admin/pages", http.StatusFound)
}

// adminPagesDestroy handles POST /admin/pages/{slug}/destroy (Rails DELETE
// /admin/pages/:id) with the two-stage semantics of spec 4.1 (task T10):
// a non-trash page is moved to trash, an already-trashed page is really
// deleted (comments included, per dependent: :destroy). Note: the Rails
// Admin::PagesController#destroy actually destroys directly; the two-stage
// behavior follows the task brief, spec 4.1, and the Rails index view
// (Trash vs. "Delete permanently?" actions).
func (s *Server) adminPagesDestroy(w http.ResponseWriter, r *http.Request) {
	page, err := s.Q.GetAdminPageBySlug(r.Context(), pageSlugParam(r))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if page.Status != int64(domain.StatusTrash) {
		if err := s.Q.UpdatePageStatus(r.Context(), query.UpdatePageStatusParams{
			Status:    int64(domain.StatusTrash),
			UpdatedAt: time.Now().UTC().Unix(),
			ID:        page.ID,
		}); err != nil {
			s.Log.Error("trash page", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.logPageActivity(r.Context(), "trashed", 0, pageActivityDescription(page.Title.String, page.Slug.String, ""))
		SetFlash(w, templates.Flash{Notice: "Page was successfully moved to trash."})
		http.Redirect(w, r, "/admin/pages", http.StatusSeeOther)
		return
	}
	if err := s.deletePageWithComments(r.Context(), page.ID); err != nil {
		s.Log.Error("delete page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logPageActivity(r.Context(), "deleted", 0, pageActivityDescription(page.Title.String, page.Slug.String, ""))
	SetFlash(w, templates.Flash{Notice: "Page was successfully deleted."})
	http.Redirect(w, r, "/admin/pages", http.StatusSeeOther)
}

// adminPagesBatchDestroy handles POST /admin/pages/batch_destroy.
func (s *Server) adminPagesBatchDestroy(w http.ResponseWriter, r *http.Request) {
	s.adminPagesBatch(w, r, "destroy")
}

// adminPagesBatchPublish handles POST /admin/pages/batch_publish.
func (s *Server) adminPagesBatchPublish(w http.ResponseWriter, r *http.Request) {
	s.adminPagesBatch(w, r, "publish")
}

// adminPagesBatchUnpublish handles POST /admin/pages/batch_unpublish.
func (s *Server) adminPagesBatchUnpublish(w http.ResponseWriter, r *http.Request) {
	s.adminPagesBatch(w, r, "unpublish")
}

// adminPagesBatch mirrors Admin::BaseController#process_batch_action: records
// are found by slug, missing ones are skipped, the first unexpected failure
// aborts with an alert, otherwise the count rides the notice. Batch destroy
// is a real delete (BaseController#perform_destroy calls destroy!), not the
// two-stage member destroy.
func (s *Server) adminPagesBatch(w http.ResponseWriter, r *http.Request, action string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	slugs := r.PostForm["ids"] // Rails params[:ids]; templates submit ids[]
	if len(slugs) == 0 {
		slugs = r.PostForm["ids[]"]
	}
	count := 0
	for _, slug := range slugs {
		page, err := s.Q.GetAdminPageBySlug(r.Context(), sql.NullString{String: slug, Valid: true})
		if err != nil {
			continue
		}
		switch action {
		case "destroy":
			err = s.deletePageWithComments(r.Context(), page.ID)
		case "publish":
			err = s.Q.UpdatePageStatus(r.Context(), query.UpdatePageStatusParams{
				Status:    int64(domain.StatusPublish),
				UpdatedAt: time.Now().UTC().Unix(),
				ID:        page.ID,
			})
		case "unpublish":
			err = s.Q.UpdatePageStatus(r.Context(), query.UpdatePageStatusParams{
				Status:    int64(domain.StatusDraft),
				UpdatedAt: time.Now().UTC().Unix(),
				ID:        page.ID,
			})
		}
		if err != nil {
			SetFlash(w, templates.Flash{Alert: fmt.Sprintf("Error processing %s for pages: %s", action, err)})
			http.Redirect(w, r, "/admin/pages", http.StatusFound)
			return
		}
		count++
	}
	verb := map[string]string{"destroy": "deleted", "publish": "published", "unpublish": "unpublished"}[action]
	SetFlash(w, templates.Flash{Notice: fmt.Sprintf("Successfully %s %d page(s).", verb, count)})
	http.Redirect(w, r, "/admin/pages", http.StatusFound)
}

// deletePageWithComments mirrors @page.destroy! with dependent: :destroy on
// the polymorphic comments association.
func (s *Server) deletePageWithComments(ctx context.Context, pageID int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.Q.WithTx(tx)
	if err := q.DeletePageComments(ctx, sql.NullInt64{Int64: pageID, Valid: true}); err != nil {
		return err
	}
	if err := q.DeletePageByID(ctx, pageID); err != nil {
		return err
	}
	return tx.Commit()
}

// schedulePagePublication mirrors Page#schedule_publication with the
// article-side cancel_old_jobs semantics required by task T10: when the saved
// page is scheduled, queued publish_page jobs for it are cancelled and a new
// one is enqueued at scheduled_at. Failures are logged, not raised (the save
// itself already succeeded, like the Rails after_save).
func (s *Server) schedulePagePublication(ctx context.Context, pageID int64, status domain.Status, scheduledAt sql.NullInt64) {
	if status != domain.StatusSchedule || !scheduledAt.Valid {
		return
	}
	if _, err := s.Q.CancelQueuedPublishPageJobs(ctx, query.CancelQueuedPublishPageJobsParams{
		LastError: sql.NullString{String: "superseded by a newer schedule", Valid: true},
		UpdatedAt: time.Now().UTC().Unix(),
		PageID:    pageID,
	}); err != nil {
		s.Log.Error("cancel publish_page jobs", "page_id", pageID, "error", err)
	}
	if _, err := s.Enqueuer().Enqueue(ctx, jobs.KindPublishPage,
		publishPagePayload{PageID: pageID},
		time.Unix(scheduledAt.Int64, 0).UTC()); err != nil {
		s.Log.Error("enqueue publish_page job", "page_id", pageID, "error", err)
	}
}

// publishPagePayload is the job_runs payload of a publish_page job
// (PublishScheduledPagesJob#perform(page_id)).
type publishPagePayload struct {
	PageID int64 `json:"page_id"`
}

// pageFormInput is the parsed + validated admin page form.
type pageFormInput struct {
	Title          sql.NullString
	Slug           sql.NullString
	ContentType    string
	RawContent     string // as submitted (content, markdown_content or html_content param)
	StoredContent  sql.NullString
	MarkdownSource sql.NullString // markdown source; NULL for other content types
	RedirectURL    sql.NullString
	PageOrder      int64
	Status         domain.Status
	Comment        int64
	ScheduledAt    sql.NullInt64
}

// formData rebuilds the re-rendered form (Rails render :new/:edit with the
// invalid record): submitted values, raw content split by content_type.
func (in pageFormInput) formData(isNew bool) adminPageFormData {
	data := adminPageFormData{
		Page: query.Page{
			Title:       in.Title,
			Slug:        in.Slug,
			ContentType: in.ContentType,
			RedirectUrl: in.RedirectURL,
			PageOrder:   in.PageOrder,
			Status:      int64(in.Status),
			Comment:     in.Comment,
			ScheduledAt: in.ScheduledAt,
		},
		IsNew:      isNew,
		StatusName: in.Status.String(),
	}
	if in.ContentType == string(domain.ContentTypeHTML) {
		data.HTMLContent = in.RawContent
	} else if in.ContentType == string(domain.ContentTypeMarkdown) {
		data.MarkdownContent = in.RawContent
	} else {
		data.RichContent = in.RawContent
	}
	return data
}

// parsePageForm parses and validates the page form, mirroring page.rb:
// title/slug presence, slug uniqueness (excludeID excepts the updated row,
// 0 on create), redirect_url must be an http(s) URL when present, html mode
// requires html_content, rich_text mode requires non-blank text content,
// schedule requires scheduled_at. Unknown enum values are validation errors
// (Rails raises ArgumentError instead; the form never sends them).
func (s *Server) parsePageForm(r *http.Request, excludeID int64) (pageFormInput, []string) {
	var in pageFormInput
	in.Title = sql.NullString{String: r.FormValue("title"), Valid: true}
	in.Slug = sql.NullString{String: r.FormValue("slug"), Valid: true}
	in.ContentType = r.FormValue("content_type")
	switch in.ContentType {
	case string(domain.ContentTypeHTML):
		in.RawContent = r.FormValue("html_content")
	case string(domain.ContentTypeMarkdown):
		in.RawContent = r.FormValue("markdown_content")
	default:
		in.RawContent = r.FormValue("content")
	}
	in.RedirectURL = sql.NullString{String: r.FormValue("redirect_url"), Valid: true}
	in.PageOrder, _ = strconv.ParseInt(strings.TrimSpace(r.FormValue("page_order")), 10, 64)
	if r.FormValue("comment") == "1" {
		in.Comment = 1
	}

	var errs []string
	if domain.IsBlank(in.Title.String) {
		errs = append(errs, "Title can't be blank")
	}
	if domain.IsBlank(in.Slug.String) {
		errs = append(errs, "Slug can't be blank")
	} else if taken, err := s.Q.AdminPageSlugCount(r.Context(), query.AdminPageSlugCountParams{
		Slug: in.Slug, ID: excludeID,
	}); err == nil && taken > 0 {
		errs = append(errs, "Slug has already been taken")
	}
	if in.RedirectURL.String != "" && !validRedirectURL(in.RedirectURL.String) {
		errs = append(errs, "Redirect url is not a valid URL")
	}
	switch domain.ContentType(in.ContentType) {
	case domain.ContentTypeRichText, domain.ContentTypeHTML, domain.ContentTypeMarkdown:
	default:
		errs = append(errs, "Content type is not included in the list")
	}
	switch statusName := r.FormValue("status"); statusName {
	case "draft":
		in.Status = domain.StatusDraft
	case "publish":
		in.Status = domain.StatusPublish
	case "schedule":
		in.Status = domain.StatusSchedule
	case "trash":
		in.Status = domain.StatusTrash
	case "shared":
		in.Status = domain.StatusShared
	default:
		errs = append(errs, "Status is not included in the list")
	}
	if raw := strings.TrimSpace(r.FormValue("scheduled_at")); raw != "" {
		if ts, err := parseScheduledAt(raw, s.siteTimeZone(r)); err == nil {
			in.ScheduledAt = sql.NullInt64{Int64: ts, Valid: true}
		}
	}
	if in.Status == domain.StatusSchedule && !in.ScheduledAt.Valid {
		errs = append(errs, "Scheduled at can't be blank")
	}
	switch in.ContentType {
	case string(domain.ContentTypeHTML):
		if domain.IsBlank(in.RawContent) {
			errs = append(errs, "Html content can't be blank")
		}
	case string(domain.ContentTypeMarkdown):
		if domain.IsBlank(in.RawContent) {
			errs = append(errs, "Content can't be blank")
		}
	default:
		if domain.IsBlank(domain.PlainText(in.RawContent)) {
			errs = append(errs, "Content can't be blank")
		}
	}
	if len(errs) > 0 {
		return in, errs
	}
	// Markdown pages store the source in content_markdown and the rendered
	// HTML in content_html, like articles (0002). Sanitize once at write time
	// (decision log 2026-08-03, spec 4.4).
	body := in.RawContent
	if in.ContentType == string(domain.ContentTypeMarkdown) {
		body = domain.RenderMarkdown(body)
		in.MarkdownSource = sql.NullString{String: in.RawContent, Valid: !domain.IsBlank(in.RawContent)}
	}
	in.StoredContent = sql.NullString{
		String: domain.AddLazyLoading(domain.SanitizeHTML(body)),
		Valid:  true,
	}
	return in, nil
}

// validRedirectURL mirrors UrlValidator: http(s) scheme with a host.
func validRedirectURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// parseScheduledAt parses the datetime-local form value in the site time zone
// (spec 4.12) into unix seconds; an unparseable value typecasts to invalid,
// like the Rails datetime column.
func parseScheduledAt(raw, tzName string) (int64, error) {
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		loc = time.UTC
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return t.UTC().Unix(), nil
		}
	}
	return 0, fmt.Errorf("invalid datetime %q", raw)
}

// formatScheduledAt renders the stored unix seconds for the datetime-local
// input in the site time zone.
func (s *Server) formatScheduledAt(r *http.Request, at sql.NullInt64) string {
	if !at.Valid {
		return ""
	}
	loc, err := time.LoadLocation(s.siteTimeZone(r))
	if err != nil {
		loc = time.UTC
	}
	return time.Unix(at.Int64, 0).In(loc).Format("2006-01-02T15:04")
}

// siteTimeZone reads settings.time_zone through the shared cache.
func (s *Server) siteTimeZone(r *http.Request) string {
	settings, err := s.Settings().Get(r.Context())
	if err != nil {
		return "UTC"
	}
	return settings.TimeZone
}

// pageStatusName maps the stored status integer back to its enum name.
func pageStatusName(status int64) string {
	return domain.Status(status).String()
}

// pageSlugParam reads the {slug} path parameter as the nullable slug column.
func pageSlugParam(r *http.Request) sql.NullString {
	return sql.NullString{String: chi.URLParam(r, "slug"), Valid: true}
}

// logPageActivity mirrors the ActivityLog.log! calls in
// Admin::PagesController (action/target/level/description on activity_logs).
// Like the Rails original, it never breaks the main flow. It writes raw SQL
// because the activity-logs feature (and its queries) belongs to a later
// task; swap for its shared helper once that lands.
func (s *Server) logPageActivity(ctx context.Context, action string, level int64, description string) {
	now := time.Now().Unix()
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO activity_logs (level, action, target, description, created_at, updated_at) VALUES (?, ?, 'page', ?, ?, ?)`,
		level, action, description, now, now)
	if err != nil {
		s.Log.Warn("activity log", "error", err)
	}
}

// pageActivityDescription mirrors ActivityLog's page payload; the Rails
// controller logs structured attributes that ActivityLog flattens to text.
func pageActivityDescription(title, slug, errs string) string {
	desc := fmt.Sprintf("title=%s slug=%s", activityQuote(title), activityQuote(slug))
	if errs != "" {
		desc += " errors=" + activityQuote(errs)
	}
	return desc
}
