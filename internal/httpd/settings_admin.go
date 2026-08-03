package httpd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/settings"
	"rables/internal/templates"
)

// settingsExtKey is the Ext registry key for the shared settings cache.
const settingsExtKey = "settings"

// Settings returns the process-wide settings cache (5-minute read TTL,
// invalidated on write), creating it on first use. Later features read site
// settings through this accessor; writers that bypass Cache.Update (setup,
// imports) must call s.Settings().Invalidate() afterwards.
func (s *Server) Settings() *settings.Cache {
	v, _ := s.Ext.LoadOrStore(settingsExtKey, settings.NewCache(s.DB, s.Log))
	return v.(*settings.Cache)
}

// RegisterSettingsRoutes mounts the admin settings pages. Rails routes them
// as GET /admin/setting/edit + PATCH /admin/setting; HTML forms cannot send
// PATCH, so the update is POST /admin/setting instead. Wired into NewRouter
// by the integrator.
func RegisterSettingsRoutes(r chi.Router, s *Server) {
	r.Route("/admin/setting", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/edit", s.settingsEditForm)
		r.Post("/", s.settingsUpdate)
	})
}

// settingsPageData feeds admin_setting_edit.html.
type settingsPageData struct {
	Flash           templates.Flash
	Setting         query.Setting
	SocialLinksJSON string // pretty-printed for the textarea
}

// settingsEditForm renders GET /admin/setting/edit, mirroring
// Admin::SettingsController#edit (Setting.first_or_create).
func (s *Server) settingsEditForm(w http.ResponseWriter, r *http.Request) {
	st, err := s.Settings().Get(r.Context())
	if err != nil {
		s.Log.Error("load settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "admin_setting_edit", settingsPageData{
		Flash:           PopFlash(r, w),
		Setting:         st,
		SocialLinksJSON: prettySocialLinks(st.SocialLinks.String),
	})
}

// settingsUpdate handles POST /admin/setting, mirroring
// Admin::SettingsController#update. Validation failures re-render the form
// with 422 and the submitted values, like the Rails render :edit.
func (s *Server) settingsUpdate(w http.ResponseWriter, r *http.Request) {
	cache := s.Settings()
	current, err := cache.Get(r.Context())
	if err != nil {
		s.Log.Error("load settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	str := func(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
	// Overlay the submitted values on the current row so a failed update
	// re-renders with the user's input and untouched columns keep their values.
	submitted := current
	submitted.Title = str(r.FormValue("title"))
	submitted.Description = str(r.FormValue("description"))
	submitted.Author = str(r.FormValue("author"))
	submitted.Url = str(r.FormValue("url"))
	submitted.HeadCode = str(r.FormValue("head_code"))
	submitted.CustomCss = str(r.FormValue("custom_css"))
	submitted.ToolCode = str(r.FormValue("tool_code"))
	submitted.Giscus = str(r.FormValue("giscus"))
	// Like Rails, the model does not validate time_zone (the form select was
	// the only guard); empty falls back to UTC and unknown names render as UTC.
	submitted.TimeZone = r.FormValue("time_zone")
	if submitted.TimeZone == "" {
		submitted.TimeZone = "UTC"
	}

	socialLinksJSON := r.FormValue("social_links_json")
	if strings.TrimSpace(socialLinksJSON) != "" {
		normalized, err := settings.NormalizeSocialLinks(socialLinksJSON)
		if err != nil {
			s.renderSettingsForm(w, submitted, socialLinksJSON, "Social links JSON is invalid: not a JSON object.")
			return
		}
		submitted.SocialLinks = sql.NullString{String: normalized, Valid: true}
	}
	// Blank social_links_json leaves the column unchanged, matching Rails'
	// parse_social_links_json which only runs when the input is present.

	// Rails validates url presence once setup is completed.
	if current.SetupCompleted != 0 && !submitted.Url.Valid {
		s.renderSettingsForm(w, submitted, socialLinksJSON, "Url can't be blank.")
		return
	}

	if err := cache.Update(r.Context(), query.UpdateSettingsParams{
		Title:       submitted.Title,
		Description: submitted.Description,
		Author:      submitted.Author,
		Url:         submitted.Url,
		TimeZone:    submitted.TimeZone,
		HeadCode:    submitted.HeadCode,
		CustomCss:   submitted.CustomCss,
		ToolCode:    submitted.ToolCode,
		Giscus:      submitted.Giscus,
		SocialLinks: submitted.SocialLinks,
		UpdatedAt:   time.Now().Unix(),
	}); err != nil {
		s.Log.Error("update settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	SetFlash(w, templates.Flash{Notice: "Setting was successfully updated."})
	http.Redirect(w, r, "/admin/setting/edit", http.StatusFound)
}

// renderSettingsForm re-renders the edit page with a 422 and an alert.
func (s *Server) renderSettingsForm(w http.ResponseWriter, st query.Setting, socialLinksJSON, alert string) {
	s.render(w, http.StatusUnprocessableEntity, "admin_setting_edit", settingsPageData{
		Flash:           templates.Flash{Alert: alert},
		Setting:         st,
		SocialLinksJSON: socialLinksJSON,
	})
}

// prettySocialLinks formats the stored JSON for the textarea, like the Rails
// view's JSON.pretty_generate; empty stays empty.
func prettySocialLinks(raw string) string {
	if raw == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return raw
	}
	return buf.String()
}
