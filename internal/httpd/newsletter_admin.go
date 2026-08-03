package httpd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	newslettersvc "rables/internal/service/newsletter"
	"rables/internal/templates"
)

// RegisterNewsletterRoutes mounts the admin newsletter settings page. Rails
// routes it as resource :newsletter (GET /admin/newsletter, PATCH
// /admin/newsletter) plus collection POST /admin/newsletter/verify; the PATCH
// becomes POST here because HTML forms cannot send it. Wired into NewRouter
// by the integrator.
func RegisterNewsletterRoutes(r chi.Router, s *Server) {
	r.Route("/admin/newsletter", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.newsletterShow)
		r.Post("/", s.newsletterUpdate)
		r.Post("/verify", s.newsletterVerify)
	})
}

// NewsletterSetting loads the newsletter_settings singleton row, creating it
// on first use (NewsletterSetting.instance plus the save on write). Later
// features (T19 sender) read the config through this accessor.
func (s *Server) NewsletterSetting(ctx context.Context) (query.NewsletterSetting, error) {
	now := time.Now().Unix()
	if err := s.Q.EnsureNewsletterSetting(ctx, query.EnsureNewsletterSettingParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		return query.NewsletterSetting{}, err
	}
	return s.Q.GetNewsletterSettings(ctx)
}

// ListmonkConfig loads the listmonks singleton row, creating it on first use
// (Listmonk.first_or_initialize plus the save on write).
func (s *Server) ListmonkConfig(ctx context.Context) (query.Listmonk, error) {
	now := time.Now().Unix()
	if err := s.Q.EnsureListmonk(ctx, query.EnsureListmonkParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		return query.Listmonk{}, err
	}
	return s.Q.GetListmonk(ctx)
}

// newsletterPageData feeds admin_newsletter.html.
type newsletterPageData struct {
	Flash           templates.Flash
	Setting         query.NewsletterSetting
	Listmonk        query.Listmonk
	ActiveTab       string
	StartTLSChecked bool // Rails: smtp_enable_starttls.nil? ? true : value
	PasswordSet     bool // renders the •••••••• placeholder
	Lists           []newslettersvc.ListmonkList
	Templates       []newslettersvc.ListmonkTemplate
}

