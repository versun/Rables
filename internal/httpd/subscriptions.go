package httpd

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	"rables/internal/service/captcha"
	subscribersvc "rables/internal/service/subscribers"
	"rables/internal/templates"
)

// subscriptionCreateBurst mirrors the subscription submission budget:
// rate_limit to: 5, within: 1.hour (plan section 4.6).
const subscriptionCreateBurst = 5

// newsletterConfirmationPayload is the exact job payload contract consumed by
// the newsletter_confirmation worker (T19): {"subscriber_id": <id>}.
type newsletterConfirmationPayload struct {
	SubscriberID int64 `json:"subscriber_id"`
}

// RegisterSubscriptionRoutes mounts the public subscription endpoints,
// mirroring routes.rb: resources :subscriptions (index/create) plus the
// confirm/unsubscribe token endpoints. GET /unsubscribe only renders the
// confirmation page; the actual unsubscribe is POST-only so link scanners
// cannot unsubscribe users. Wired into NewRouter by the integrator.
func RegisterSubscriptionRoutes(r chi.Router, s *Server) {
	limiter := NewIPRateLimiter(rate.Every(12*time.Minute), subscriptionCreateBurst)
	r.Get("/subscriptions", s.subscriptionsIndex)
	r.With(RateLimit(limiter, ClientIP)).Post("/subscriptions", s.subscriptionsCreate)
	r.Get("/confirm", s.subscriptionConfirm)
	r.Get("/unsubscribe", s.subscriptionUnsubscribeForm)
	r.Post("/unsubscribe", s.subscriptionUnsubscribe)
}

// subscribeFormData feeds the "subscribe_form" partial. It covers the three
// Rails variants: the /subscriptions page form (tag checkboxes, plain POST),
// the navbar form on the index page, and the tag-page inline form (hidden
// preselected tag) — the latter two render only when the native newsletter
// is enabled and carry the async newsletter-subscription hooks (T28).
type subscribeFormData struct {
	Show        bool // false renders nothing (newsletter disabled / not home)
	Inline      bool // navbar/tag variants: async newsletter-subscription hooks
	Tags        []query.Tag
	HiddenTagID int64
	Placeholder string
	AnswerID    string // unique captcha label target (Rails uses SecureRandom.hex(6))
	Question    string
	A           int
	B           int
	Op          string
	Token       string
}

// issueSubscribeForm fills the captcha fields of a form data value.
func (s *Server) issueSubscribeForm(data subscribeFormData) subscribeFormData {
	challenge, token := captcha.New(s.Cfg.HMACSecret, captcha.TTL).IssueChallenge()
	data.Question, data.Token = challenge.Question, token
	data.A, data.B, data.Op = challenge.A, challenge.B, challenge.Op
	data.AnswerID = randomHex(6)
	return data
}

// randomHex mirrors SecureRandom.hex(n): n random bytes hex-encoded.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

// subscribePageForm builds the /subscriptions page form (always shown, tag
// checkboxes from Tag.alphabetical).
func (s *Server) subscribePageForm(ctx context.Context) (subscribeFormData, error) {
	tags, err := s.Q.ListPublicTags(ctx)
	if err != nil {
		return subscribeFormData{}, err
	}
	return s.issueSubscribeForm(subscribeFormData{
		Show:        true,
		Tags:        tags,
		Placeholder: "输入您的邮箱地址",
	}), nil
}

// subscribeInlineForm builds the navbar (hiddenTagID 0) or tag-page inline
// form. It renders only when the native newsletter is enabled, mirroring the
// Rails newsletter_setting[:enabled] && newsletter_setting[:native] gate.
func (s *Server) subscribeInlineForm(ctx context.Context, hiddenTagID int64) subscribeFormData {
	placeholder := "通过邮件订阅更新"
	if hiddenTagID > 0 {
		placeholder = "通过邮件订阅该标签的更新"
	}
	data := subscribeFormData{Inline: true, HiddenTagID: hiddenTagID, Placeholder: placeholder}
	ns, err := s.Q.GetNewsletterSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return data // no row: first_or_initialize defaults to disabled
	}
	if err != nil {
		s.Log.Error("load newsletter settings", "error", err)
		return data
	}
	if ns.Enabled != 1 || ns.Provider != "native" {
		return data
	}
	data.Show = true
	return s.issueSubscribeForm(data)
}

// subscriptionsPageData feeds public_subscriptions.html.
type subscriptionsPageData struct {
	Flash  templates.Flash
	Chrome siteChrome
	Form   subscribeFormData
}

// subscriptionsIndex renders GET /subscriptions, mirroring
// SubscriptionsController#index.
func (s *Server) subscriptionsIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	form, err := s.subscribePageForm(ctx)
	if err != nil {
		s.listError(w, "list tags", err)
		return
	}
	s.render(w, http.StatusOK, "public_subscriptions", subscriptionsPageData{
		Flash:  PopFlash(r, w),
		Chrome: chrome,
		Form:   form,
	})
}

