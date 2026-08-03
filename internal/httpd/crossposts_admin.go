package httpd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/service/activity"
	articlesvc "rables/internal/service/articles"
	"rables/internal/service/crosspost"
	"rables/internal/templates"
)

// RegisterCrosspostRoutes mounts the admin crosspost settings pages. Rails
// routes them as resources :crossposts (GET /admin/crossposts, PATCH
// /admin/crossposts/:id) plus member POST /admin/crossposts/:id/verify; the
// PATCH becomes POST here because HTML forms cannot send it. Wired into
// NewRouter by the integrator.
func RegisterCrosspostRoutes(r chi.Router, s *Server) {
	r.Route("/admin/crossposts", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.crosspostsIndex)
		r.Post("/{platform}", s.crosspostUpdate)
		r.Post("/{platform}/verify", s.crosspostVerify)
	})
}

// crosspostPlatformView feeds one platform tab of admin_crossposts.html.
type crosspostPlatformView struct {
	Config          query.Crosspost
	DefaultMaxChars int
}

// crosspostsPageData feeds admin_crossposts.html.
type crosspostsPageData struct {
	Flash          templates.Flash
	ActivePlatform string
	Platforms      map[string]crosspostPlatformView
}

// crosspostsIndex renders GET /admin/crossposts, mirroring
// Admin::CrosspostsController#index: read-only, missing rows are built in
// memory (find_or_initialize_by) and never written on GET.
func (s *Server) crosspostsIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	platforms := make(map[string]crosspostPlatformView, len(articlesvc.CrosspostPlatforms))
	for _, name := range articlesvc.CrosspostPlatforms {
		cfg, err := s.Q.GetCrosspostByPlatform(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			cfg = query.Crosspost{Platform: name}
		} else if err != nil {
			s.Log.Error("load crosspost config", "platform", name, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		platforms[name] = crosspostPlatformView{
			Config:          cfg,
			DefaultMaxChars: domain.EffectiveMaxCharacters(name, 0),
		}
	}
	active := r.URL.Query().Get("platform")
	if !slices.Contains(articlesvc.CrosspostPlatforms, active) {
		active = "mastodon"
	}
	s.render(w, http.StatusOK, "admin_crossposts", crosspostsPageData{
		Flash:          PopFlash(r, w),
		ActivePlatform: active,
		Platforms:      platforms,
	})
}

// crosspostUpdate handles POST /admin/crossposts/{platform}, mirroring
// Admin::CrosspostsController#update: the permitted crosspost_params overlay
// the stored row (find_or_create_by + update), the model validations run,
// and the redirect carries the notice/alert.
func (s *Server) crosspostUpdate(w http.ResponseWriter, r *http.Request) {
	platform := chi.URLParam(r, "platform")
	redirectURL := "/admin/crossposts?platform=" + url.QueryEscape(platform)
	if !slices.Contains(articlesvc.CrosspostPlatforms, platform) {
		// find_or_create_by with an invalid platform fails the inclusion
		// validation, so update redirects with the alert.
		const msg = "Platform is not included in the list"
		s.logCrosspostActivity(r.Context(), "error", "failed", platform, msg)
		SetFlash(w, templates.Flash{Alert: msg})
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	now := time.Now().Unix()
	if err := s.Q.EnsureCrosspost(ctx, query.EnsureCrosspostParams{Platform: platform, CreatedAt: now, UpdatedAt: now}); err != nil {
		s.Log.Error("ensure crosspost", "platform", platform, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cfg, err := s.Q.GetCrosspostByPlatform(ctx, platform)
	if err != nil {
		s.Log.Error("load crosspost", "platform", platform, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	cfg = applyCrosspostForm(r, cfg)
	if errs := crosspostValidationErrors(cfg); len(errs) > 0 {
		s.logCrosspostActivity(ctx, "error", "failed", platform, strings.Join(errs, ", "))
		SetFlash(w, templates.Flash{Alert: strings.Join(errs, ", ")})
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	if err := s.Q.UpdateCrosspost(ctx, query.UpdateCrosspostParams{
		Enabled:              cfg.Enabled,
		ServerUrl:            cfg.ServerUrl,
		Username:             cfg.Username,
		AccessToken:          cfg.AccessToken,
		AccessTokenSecret:    cfg.AccessTokenSecret,
		ClientKey:            cfg.ClientKey,
		ClientSecret:         cfg.ClientSecret,
		ApiKey:               cfg.ApiKey,
		ApiKeySecret:         cfg.ApiKeySecret,
		AppPassword:          cfg.AppPassword,
		MaxCharacters:        cfg.MaxCharacters,
		AutoFetchComments:    cfg.AutoFetchComments,
		CommentFetchSchedule: cfg.CommentFetchSchedule,
		UpdatedAt:            now,
		Platform:             platform,
	}); err != nil {
		s.Log.Error("update crosspost", "platform", platform, "error", err)
		s.logCrosspostActivity(ctx, "error", "failed", platform, "Failed to update CrossPost settings.")
		SetFlash(w, templates.Flash{Alert: "Failed to update CrossPost settings."})
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}
	s.logCrosspostActivity(ctx, "info", "updated", platform, "")
	SetFlash(w, templates.Flash{Notice: "CrossPost settings updated successfully."})
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// applyCrosspostForm overlays the submitted crosspost[...] fields on the
// stored row, like the Rails partial permit+update: only keys present in the
// submission change (each platform form renders a different field set).
func applyCrosspostForm(r *http.Request, cfg query.Crosspost) query.Crosspost {
	str := func(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
	last := func(name string) (string, bool) {
		values, ok := r.PostForm[name]
		if !ok || len(values) == 0 {
			return "", false
		}
		return values[len(values)-1], true
	}

	if v, present := formCheckbox(r, "crosspost[enabled]"); present {
		cfg.Enabled = 0
		if v {
			cfg.Enabled = 1
		}
	}
	for _, field := range []string{
		"server_url", "access_token", "access_token_secret",
		"client_key", "client_secret", "api_key", "api_key_secret",
		"app_password", "username",
	} {
		v, ok := last("crosspost[" + field + "]")
		if !ok {
			continue
		}
		switch field {
		case "server_url":
			cfg.ServerUrl = str(v)
		case "access_token":
			cfg.AccessToken = str(v)
		case "access_token_secret":
			cfg.AccessTokenSecret = str(v)
		case "client_key":
			cfg.ClientKey = str(v)
		case "client_secret":
			cfg.ClientSecret = str(v)
		case "api_key":
			cfg.ApiKey = str(v)
		case "api_key_secret":
			cfg.ApiKeySecret = str(v)
		case "app_password":
			cfg.AppPassword = str(v)
		case "username":
			cfg.Username = str(v)
		}
	}
	if v, ok := last("crosspost[max_characters]"); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if v == "" || err != nil { // ActiveModel integer typecast of junk is nil
			cfg.MaxCharacters = sql.NullInt64{}
		} else {
			cfg.MaxCharacters = sql.NullInt64{Int64: n, Valid: true}
		}
	}
	if v, present := formCheckbox(r, "crosspost[auto_fetch_comments]"); present {
		cfg.AutoFetchComments = 0
		if v {
			cfg.AutoFetchComments = 1
		}
	}
	if v, ok := last("crosspost[comment_fetch_schedule]"); ok {
		cfg.CommentFetchSchedule = str(v)
	}
	return cfg
}

// crosspostValidationErrors ports Crosspost's validations, producing Rails
// full messages in validation-declaration order (max_characters, then the
// per-platform enabled presence checks, then the server_url format).
func crosspostValidationErrors(cfg query.Crosspost) []string {
	var errs []string
	if cfg.MaxCharacters.Valid && cfg.MaxCharacters.Int64 <= 0 {
		errs = append(errs, "Max characters must be greater than 0")
	}
	if cfg.Enabled == 1 {
		blank := func(v sql.NullString) bool { return !v.Valid || v.String == "" }
		switch cfg.Platform {
		case "mastodon":
			if blank(cfg.ClientKey) {
				errs = append(errs, "Client key can't be blank")
			}
			if blank(cfg.ClientSecret) {
				errs = append(errs, "Client secret can't be blank")
			}
			if blank(cfg.AccessToken) {
				errs = append(errs, "Access token can't be blank")
			}
		case "twitter":
			if blank(cfg.AccessToken) {
				errs = append(errs, "Access token can't be blank")
			}
			if blank(cfg.AccessTokenSecret) {
				errs = append(errs, "Access token secret can't be blank")
			}
			if blank(cfg.ApiKey) {
				errs = append(errs, "Api key can't be blank")
			}
			if blank(cfg.ApiKeySecret) {
				errs = append(errs, "Api key secret can't be blank")
			}
		case "bluesky":
			if blank(cfg.Username) {
				errs = append(errs, "Username can't be blank")
			}
			if blank(cfg.AppPassword) {
				errs = append(errs, "App password can't be blank")
			}
		}
	}
	if cfg.ServerUrl.Valid && cfg.ServerUrl.String != "" {
		u, err := url.Parse(strings.TrimSpace(cfg.ServerUrl.String))
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
			errs = append(errs, "Server url must be a valid http(s) URL")
		case u.User != nil:
			errs = append(errs, "Server url must not include credentials")
		}
	}
	return errs
}

// crosspostVerify handles POST /admin/crossposts/{platform}/verify, mirroring
// #verify's JSON format: the submitted (not yet saved) credentials are
// checked through the platform's Verify. Platforms without a registered
// implementation (twitter before T21) report "not configured".
func (s *Server) crosspostVerify(w http.ResponseWriter, r *http.Request) {
	platform := chi.URLParam(r, "platform")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	result := func(status, message string) {
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "message": message})
	}

	// Rails raises on a crosspost[:platform] / params[:id] mismatch.
	if submitted := r.PostFormValue("crosspost[platform]"); submitted != platform {
		result("error", "Verification failed. Please check your settings and try again.")
		return
	}
	p := crosspost.Get(platform)
	if p == nil {
		result("error", fmt.Sprintf("Platform %s is not configured.", platform))
		return
	}

	cfg := query.Crosspost{Platform: platform}
	str := func(name string) sql.NullString {
		v := r.PostFormValue("crosspost[" + name + "]")
		return sql.NullString{String: v, Valid: v != ""}
	}
	cfg.ServerUrl = str("server_url")
	cfg.Username = str("username")
	cfg.AccessToken = str("access_token")
	cfg.AccessTokenSecret = str("access_token_secret")
	cfg.ClientKey = str("client_key")
	cfg.ClientSecret = str("client_secret")
	cfg.ApiKey = str("api_key")
	cfg.ApiKeySecret = str("api_key_secret")
	cfg.AppPassword = str("app_password")

	// The Rails controller fills in default server URLs before verifying.
	switch platform {
	case "mastodon":
		if cfg.ServerUrl.String == "" {
			cfg.ServerUrl = sql.NullString{String: "https://mastodon.social", Valid: true}
		}
	case "bluesky":
		if cfg.ServerUrl.String == "" {
			cfg.ServerUrl = sql.NullString{String: "https://bsky.social/xrpc", Valid: true}
		}
	}

	if err := p.Verify(r.Context(), cfg); err != nil {
		result("error", err.Error())
		return
	}
	result("success", "Verified Successfully!")
}

// logCrosspostActivity mirrors the ActivityLog.log! calls in
// Admin::CrosspostsController (platform/errors ride in the description).
func (s *Server) logCrosspostActivity(ctx context.Context, level, action, platform, errMsg string) {
	desc := "platform=" + platform
	if errMsg != "" {
		desc += " errors=" + activity.Quote(errMsg)
	}
	activity.Log(ctx, s.DB, level, action, "crosspost", desc)
}
