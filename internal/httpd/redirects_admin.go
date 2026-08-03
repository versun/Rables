package httpd

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/service/activity"
	"rables/internal/templates"
)

// RegisterRedirectsRoutes mounts the admin redirect CRUD behind RequireAuth,
// mirroring Rails' namespace :admin resources :redirects (the controller
// implements no #show). HTML forms cannot PATCH/DELETE, so update maps
// PATCH /admin/redirects/:id to POST /admin/redirects/{id} and destroy maps
// DELETE to POST /admin/redirects/{id}/destroy.
func RegisterRedirectsRoutes(r chi.Router, s *Server) {
	r.Route("/admin/redirects", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminRedirectsIndex)
		r.Get("/new", s.adminRedirectsNew)
		r.Post("/", s.adminRedirectsCreate)
		r.Get("/{id}/edit", s.adminRedirectsEdit)
		r.Post("/{id}", s.adminRedirectsUpdate)
		r.Post("/{id}/destroy", s.adminRedirectsDestroy)
	})
}

// adminRedirectsIndexData feeds admin_redirects_index.html.
type adminRedirectsIndexData struct {
	Flash     templates.Flash
	Redirects []query.Redirect
}

// adminRedirectsIndex renders GET /admin/redirects (newest first).
func (s *Server) adminRedirectsIndex(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListRedirects(r.Context())
	if err != nil {
		s.Log.Error("list redirects", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "admin_redirects_index", adminRedirectsIndexData{
		Flash:     PopFlash(r, w),
		Redirects: rows,
	})
}

// adminRedirectFormData feeds admin_redirects_new.html and
// admin_redirects_edit.html.
type adminRedirectFormData struct {
	Flash    templates.Flash
	Redirect query.Redirect
	Errors   []string // validation messages, shown like the Rails form-errors block
}

// adminRedirectsNew renders GET /admin/redirects/new. New records default to
// enabled, like the Rails form's checkbox.
func (s *Server) adminRedirectsNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "admin_redirects_new", adminRedirectFormData{
		Flash:    PopFlash(r, w),
		Redirect: query.Redirect{Enabled: 1},
	})
}

// adminRedirectsCreate handles POST /admin/redirects, mirroring
// Admin::RedirectsController#create.
func (s *Server) adminRedirectsCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	in := redirectFromForm(r)
	if errs := validateRedirectForm(in); len(errs) > 0 {
		activity.Log(r.Context(), s.DB, "error", "failed", "redirect",
			fmt.Sprintf("regex=%s errors=%s", activity.Quote(in.Regex), activity.Quote(strings.Join(errs, ", "))))
		s.render(w, http.StatusUnprocessableEntity, "admin_redirects_new", adminRedirectFormData{
			Redirect: in,
			Errors:   errs,
		})
		return
	}
	now := time.Now().Unix()
	redirect, err := s.Q.CreateRedirect(r.Context(), query.CreateRedirectParams{
		Regex:       in.Regex,
		Replacement: in.Replacement,
		Permanent:   in.Permanent,
		Enabled:     in.Enabled,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		s.Log.Error("create redirect", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	activity.Log(r.Context(), s.DB, "info", "created", "redirect",
		fmt.Sprintf("regex=%s replacement=%s", activity.Quote(redirect.Regex), activity.Quote(redirect.Replacement)))
	s.InvalidateRedirectCache()
	SetFlash(w, templates.Flash{Notice: "Redirect was successfully created."})
	http.Redirect(w, r, "/admin/redirects", http.StatusFound)
}

// adminRedirectsEdit renders GET /admin/redirects/{id}/edit.
func (s *Server) adminRedirectsEdit(w http.ResponseWriter, r *http.Request) {
	redirect, err := s.Q.GetRedirectByID(r.Context(), redirectIDParam(r))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get redirect", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "admin_redirects_edit", adminRedirectFormData{
		Flash:    PopFlash(r, w),
		Redirect: redirect,
	})
}

