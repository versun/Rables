package httpd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
// DELETE to POST /admin/redirects/{id}/destroy. The Go port adds
// POST /admin/redirects/reorder for the drag-and-drop ordering UI.
func RegisterRedirectsRoutes(r chi.Router, s *Server) {
	r.Route("/admin/redirects", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminRedirectsIndex)
		r.Get("/new", s.adminRedirectsNew)
		r.Post("/", s.adminRedirectsCreate)
		r.Post("/reorder", s.adminRedirectsReorder)
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

// adminRedirectsIndex renders GET /admin/redirects in evaluation order
// (position, then id): the first matching rule wins.
func (s *Server) adminRedirectsIndex(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListRedirects(r.Context())
	if err != nil {
		s.Log.Error("list redirects", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "admin_redirects_index", adminRedirectsIndexData{
		Flash:     s.PopFlash(r, w),
		Redirects: rows,
	})
}

// adminRedirectFormData feeds admin_redirects_new.html and
// admin_redirects_edit.html.
type adminRedirectFormData struct {
	Flash    templates.Flash
	Redirect query.Redirect
	Kind     string   // form kind: "path", "host" or "regex"
	Host     string   // host-kind input (raw on errors, split from match_from on edit)
	Path     string   // path input of the path/host kinds (same sourcing as Host)
	Errors   []string // validation messages, shown like the Rails form-errors block
}

// adminRedirectsNew renders GET /admin/redirects/new. New records default to
// enabled, like the Rails form's checkbox, and to the path kind.
func (s *Server) adminRedirectsNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "admin_redirects_new", adminRedirectFormData{
		Flash:    s.PopFlash(r, w),
		Redirect: query.Redirect{Enabled: 1},
		Kind:     "path",
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
			fmt.Sprintf("%s errors=%s", redirectLogMatch(in.Redirect), activity.Quote(strings.Join(errs, ", "))))
		s.render(w, http.StatusUnprocessableEntity, "admin_redirects_new", adminRedirectFormData{
			Redirect: in.Redirect,
			Kind:     in.kind,
			Host:     in.host,
			Path:     in.path,
			Errors:   errs,
		})
		return
	}
	now := time.Now().Unix()
	redirect, err := s.Q.CreateRedirect(r.Context(), query.CreateRedirectParams{
		Regex:       in.Regex,
		Replacement: in.Replacement,
		MatchFrom:   in.MatchFrom,
		MatchPrefix: in.MatchPrefix,
		MatchOn:     in.MatchOn,
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
		fmt.Sprintf("%s replacement=%s", redirectLogMatch(redirect), activity.Quote(redirect.Replacement)))
	s.InvalidateRedirectCache()
	s.SetFlash(w, templates.Flash{Notice: "Redirect was successfully created."})
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
	kind, host, path := redirectFormSplit(redirect)
	s.render(w, http.StatusOK, "admin_redirects_edit", adminRedirectFormData{
		Flash:    s.PopFlash(r, w),
		Redirect: redirect,
		Kind:     kind,
		Host:     host,
		Path:     path,
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
	existing, err := s.Q.GetRedirectByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.Log.Error("get redirect", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	in := redirectFromForm(r)
	in.ID = id
	// A regex rule re-saved with its pattern untouched keeps its stored match
	// subject: re-deriving it would silently reclassify a legacy path rule
	// like "posts/(\d+)" (no leading "/") to host+path, where the
	// whole-subject anchoring can never match. Editing the pattern re-derives
	// the subject as documented in the form.
	if in.kind == "regex" && existing.MatchOn != redirectMatchSimple && existing.Regex == in.Regex {
		in.MatchOn = existing.MatchOn
	}
	if errs := validateRedirectForm(in); len(errs) > 0 {
		activity.Log(r.Context(), s.DB, "error", "failed", "redirect",
			fmt.Sprintf("%s errors=%s", redirectLogMatch(in.Redirect), activity.Quote(strings.Join(errs, ", "))))
		s.render(w, http.StatusUnprocessableEntity, "admin_redirects_edit", adminRedirectFormData{
			Redirect: in.Redirect,
			Kind:     in.kind,
			Host:     in.host,
			Path:     in.path,
			Errors:   errs,
		})
		return
	}
	if err := s.Q.UpdateRedirect(r.Context(), query.UpdateRedirectParams{
		Regex:       in.Regex,
		Replacement: in.Replacement,
		MatchFrom:   in.MatchFrom,
		MatchPrefix: in.MatchPrefix,
		MatchOn:     in.MatchOn,
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
		fmt.Sprintf("%s replacement=%s", redirectLogMatch(in.Redirect), activity.Quote(in.Replacement)))
	s.InvalidateRedirectCache()
	s.SetFlash(w, templates.Flash{Notice: "Redirect was successfully updated."})
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
		fmt.Sprintf("%s replacement=%s", redirectLogMatch(redirect), activity.Quote(redirect.Replacement)))
	s.InvalidateRedirectCache()
	s.SetFlash(w, templates.Flash{Notice: "Redirect was successfully deleted."})
	http.Redirect(w, r, "/admin/redirects", http.StatusSeeOther)
}

// adminRedirectsReorder handles POST /admin/redirects/reorder: the
// drag-and-drop UI posts the full id list in its new visual order and
// positions are renumbered from 1. Unknown ids update zero rows and are
// ignored, so a stale tab cannot corrupt the order of rules it never saw —
// except for the gap it leaves, which the next reorder closes.
func (s *Server) adminRedirectsReorder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.Log.Error("reorder redirects", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	q := s.Q.WithTx(tx)
	for i, id := range in.IDs {
		if err := q.SetRedirectPosition(r.Context(), query.SetRedirectPositionParams{
			Position: int64(i + 1),
			ID:       id,
		}); err != nil {
			s.Log.Error("reorder redirects", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.Log.Error("reorder redirects", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	activity.Log(r.Context(), s.DB, "info", "reordered", "redirect",
		fmt.Sprintf("count=%d", len(in.IDs)))
	s.InvalidateRedirectCache()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"success":true}`))
}

// redirectIDParam reads the {id} path parameter; an unparsable id yields 0,
// which matches no redirect, like Redirect.find raising RecordNotFound.
func redirectIDParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id
}

// redirectFormValues is one parsed redirect form: the synthesized Redirect
// ready to persist, plus the raw kind/host/path inputs. The raw inputs are
// kept because the stored match_from loses the host/path split, which the
// validation messages and the re-rendered form need.
type redirectFormValues struct {
	query.Redirect
	kind string // "path", "host" or "regex"
	host string // host-kind input as typed (trimmed)
	path string // path/host_path input as typed (trimmed)
}

// redirectFromForm reads the permitted redirect params. kind selects the mode:
// "path" and "host" build a simple rule (match_from/match_prefix/replacement,
// regex cleared), "regex" fills regex/replacement and derives the match subject
// from the pattern. Anything unexpected (including an absent kind) is a path
// rule.
func redirectFromForm(r *http.Request) redirectFormValues {
	in := redirectFormValues{
		Redirect: query.Redirect{
			Replacement: r.FormValue("replacement"),
			Permanent:   checkboxInt(r, "permanent"),
			Enabled:     checkboxInt(r, "enabled"),
		},
		kind: r.FormValue("kind"),
	}
	switch in.kind {
	case "regex":
		in.Regex = r.FormValue("regex")
		in.MatchOn = detectRedirectMatchOn(in.Regex)
		return in
	case "host":
		in.host = strings.TrimSpace(r.FormValue("host"))
		in.path = strings.TrimSpace(r.FormValue("host_path"))
		in.MatchOn = redirectMatchSimple
		in.MatchFrom = normalizeMatchFrom(joinHostPath(in.host, in.path))
	default: // "path" and anything unexpected
		in.kind = "path"
		in.path = strings.TrimSpace(r.FormValue("path"))
		in.MatchOn = redirectMatchSimple
		in.MatchFrom = in.path
	}
	if r.FormValue("match_mode") == "prefix" {
		in.MatchPrefix = 1
	}
	return in
}

// joinHostPath combines the host kind's two inputs into one match_from string:
// the bare host when the optional path is blank.
func joinHostPath(host, path string) string {
	if path == "" {
		return host
	}
	return strings.TrimSuffix(host, "/") + "/" + strings.TrimPrefix(path, "/")
}

// redirectFormSplit derives the form kind and the host/path inputs of a stored
// redirect: simple rules split on the leading-"/" convention (a path rule's
// match_from is the path; a host rule's splits at the first "/"), everything
// else edits as a regex rule.
func redirectFormSplit(red query.Redirect) (kind, host, path string) {
	if red.MatchOn == redirectMatchSimple {
		if strings.HasPrefix(red.MatchFrom, "/") {
			return "path", "", red.MatchFrom
		}
		h, p, found := strings.Cut(red.MatchFrom, "/")
		if !found {
			return "host", red.MatchFrom, ""
		}
		return "host", h, "/" + p
	}
	return "regex", "", ""
}

// redirectLogMatch is the activity-log subject of one redirect write: a simple
// rule is identified by its match_from (its regex is always empty), a regex
// rule by its pattern.
func redirectLogMatch(in query.Redirect) string {
	if in.MatchOn == redirectMatchSimple {
		return "match_from=" + activity.Quote(in.MatchFrom)
	}
	return "regex=" + activity.Quote(in.Regex)
}

// normalizeMatchFrom trims surrounding spaces and normalizes the host part of
// a simple rule's match string the way redirectRequestHost normalizes the
// request Host (lowercased, port stripped, IPv6 brackets removed, no trailing
// root dot), so a saved rule matches what the middleware sees. URL paths are
// case-sensitive and kept as entered.
func normalizeMatchFrom(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "/") {
		return v
	}
	host, path, found := strings.Cut(v, "/")
	// An empty port ("host:") is left alone — SplitHostPort accepts it, and
	// stripping it would quietly turn "https://x" into "https//x".
	// validateRedirectForm rejects the dangling colon instead.
	if h, p, err := net.SplitHostPort(host); err == nil && p != "" {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !found {
		return host
	}
	return host + "/" + path
}

// detectRedirectMatchOn infers a regex rule's subject from the pattern:
// starting with "/" means the URL path on any host; anything else matches
// host+path. Paths always begin with "/" and hosts never contain one, so the
// two never collide. Leading "^" anchors, inline flags ("(?i)") and opening
// group prefixes ("(", "(?:", "(?P<name>") are unwrapped first, so "(?i)/old"
// and "(?:/a|/b)" are still path rules.
func detectRedirectMatchOn(pattern string) string {
	p := pattern
	for {
		if strings.HasPrefix(p, "^") {
			p = p[1:]
			continue
		}
		if rest, ok := unwrapGroupPrefix(p); ok {
			p = rest
			continue
		}
		break
	}
	if strings.HasPrefix(p, "/") {
		return redirectMatchPath
	}
	return redirectMatchHostPath
}

// unwrapGroupPrefix strips one leading group opener from p: "(", "(?:",
// "(?=", "(?!", "(?P<name>"/"(?<name>", "(?flags)" and "(?flags:...)". ok is
// false when p does not start with an unwrappable group.
func unwrapGroupPrefix(p string) (rest string, ok bool) {
	if !strings.HasPrefix(p, "(") {
		return "", false
	}
	if !strings.HasPrefix(p, "(?") {
		return p[1:], true
	}
	for _, prefix := range []string{"(?:", "(?=", "(?!"} {
		if rest := strings.TrimPrefix(p, prefix); rest != p {
			return rest, true
		}
	}
	for _, prefix := range []string{"(?P<", "(?<"} {
		if rest := strings.TrimPrefix(p, prefix); rest != p {
			if i := strings.IndexByte(rest, '>'); i > 0 {
				return rest[i+1:], true
			}
			return "", false
		}
	}
	// "(?flags)" or "(?flags:...)".
	rest = p[2:]
	if i := strings.IndexAny(rest, "):"); i > 0 && isInlineFlags(rest[:i]) {
		return rest[i+1:], true
	}
	return "", false
}

// isInlineFlags reports whether s is a valid RE2 inline-flag list ("i", "m",
// "s", "U", with optional "-" toggles).
func isInlineFlags(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 'i', 'm', 's', 'U', '-':
		default:
			return false
		}
	}
	return true
}

// validSimpleRedirectTarget constrains a simple rule's target to a site path
// or an absolute http(s) URL. A "//host" value is rejected: browsers resolve
// that protocol-relative Location off-site, which the "path or absolute URL"
// contract does not intend.
func validSimpleRedirectTarget(t string) bool {
	if strings.HasPrefix(t, "//") {
		return false
	}
	return strings.HasPrefix(t, "/") ||
		strings.HasPrefix(t, "http://") ||
		strings.HasPrefix(t, "https://")
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

// validateRedirectForm mirrors the Redirect validations: presence first, then
// per-kind shape checks — a regex must compile, a host must be a plain host
// name, a path must start with "/", and a simple target must be a path or
// absolute URL (a bare word like "tags/blog" would emit a broken relative
// Location). Messages name the form field the admin sees.
func validateRedirectForm(in redirectFormValues) []string {
	var errs []string
	if in.kind == "regex" {
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
	if in.kind == "host" {
		if in.host == "" {
			errs = append(errs, "Host can't be blank")
		} else if strings.ContainsAny(in.host, " \t") || strings.Contains(in.host, "://") ||
			strings.Contains(in.host, "/") || strings.HasSuffix(in.host, ":") {
			// A trailing bare colon is an empty port ("host:"), which
			// normalizeMatchFrom deliberately keeps (see its comment): the
			// rule would never match, so reject it here instead. IPv6 hosts
			// ("::1") do not end in a colon and pass.
			errs = append(errs, "Host must be a plain host name, like blog.example.com (no scheme, path or port)")
		}
	}
	if in.path == "" {
		if in.kind == "path" {
			errs = append(errs, "Path can't be blank")
		}
	} else if !strings.HasPrefix(in.path, "/") {
		errs = append(errs, "Path must start with /")
	} else if strings.ContainsAny(in.path, " \t") {
		errs = append(errs, "Path must not contain spaces")
	}
	if strings.TrimSpace(in.Replacement) == "" {
		errs = append(errs, "Replacement can't be blank")
	} else if !validSimpleRedirectTarget(in.Replacement) {
		errs = append(errs, "Replacement must start with / or be an absolute URL (http:// or https://)")
	}
	return errs
}