// subscriptionsCreate handles POST /subscriptions, mirroring
// SubscriptionsController#create (HTML and JSON formats): captcha check,
// find-or-initialize by email, resubscribe reset, tag assignment, and the
// confirmation-email job.
func (s *Server) subscriptionsCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// params.dig(:subscription, :email) || params[:email]; Ruby's || falls
	// back on nil (key absent), not on an empty string.
	email := r.FormValue("email")
	if _, ok := r.PostForm["subscription[email]"]; ok {
		email = r.PostForm.Get("subscription[email]")
	}
	wantsJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	fail := func(message string) {
		if wantsJSON {
			writeSubscriptionJSON(w, http.StatusUnprocessableEntity, false, message)
			return
		}
		SetFlash(w, templates.Flash{Alert: message})
		http.Redirect(w, r, "/", http.StatusFound)
	}
	succeed := func(message string) {
		if wantsJSON {
			writeSubscriptionJSON(w, http.StatusOK, true, message)
			return
		}
		SetFlash(w, templates.Flash{Notice: message})
		http.Redirect(w, r, "/", http.StatusFound)
	}

	if domain.IsBlank(email) {
		fail("请输入有效的邮箱地址。")
		return
	}

	cap := captcha.New(s.Cfg.HMACSecret, captcha.TTL)
	token, answer := r.FormValue("captcha[token]"), r.FormValue("captcha[answer]")
	if _, ok := cap.Expected(token); !ok {
		fail("验证已过期：请刷新页面后重新回答数学题。")
		return
	}
	if !cap.Verify(token, answer) {
		fail("验证失败：请回答数学题。")
		return
	}

	ctx := r.Context()
	sub, err := s.Q.GetSubscriberByEmail(ctx, email)
	found := true
	switch {
	case errors.Is(err, sql.ErrNoRows):
		found = false
	case err != nil:
		s.Log.Error("find subscriber", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if found && subscribersvc.Confirmed(sub) && !sub.UnsubscribedAt.Valid {
		// already_subscribed?: tags and confirmation state stay untouched.
		succeed("您已经订阅了我们的邮件列表。")
		return
	}

	// Subscriber.save validations (presence was checked above); an existing
	// row always passes, but a freshly submitted address may not.
	if !subscribersvc.ValidEmail(email) {
		s.logSubscriptionActivity(ctx, "failed", "subscription", 2,
			fmt.Sprintf("email=%s errors=%s", activityQuote(email), activityQuote("Email is invalid")))
		fail("Email is invalid")
		return
	}

	if found && sub.UnsubscribedAt.Valid {
		// Re-subscribe of an unsubscribed address: reset the confirmation
		// state and issue a fresh confirmation token (a new confirmation
		// email is enqueued below).
		newToken, err := subscribersvc.NewToken()
		if err != nil {
			s.Log.Error("generate token", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.Q.ResetSubscriberForResubscribe(ctx, query.ResetSubscriberForResubscribeParams{
			ConfirmationToken: sql.NullString{String: newToken, Valid: true},
			UpdatedAt:         time.Now().UTC().Unix(),
			ID:                sub.ID,
		}); err != nil {
			s.Log.Error("reset subscriber", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	if !found {
		sub, err = subscribersvc.Create(ctx, s.Q, email, "", "")
		if err != nil {
			s.Log.Error("create subscriber", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	// Tag.where(id: tag_ids): blank entries dropped, dangling ids ignored;
	// no selection subscribes to all content (empty tag set).
	tagIDs := s.existingTagIDs(ctx, r.PostForm["subscription[tag_ids][]"])
	if err := subscribersvc.ReplaceTags(ctx, s.Q, sub.ID, tagIDs); err != nil {
		s.Log.Error("replace subscriber tags", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if _, err := s.Enqueuer().Enqueue(ctx, jobs.KindNewsletterConfirmation,
		newsletterConfirmationPayload{SubscriberID: sub.ID}, time.Now()); err != nil {
		s.Log.Error("enqueue confirmation email", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.logSubscriptionActivity(ctx, "created", "subscription", 0,
		fmt.Sprintf("email=%s tags=[%s]", activityQuote(email), joinIDs(tagIDs)))
	succeed("订阅成功！请检查您的邮箱并点击确认链接。")
}

// existingTagIDs resolves raw tag_id form values to existing tag ids,
// mirroring Tag.where(id: tag_ids): blanks are rejected first, non-numeric
// values cast to 0 (no match), and dangling ids are dropped.
func (s *Server) existingTagIDs(ctx context.Context, raw []string) []int64 {
	var ids []int64
	for _, v := range raw {
		if domain.IsBlank(v) {
			continue
		}
		id := rubyToI(v)
		if id <= 0 {
			continue
		}
		if _, err := s.Q.GetTagByID(ctx, id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// writeSubscriptionJSON renders the {success, message} contract consumed by
// the newsletter-subscription JS (T28).
func writeSubscriptionJSON(w http.ResponseWriter, status int, success bool, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": success, "message": message})
}

// joinIDs mirrors ActivityLog.format_value for an integer array ("1,2").
func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// subscriptionConfirmData feeds public_subscribe_confirm.html.
type subscriptionConfirmData struct {
	Flash   templates.Flash
	Chrome  siteChrome
	Success bool
	Message string
}

// subscriptionConfirm renders GET /confirm?token=, mirroring
// SubscriptionsController#confirm: a found, unconfirmed token confirms the
// subscriber; anything else renders the failure page (always 200).
func (s *Server) subscriptionConfirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	data := subscriptionConfirmData{Flash: PopFlash(r, w), Chrome: chrome}
	token := r.URL.Query().Get("token")
	sub, err := s.Q.GetSubscriberByConfirmationToken(ctx, sql.NullString{String: token, Valid: true})
	switch {
	case err == nil:
		data.Success = true
		if subscribersvc.Confirmed(sub) {
			data.Message = "您的邮箱已经确认过了。"
		} else {
			if err := s.Q.ConfirmSubscriber(ctx, query.ConfirmSubscriberParams{
				ConfirmedAt: sql.NullInt64{Int64: time.Now().UTC().Unix(), Valid: true},
				UpdatedAt:   time.Now().UTC().Unix(),
				ID:          sub.ID,
			}); err != nil {
				s.Log.Error("confirm subscriber", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			s.logSubscriptionActivity(ctx, "confirmed", "subscription", 0, "email="+activityQuote(sub.Email))
			data.Message = "订阅确认成功！"
		}
	case errors.Is(err, sql.ErrNoRows):
		// invalid link: the failure page renders below
	default:
		s.Log.Error("find subscriber by confirmation token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "public_subscribe_confirm", data)
}

// unsubscribeConfirmData feeds public_unsubscribe_confirm.html.
type unsubscribeConfirmData struct {
	Flash  templates.Flash
	Chrome siteChrome
	Found  bool
	Email  string
	Token  string
}

// subscriptionUnsubscribeForm renders GET /unsubscribe?token=: the
// confirmation page only — the actual unsubscribe requires POST.
func (s *Server) subscriptionUnsubscribeForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	data := unsubscribeConfirmData{Flash: PopFlash(r, w), Chrome: chrome, Token: r.URL.Query().Get("token")}
	sub, err := s.Q.GetSubscriberByUnsubscribeToken(ctx, sql.NullString{String: data.Token, Valid: true})
	switch {
	case err == nil:
		data.Found, data.Email = true, sub.Email
	case errors.Is(err, sql.ErrNoRows):
	default:
		s.Log.Error("find subscriber by unsubscribe token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "public_unsubscribe_confirm", data)
}

// unsubscribeResultData feeds public_unsubscribe.html.
type unsubscribeResultData struct {
	Flash   templates.Flash
	Chrome  siteChrome
	Success bool
}

// subscriptionUnsubscribe handles POST /unsubscribe, mirroring the POST
// branch of SubscriptionsController#unsubscribe.
func (s *Server) subscriptionUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	data := unsubscribeResultData{Flash: PopFlash(r, w), Chrome: chrome}
	sub, err := s.Q.GetSubscriberByUnsubscribeToken(ctx, sql.NullString{String: r.FormValue("token"), Valid: true})
	switch {
	case err == nil:
		// Subscriber#unsubscribe! is a no-op when already unsubscribed.
		if !sub.UnsubscribedAt.Valid {
			if err := s.Q.UnsubscribeSubscriber(ctx, query.UnsubscribeSubscriberParams{
				UnsubscribedAt: sql.NullInt64{Int64: time.Now().UTC().Unix(), Valid: true},
				UpdatedAt:      time.Now().UTC().Unix(),
				ID:             sub.ID,
			}); err != nil {
				s.Log.Error("unsubscribe subscriber", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		s.logSubscriptionActivity(ctx, "unsubscribed", "subscription", 0, "email="+activityQuote(sub.Email))
		data.Success = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		s.Log.Error("find subscriber by unsubscribe token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "public_unsubscribe", data)
}

// logSubscriptionActivity mirrors the ActivityLog.log! calls of the
// subscription flows; like the Rails original it never breaks the main flow.
// It writes raw SQL because the activity-logs feature belongs to a later
// task (same interim pattern as logTagActivity).
func (s *Server) logSubscriptionActivity(ctx context.Context, action, target string, level int64, description string) {
	now := time.Now().Unix()
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO activity_logs (level, action, target, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		level, action, target, description, now, now)
	if err != nil {
		s.Log.Warn("activity log", "error", err)
	}
}
