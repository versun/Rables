package httpd

import (
	"database/sql"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"rables/internal/db/query"
	"rables/internal/templates"
)

// setupPageData feeds auth_setup.html.
type setupPageData struct {
	Flash    templates.Flash
	UserName string
	Title    string
}

// setupForm renders GET /setup, mirroring SetupController#show: once setup is
// done it bounces to the admin root.
func (s *Server) setupForm(w http.ResponseWriter, r *http.Request) {
	if !s.setupIncomplete(r.Context()) {
		SetFlash(w, templates.Flash{Notice: "Setup has already been completed."})
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	s.render(w, http.StatusOK, "auth_setup", setupPageData{Flash: PopFlash(r, w)})
}

// setupCreate handles POST /setup, mirroring SetupController#create: inside
// one transaction it re-checks incompleteness, creates the admin user and
// flips settings.setup_completed to 1.
func (s *Server) setupCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	userName := normalizeUserName(r.FormValue("user_name"))
	password := r.FormValue("password")
	confirmation := r.FormValue("password_confirmation")

	fail := func(alert string) {
		s.render(w, http.StatusUnprocessableEntity, "auth_setup", setupPageData{
			Flash:    templates.Flash{Alert: alert},
			UserName: userName,
			Title:    r.FormValue("title"),
		})
	}

	// Model-level validations (Setting validates :url once setup_completed;
	// the Rails form also marks title required).
	switch {
	case userName == "":
		fail("User name can't be blank.")
		return
	case password == "":
		fail("Password can't be blank.")
		return
	case password != confirmation:
		fail("Password confirmation doesn't match password.")
		return
	case r.FormValue("title") == "":
		fail("Title can't be blank.")
		return
	case r.FormValue("url") == "":
		fail("URL can't be blank.")
		return
	}

	digest, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		s.Log.Error("hash password", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.Log.Error("begin setup tx", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	q := s.Q.WithTx(tx)
	ctx := r.Context()
	now := time.Now().Unix()

	// Fresh re-check inside the transaction to shrink the TOCTOU window,
	// like Rails' setup_incomplete_fresh?.
	users, err := q.CountUsers(ctx)
	if err != nil {
		s.Log.Error("setup: count users", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	settings, settingsErr := q.GetSettings(ctx)
	completed := users > 0 && settingsErr == nil && settings.SetupCompleted != 0
	if completed {
		SetFlash(w, templates.Flash{Notice: "Setup has already been completed."})
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}

	if _, err := q.CreateUser(ctx, query.CreateUserParams{
		UserName:       userName,
		PasswordDigest: string(digest),
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		if isUniqueViolation(err) {
			fail("User name has already been taken.")
			return
		}
		s.Log.Error("setup: create user", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := q.EnsureSettings(ctx, query.EnsureSettingsParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		s.Log.Error("setup: ensure settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tz := r.FormValue("time_zone")
	if tz == "" {
		tz = "UTC"
	}
	str := func(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
	if err := q.CompleteSetup(ctx, query.CompleteSetupParams{
		Title:       str(r.FormValue("title")),
		Description: str(r.FormValue("description")),
		Author:      str(r.FormValue("author")),
		Url:         str(r.FormValue("url")),
		TimeZone:    tz,
		UpdatedAt:   now,
	}); err != nil {
		s.Log.Error("setup: complete settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		s.Log.Error("setup: commit", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.InvalidateSetupCache()
	SetFlash(w, templates.Flash{Notice: "Setup completed successfully! Please log in with your admin credentials."})
	http.Redirect(w, r, "/session/new", http.StatusFound)
}
