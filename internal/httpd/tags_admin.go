package httpd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	tagsvc "rables/internal/service/tags"
	"rables/internal/templates"
)

// RegisterTagsRoutes mounts the admin tag CRUD behind RequireAuth, mirroring
// Rails' namespace :admin resources :tags with collection post
// :batch_destroy. HTML forms cannot PATCH/DELETE, so update maps Rails PATCH
// /admin/tags/:id to POST /admin/tags/{id} and destroy maps DELETE to
// POST /admin/tags/{id}/destroy.
func RegisterTagsRoutes(r chi.Router, s *Server) {
	r.Route("/admin/tags", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminTagsIndex)
		r.Get("/new", s.adminTagsNew)
		r.Post("/", s.adminTagsCreate)
		r.Post("/batch_destroy", s.adminTagsBatchDestroy)
		r.Get("/{id}/edit", s.adminTagsEdit)
		r.Post("/{id}", s.adminTagsUpdate)
		r.Post("/{id}/destroy", s.adminTagsDestroy)
	})
}

// adminTagsIndexData feeds admin_tags_index.html.
type adminTagsIndexData struct {
	Flash templates.Flash
	Tags  []query.ListTagsWithArticleCountRow
}

// adminTagsIndex renders GET /admin/tags.
func (s *Server) adminTagsIndex(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListTagsWithArticleCount(r.Context())
	if err != nil {
		s.Log.Error("list tags", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "admin_tags_index", adminTagsIndexData{
		Flash: PopFlash(r, w),
		Tags:  rows,
	})
}

// adminTagFormData feeds admin_tags_new.html and admin_tags_edit.html.
type adminTagFormData struct {
	Flash templates.Flash
	Tag   query.Tag
	Error string // validation message, shown like the Rails form-errors block
}

// adminTagsNew renders GET /admin/tags/new.
func (s *Server) adminTagsNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "admin_tags_new", adminTagFormData{Flash: PopFlash(r, w)})
}

// adminTagsCreate handles POST /admin/tags, mirroring
// Admin::TagsController#create.
func (s *Server) adminTagsCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	tag, err := tagsvc.Create(r.Context(), s.Q, name)
	if err != nil {
		if msg := tagFormError(err); msg != "" {
			s.logTagActivity(r.Context(), "failed", 2, fmt.Sprintf("name=%s errors=%s", activityQuote(name), activityQuote(msg)))
			s.render(w, http.StatusUnprocessableEntity, "admin_tags_new", adminTagFormData{
				Tag:   query.Tag{Name: strings.TrimSpace(name)},
				Error: msg,
			})
			return
		}
		s.Log.Error("create tag", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logTagActivity(r.Context(), "created", 0, fmt.Sprintf("name=%s slug=%s", activityQuote(tag.Name), activityQuote(tag.Slug)))
	SetFlash(w, templates.Flash{Notice: "Tag was successfully created."})
	http.Redirect(w, r, "/admin/tags", http.StatusFound)
}

// adminTagsEdit renders GET /admin/tags/{id}/edit.
func (s *Server) adminTagsEdit(w http.ResponseWriter, r *http.Request) {
	tag, err := s.Q.GetTagByID(r.Context(), tagIDParam(r))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get tag", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "admin_tags_edit", adminTagFormData{
		Flash: PopFlash(r, w),
		Tag:   tag,
	})
}