// newsletterShow renders GET /admin/newsletter, mirroring
// Admin::NewsletterController#show.
func (s *Server) newsletterShow(w http.ResponseWriter, r *http.Request) {
	st, err := s.NewsletterSetting(r.Context())
	if err != nil {
		s.Log.Error("load newsletter setting", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	lm, err := s.ListmonkConfig(r.Context())
	if err != nil {
		s.Log.Error("load listmonk config", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := s.newsletterShowData(r, newsletterTab(r.URL.Query().Get("tab")), st, lm)
	data.Flash = PopFlash(r, w)
	s.render(w, http.StatusOK, "admin_newsletter", data)
}

// newsletterShowData assembles the page data, including the best-effort
// listmonk list/template options (load_listmonk_options).
func (s *Server) newsletterShowData(r *http.Request, tab string, st query.NewsletterSetting, lm query.Listmonk) newsletterPageData {
	data := newsletterPageData{
		Setting:         st,
		Listmonk:        lm,
		ActiveTab:       tab,
		StartTLSChecked: !st.SmtpEnableStarttls.Valid || st.SmtpEnableStarttls.Int64 != 0,
		PasswordSet:     st.SmtpPassword.Valid && st.SmtpPassword.String != "",
	}
	if st.Provider == "listmonk" && lm.Url.Valid && lm.Username.Valid && lm.ApiKey.Valid {
		client := newslettersvc.ListmonkClient{URL: lm.Url.String, Username: lm.Username.String, APIKey: lm.ApiKey.String}
		if lists, err := client.FetchLists(r.Context()); err == nil {
			data.Lists = lists
		}
		if tmpls, err := client.FetchTemplates(r.Context()); err == nil {
			data.Templates = tmpls
		}
	}
	return data
}

// newsletterTab mirrors Admin::NewsletterController#newsletter_tab.
func newsletterTab(value string) string {
	switch value {
	case "general", "native", "listmonk":
		return value
	}
	return "general"
}

// newsletterUpdate handles POST /admin/newsletter, mirroring #update: forms
// carrying newsletter_setting[...] fields update the native settings (only
// the submitted keys, like the Rails partial permit+update), everything else
// updates the listmonk row.
func (s *Server) newsletterUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tab := newsletterTab(r.FormValue("tab"))
	isSetting := false
	for key := range r.PostForm {
		if strings.HasPrefix(key, "newsletter_setting[") {
			isSetting = true
			break
		}
	}
	if isSetting {
		s.updateNewsletterSetting(w, r, tab)
		return
	}
	s.updateListmonk(w, r, tab)
}

// updateNewsletterSetting overlays the submitted newsletter_setting fields on
// the current row and saves them, mirroring the model validations on failure.
func (s *Server) updateNewsletterSetting(w http.ResponseWriter, r *http.Request, tab string) {
	ctx := r.Context()
	st, err := s.NewsletterSetting(ctx)
	if err != nil {
		s.Log.Error("load newsletter setting", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	str := func(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
	last := func(name string) (string, bool) {
		values, ok := r.PostForm[name]
		if !ok || len(values) == 0 {
			return "", false
		}
		return values[len(values)-1], true
	}

	if v, ok := last("newsletter_setting[provider]"); ok {
		st.Provider = v
	}
	if v, present := formCheckbox(r, "newsletter_setting[enabled]"); present {
		if v {
			st.Enabled = 1
		} else {
			st.Enabled = 0
		}
	}
	if v, ok := last("newsletter_setting[from_email]"); ok {
		st.FromEmail = str(v)
	}
	if v, ok := last("newsletter_setting[smtp_address]"); ok {
		st.SmtpAddress = str(v)
	}
	if v, ok := last("newsletter_setting[smtp_port]"); ok {
		port, err := strconv.ParseInt(v, 10, 64)
		if v == "" || err != nil { // blank or junk typecasts to NULL, like ActiveModel
			st.SmtpPort = sql.NullInt64{}
		} else {
			st.SmtpPort = sql.NullInt64{Int64: port, Valid: true}
		}
	}
	if v, ok := last("newsletter_setting[smtp_user_name]"); ok {
		st.SmtpUserName = str(v)
	}
	if v, ok := last("newsletter_setting[smtp_password]"); ok {
		// The masked placeholder (or a blank field) keeps the stored password.
		if (v != "••••••••" && v != "") || st.SmtpPassword.String == "" {
			st.SmtpPassword = str(v)
		}
	}
	if v, ok := last("newsletter_setting[smtp_domain]"); ok {
		st.SmtpDomain = str(v)
	}
	if v, ok := last("newsletter_setting[smtp_authentication]"); ok {
		st.SmtpAuthentication = str(v)
	}
	if v, present := formCheckbox(r, "newsletter_setting[smtp_enable_starttls]"); present {
		st.SmtpEnableStarttls = sql.NullInt64{Int64: 0, Valid: true}
		if v {
			st.SmtpEnableStarttls.Int64 = 1
		}
	}

	if errs := newslettersvc.ValidateSetting(st); len(errs) > 0 {
		lm, lerr := s.ListmonkConfig(ctx)
		if lerr != nil {
			s.Log.Error("load listmonk config", "error", lerr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data := s.newsletterShowData(r, tab, st, lm)
		data.Flash = templates.Flash{Alert: strings.Join(errs, ", ")}
		s.render(w, http.StatusUnprocessableEntity, "admin_newsletter", data)
		return
	}

	if err := s.Q.UpdateNewsletterSetting(ctx, query.UpdateNewsletterSettingParams{
		Enabled:            st.Enabled,
		Provider:           st.Provider,
		FromEmail:          st.FromEmail,
		SmtpAddress:        st.SmtpAddress,
		SmtpPort:           st.SmtpPort,
		SmtpUserName:       st.SmtpUserName,
		SmtpPassword:       st.SmtpPassword,
		SmtpDomain:         st.SmtpDomain,
		SmtpAuthentication: st.SmtpAuthentication,
		SmtpEnableStarttls: st.SmtpEnableStarttls,
		UpdatedAt:          time.Now().Unix(),
	}); err != nil {
		s.Log.Error("update newsletter setting", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	SetFlash(w, templates.Flash{Notice: "Newsletter settings updated successfully."})
	http.Redirect(w, r, "/admin/newsletter?tab="+tab, http.StatusFound)
}

// updateListmonk mirrors the listmonk branch of #update.
func (s *Server) updateListmonk(w http.ResponseWriter, r *http.Request, tab string) {
	ctx := r.Context()
	lm, err := s.ListmonkConfig(ctx)
	if err != nil {
		s.Log.Error("load listmonk config", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	str := func(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
	intOrNull := func(v string) sql.NullInt64 {
		n, err := strconv.ParseInt(v, 10, 64)
		if v == "" || err != nil { // ActiveModel integer typecast of junk is nil
			return sql.NullInt64{}
		}
		return sql.NullInt64{Int64: n, Valid: true}
	}

	lm.Url = str(r.PostFormValue("listmonk[url]"))
	lm.Username = str(r.PostFormValue("listmonk[username]"))
	lm.ApiKey = str(r.PostFormValue("listmonk[api_key]"))
	lm.ListID = intOrNull(r.PostFormValue("listmonk[list_id]"))
	lm.TemplateID = intOrNull(r.PostFormValue("listmonk[template_id]"))
	if v, present := formCheckbox(r, "listmonk[enabled]"); present {
		if v {
			lm.Enabled = 1
		} else {
			lm.Enabled = 0
		}
	}

	if errs := newslettersvc.ValidateListmonk(lm); len(errs) > 0 {
		st, serr := s.NewsletterSetting(ctx)
		if serr != nil {
			s.Log.Error("load newsletter setting", "error", serr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data := s.newsletterShowData(r, tab, st, lm)
		data.Flash = templates.Flash{Alert: strings.Join(errs, ", ")}
		s.render(w, http.StatusUnprocessableEntity, "admin_newsletter", data)
		return
	}

	if err := s.Q.UpdateListmonk(ctx, query.UpdateListmonkParams{
		Url:        lm.Url,
		Username:   lm.Username,
		ApiKey:     lm.ApiKey,
		ListID:     lm.ListID,
		TemplateID: lm.TemplateID,
		Enabled:    lm.Enabled,
		UpdatedAt:  time.Now().Unix(),
	}); err != nil {
		s.Log.Error("update listmonk config", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	SetFlash(w, templates.Flash{Notice: "Newsletter settings updated successfully."})
	http.Redirect(w, r, "/admin/newsletter?tab="+tab, http.StatusFound)
}

// newsletterVerify handles POST /admin/newsletter/verify, mirroring #verify:
// the request carries SMTP fields (native check) or listmonk fields, as JSON
// (the Stimulus controller posts JSON) or form data.
func (s *Server) newsletterVerify(w http.ResponseWriter, r *http.Request) {
	params, err := newsletterVerifyParams(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if params["smtp_address"] != "" || params["smtp_user_name"] != "" {
		s.verifySMTP(w, r, params)
		return
	}
	s.verifyListmonk(w, r, params)
}

// newsletterVerifyParams reads the verify payload from a JSON body or a form.
func newsletterVerifyParams(r *http.Request) (map[string]string, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var params map[string]string
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			return nil, err
		}
		return params, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	params := make(map[string]string)
	for _, key := range []string{
		"smtp_address", "smtp_port", "smtp_user_name", "smtp_password",
		"smtp_domain", "smtp_authentication", "smtp_enable_starttls", "from_email",
		"username", "api_key", "url", "list_id", "template_id",
	} {
		params[key] = r.FormValue(key)
	}
	return params, nil
}

// verifySMTP mirrors #verify_smtp: a real dial + EHLO + STARTTLS + AUTH
// handshake (stopping before RCPT) with the Rails JSON result shape.
func (s *Server) verifySMTP(w http.ResponseWriter, r *http.Request, params map[string]string) {
	password := params["smtp_password"]
	if password == "••••••••" || password == "" {
		// Placeholder or blank falls back to the stored password.
		if st, err := s.NewsletterSetting(r.Context()); err == nil && st.SmtpPassword.Valid {
			password = st.SmtpPassword.String
		}
	}
	if params["smtp_address"] == "" || params["smtp_port"] == "" ||
		params["smtp_user_name"] == "" || password == "" || params["from_email"] == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false,
			"error":   "Please fill in all required fields",
		})
		return
	}

	domain := params["smtp_domain"]
	if domain == "" {
		if i := strings.LastIndex(params["from_email"], "@"); i >= 0 {
			domain = params["from_email"][i+1:]
		}
	}
	port, _ := strconv.Atoi(params["smtp_port"]) // junk typecasts to 0, like String#to_i
	cfg := newslettersvc.SMTPConfig{
		Address:        params["smtp_address"],
		Port:           port,
		Domain:         domain,
		UserName:       params["smtp_user_name"],
		Password:       password,
		Authentication: params["smtp_authentication"],
		EnableStartTLS: params["smtp_enable_starttls"] != "0",
		FromEmail:      params["from_email"],
	}

	err := newslettersvc.VerifySMTP(r.Context(), cfg)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"message": "SMTP configuration verified successfully!",
		})
		return
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"success": false,
		"error":   smtpVerifyErrorMessage(err),
	})
}

// smtpVerifyErrorMessage maps the handshake failure to the Rails message.
func smtpVerifyErrorMessage(err error) string {
	if errors.Is(err, newslettersvc.ErrAuthentication) {
		return "Authentication failed: Invalid credentials. Please check your username and password."
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "Connection refused. Please check your SMTP address and port settings."
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "Connection timeout. Please check your SMTP address, port and network connection."
	}
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) {
		return fmt.Sprintf("SMTP error: %s", err.Error())
	}
	return fmt.Sprintf("Verification failed: %s", err.Error())
}

// verifyListmonk mirrors #verify_listmonk against a temporary (unpersisted)
// configuration taken from the request.
func (s *Server) verifyListmonk(w http.ResponseWriter, r *http.Request, params map[string]string) {
	client := newslettersvc.ListmonkClient{
		URL:      params["url"],
		Username: params["username"],
		APIKey:   params["api_key"],
	}
	if !client.Configured() {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false,
			"error":   "Please configure all required fields first",
		})
		return
	}

	// Both fetches always run (the Rails model rescues internally), and the
	// templates call is the one whose error survives in last_error.
	lists, _ := client.FetchLists(r.Context())
	tmpls, terr := client.FetchTemplates(r.Context())

	if len(lists) > 0 && len(tmpls) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":             true,
			"lists":               lists,
			"templates":           tmpls,
			"current_list_id":     railsIntTypecast(params["list_id"]),
			"current_template_id": railsIntTypecast(params["template_id"]),
		})
		return
	}
	message := "Failed to fetch lists or templates. Please check your configuration."
	if terr != nil {
		message = terr.Error()
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"success": false,
		"error":   message,
	})
}

// railsIntTypecast mirrors the ActiveModel integer cast used when the
// temporary Listmonk is built from request params: junk becomes nil.
func railsIntTypecast(v string) any {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if v == "" || err != nil {
		return nil
	}
	return n
}