// adminRedirectsUpdate handles POST /admin/redirects/{id} (Rails PATCH
// /admin/redirects/:id), mirroring Admin::RedirectsController#update.
func (s *Server) adminRedirectsUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := redirectIDParam(r)
	if _, err := s.Q.GetRedirectByID(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.Log.Error("get redirect", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	in := redirectFromForm(r)
	in.ID = id
	if errs := validateRedirectForm(in); len(errs) > 0 {
		activity.Log(r.Context(), s.DB, "error", "failed", "redirect",
			fmt.Sprintf("regex=%s errors=%s", activity.Quote(in.Regex), activity.Quote(strings.Join(errs, ", "))))
		s.render(w, http.StatusUnprocessableEntity, "admin_redirects_edit", adminRedirectFormData{
			Redirect: in,
			Errors:   errs,
		})
		return
	}
	if err := s.Q.UpdateRedirect(r.Context(), query.UpdateRedirectParams{
		Regex:       in.Regex,
		Replacement: in.Replacement,
		Permanent:   in.Permanent,
		Enabled:     in.Enabled,
		UpdatedAt:   time.Now().Unix(),
		ID:          id,
	}); err != nil {
		s.Log.Error("update redirect", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	activity.Log(r.Context(), s.DB, "info", "updated", "redirect",
		fmt.Sprintf("regex=%s replacement=%s", activity.Quote(in.Regex), activity.Quote(in.Replacement)))
	s.InvalidateRedirectCache()
	SetFlash(w, templates.Flash{Notice: "Redirect was successfully updated."})
	http.Redirect(w, r, "/admin/redirects", http.StatusFound)
}

// adminRedirectsDestroy handles POST /admin/redirects/{id}/destroy (Rails
// DELETE /admin/redirects/:id), mirroring Admin::RedirectsController#destroy.
func (s *Server) adminRedirectsDestroy(w http.ResponseWriter, r *http.Request) {
	id := redirectIDParam(r)
	redirect, err := s.Q.GetRedirectByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get redirect", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.Q.DeleteRedirect(r.Context(), id); err != nil {
		s.Log.Error("destroy redirect", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	activity.Log(r.Context(), s.DB, "info", "deleted", "redirect",
		fmt.Sprintf("regex=%s replacement=%s", activity.Quote(redirect.Regex), activity.Quote(redirect.Replacement)))
	s.InvalidateRedirectCache()
	SetFlash(w, templates.Flash{Notice: "Redirect was successfully deleted."})
	http.Redirect(w, r, "/admin/redirects", http.StatusSeeOther)
}

// redirectIDParam reads the {id} path parameter; an unparsable id yields 0,
// which matches no redirect, like Redirect.find raising RecordNotFound.
func redirectIDParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id
}

// redirectFromForm reads the permitted redirect params (regex, replacement,
// permanent, enabled).
func redirectFromForm(r *http.Request) query.Redirect {
	return query.Redirect{
		Regex:       r.FormValue("regex"),
		Replacement: r.FormValue("replacement"),
		Permanent:   checkboxInt(r, "permanent"),
		Enabled:     checkboxInt(r, "enabled"),
	}
}

// checkboxInt interprets a Rails-style checkbox param: "1"/"true"/"on" is 1,
// anything else (including the hidden "0" or an absent param) is 0.
func checkboxInt(r *http.Request, name string) int64 {
	switch v := r.PostForm[name]; len(v) {
	case 0:
		return 0
	default:
		switch v[len(v)-1] {
		case "1", "true", "on":
			return 1
		}
		return 0
	}
}

// validateRedirectForm mirrors the Redirect validations: regex and replacement
// presence first (in declaration order), then the regex must compile.
func validateRedirectForm(in query.Redirect) []string {
	var errs []string
	regexBlank := strings.TrimSpace(in.Regex) == ""
	if regexBlank {
		errs = append(errs, "Regex can't be blank")
	}
	if strings.TrimSpace(in.Replacement) == "" {
		errs = append(errs, "Replacement can't be blank")
	}
	if !regexBlank {
		if _, err := regexp.Compile(in.Regex); err != nil {
			errs = append(errs, fmt.Sprintf("Regex is not a valid regular expression: %s", err))
		}
	}
	return errs
}