// adminTagsUpdate handles POST /admin/tags/{id} (Rails PATCH /admin/tags/:id),
// mirroring Admin::TagsController#update. A rename also bumps the tagged
// articles' updated_at inside the same transaction (Tag#touch_articles).
func (s *Server) adminTagsUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := tagIDParam(r)
	name := r.FormValue("name")
	if err := tagsvc.Rename(r.Context(), s.DB, id, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if msg := tagFormError(err); msg != "" {
			s.logTagActivity(r.Context(), "failed", 2, fmt.Sprintf("name=%s errors=%s", activityQuote(name), activityQuote(msg)))
			tag, getErr := s.Q.GetTagByID(r.Context(), id)
			if getErr != nil {
				http.NotFound(w, r)
				return
			}
			tag.Name = strings.TrimSpace(name)
			s.render(w, http.StatusUnprocessableEntity, "admin_tags_edit", adminTagFormData{Tag: tag, Error: msg})
			return
		}
		s.Log.Error("update tag", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tag, err := s.Q.GetTagByID(r.Context(), id)
	if err != nil {
		s.Log.Error("get tag", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logTagActivity(r.Context(), "updated", 0, fmt.Sprintf("name=%s slug=%s", activityQuote(tag.Name), activityQuote(tag.Slug)))
	SetFlash(w, templates.Flash{Notice: "Tag was successfully updated."})
	http.Redirect(w, r, "/admin/tags", http.StatusFound)
}

// adminTagsDestroy handles POST /admin/tags/{id}/destroy (Rails DELETE
// /admin/tags/:id), mirroring Admin::TagsController#destroy. Join rows are
// removed by the dependent-destroy semantics of tags.Destroy.
func (s *Server) adminTagsDestroy(w http.ResponseWriter, r *http.Request) {
	id := tagIDParam(r)
	tag, err := s.Q.GetTagByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get tag", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := tagsvc.Destroy(r.Context(), s.DB, id); err != nil {
		s.Log.Error("destroy tag", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logTagActivity(r.Context(), "deleted", 0, fmt.Sprintf("name=%s slug=%s", activityQuote(tag.Name), activityQuote(tag.Slug)))
	SetFlash(w, templates.Flash{Notice: "Tag was successfully deleted."})
	http.Redirect(w, r, "/admin/tags", http.StatusSeeOther)
}

// adminTagsBatchDestroy handles POST /admin/tags/batch_destroy, mirroring
// Admin::BaseController#process_batch_action(action: :destroy) with
// find_record_for_batch by id: missing records are skipped, the first failure
// aborts with an alert, otherwise the count is reported in the notice.
func (s *Server) adminTagsBatchDestroy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ids := r.PostForm["ids"] // Rails params[:ids]; templates submit ids[]
	if len(ids) == 0 {
		ids = r.PostForm["ids[]"]
	}
	count := 0
	for _, raw := range ids {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		if err := tagsvc.Destroy(r.Context(), s.DB, id); errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			SetFlash(w, templates.Flash{Alert: fmt.Sprintf("Error processing destroy for tags: %s", err)})
			http.Redirect(w, r, "/admin/tags", http.StatusFound)
			return
		}
		count++
	}
	SetFlash(w, templates.Flash{Notice: fmt.Sprintf("Successfully deleted %d tag(s).", count)})
	http.Redirect(w, r, "/admin/tags", http.StatusFound)
}

// tagIDParam reads the {id} path parameter; an unparsable id yields 0, which
// matches no tag, like Tag.find raising RecordNotFound.
func tagIDParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id
}

// tagFormError maps the tag validation failures to the Rails full-message
// wording; other errors return "".
func tagFormError(err error) string {
	switch {
	case errors.Is(err, tagsvc.ErrNameBlank):
		return "Name can't be blank"
	case errors.Is(err, tagsvc.ErrNameTaken):
		return "Name has already been taken"
	}
	return ""
}

// logTagActivity mirrors the ActivityLog.log! calls in
// Admin::TagsController (action/target/level/description on activity_logs).
// Like the Rails original, it never breaks the main flow. It writes raw SQL
// because the activity-logs feature (and its queries) belongs to a later
// task; swap for its shared helper once that lands.
func (s *Server) logTagActivity(ctx context.Context, action string, level int64, description string) {
	now := time.Now().Unix()
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO activity_logs (level, action, target, description, created_at, updated_at) VALUES (?, ?, 'tag', ?, ?, ?)`,
		level, action, description, now, now)
	if err != nil {
		s.Log.Warn("activity log", "error", err)
	}
}

// activityQuote mirrors ActivityLog.quote_string: squished, with backslashes
// and double quotes escaped, wrapped in double quotes.
func activityQuote(text string) string {
	s := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
