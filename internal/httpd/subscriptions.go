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
	"slices"
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
	r.With(RateLimit(limiter, s.rateLimitKey)).Post("/subscriptions", s.subscriptionsCreate)
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
	// Must stay private: the form embeds a per-request captcha token. A
	// present flash cookie means the page renders a one-time flash, so the
	// response must not be cached (same check as publicArticleIndex).
	_, flashCookieErr := r.Cookie(flashCookieName)
	cacheControl := "private, max-age=60"
	if flashCookieErr == nil {
		cacheControl = "private, no-cache"
	}
	w.Header().Set("Cache-Control", cacheControl)
	s.render(w, http.StatusOK, "public_subscriptions", subscriptionsPageData{
		Flash:  s.PopFlash(r, w),
		Chrome: chrome,
		Form:   form,
	})
}

// subscriptionsCreate handles POST /subscriptions, mirroring
// SubscriptionsController#create (HTML and JSON formats): captcha check,
// find-or-initialize by email, resubscribe reset, tag assignment, and the
// confirmation-email job.
func (s *Server) subscriptionsCreate(w http.ResponseWriter, r *http.Request) {
	// The inline navbar/tag forms submit via fetch(FormData) (multipart),
	// the /subscriptions page posts urlencoded — parseCappedForm covers
	// both. Reads below use r.Form because it merges query string and body
	// params like Rails' params (r.PostForm would also work here:
	// ParseMultipartForm populates it too, per Go issue 9305).
	if !parseCappedForm(w, r) {
		return
	}
	// File parts in a crafted multipart request spill to disk above
	// maxMemory; this form has no file inputs, but clean up regardless.
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	// params.dig(:subscription, :email) || params[:email]; Ruby's || falls
	// back on nil (key absent), not on an empty string.
	email := r.FormValue("email")
	if _, ok := r.Form["subscription[email]"]; ok {
		email = r.Form.Get("subscription[email]")
	}
	// Normalize so a case variant lands on the existing row: the lookup
	// below and UNIQUE(email) are both case-sensitive (see NormalizeEmail).
	email = subscribersvc.NormalizeEmail(email)
	wantsJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	fail := func(message string) {
		if wantsJSON {
			writeSubscriptionJSON(w, http.StatusUnprocessableEntity, false, message)
			return
		}
		s.redirectWithFlash(w, r, "/", templates.Flash{Alert: message})
	}
	succeed := func(message string) {
		if wantsJSON {
			writeSubscriptionJSON(w, http.StatusOK, true, message)
			return
		}
		s.redirectWithFlash(w, r, "/", templates.Flash{Notice: message})
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

	if !found {
		sub, err = s.createSubscriber(ctx, email)
		if err != nil {
			s.Log.Error("create subscriber", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// The row may already exist (and even be confirmed) when a
		// concurrent request won the insert race; mirror the
		// already_subscribed? guard for that case.
		if subscribersvc.Confirmed(sub) && !sub.UnsubscribedAt.Valid {
			succeed("您已经订阅了我们的邮件列表。")
			return
		}
	}

	if sub.UnsubscribedAt.Valid {
		// Re-subscribe of an unsubscribed address (found directly, or
		// recovered from the insert race above): reset the confirmation
		// state and issue a fresh confirmation token (a new confirmation
		// email is enqueued below). Without the reset the confirmation link
		// would only set confirmed_at, leaving the address unsubscribed.
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
	} else if !subscribersvc.Confirmed(sub) && sub.ConfirmationToken.String == "" {
		// Re-subscribe of a legacy imported row with a blank confirmation
		// token (Subscriber#generate_tokens fills blanks on every save):
		// the confirmation email enqueued below must link a usable token —
		// /confirm deliberately rejects blank ones.
		newToken, err := subscribersvc.NewToken()
		if err != nil {
			s.Log.Error("generate token", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.Q.SetSubscriberConfirmationToken(ctx, query.SetSubscriberConfirmationTokenParams{
			ConfirmationToken: sql.NullString{String: newToken, Valid: true},
			UpdatedAt:         time.Now().UTC().Unix(),
			ID:                sub.ID,
		}); err != nil {
			s.Log.Error("set confirmation token", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	// Tag.where(id: tag_ids): blank entries dropped, dangling ids ignored;
	// no selection subscribes to all content (empty tag set).
	tagIDs, err := s.existingTagIDs(ctx, r.Form["subscription[tag_ids][]"])
	if err != nil {
		s.Log.Error("resolve subscriber tags", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := subscribersvc.ReplaceTags(ctx, s.DB, sub.ID, tagIDs); err != nil {
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

// createSubscriber inserts a new subscriber row. When a concurrent request
// already inserted the same address (TOCTOU against the UNIQUE email
// column: both requests saw found=false), it reloads and returns the
// existing row instead of failing with a bare 500.
func (s *Server) createSubscriber(ctx context.Context, email string) (query.Subscriber, error) {
	sub, err := subscribersvc.Create(ctx, s.Q, email, "", "")
	if err == nil {
		return sub, nil
	}
	if !isUniqueViolation(err) {
		return query.Subscriber{}, err
	}
	return s.Q.GetSubscriberByEmail(ctx, email)
}

// existingTagIDs resolves raw tag_id form values to existing tag ids,
// mirroring Tag.where(id: tag_ids): blanks are rejected first, non-numeric
// values cast to 0 (no match), dangling ids are dropped, and duplicates
// collapse — a repeated id would make ReplaceTags' second AddSubscriberTag
// hit UNIQUE(subscriber_id, tag_id) and roll the transaction back. A lookup
// failure other than ErrNoRows (a real DB error) aborts the request rather
// than silently dropping a selected tag.
func (s *Server) existingTagIDs(ctx context.Context, raw []string) ([]int64, error) {
	var ids []int64
	for _, v := range raw {
		if domain.IsBlank(v) {
			continue
		}
		id := rubyToI(v)
		if id <= 0 {
			continue
		}
		if slices.Contains(ids, id) {
			continue
		}
		if _, err := s.Q.GetTagByID(ctx, id); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
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
	data := subscriptionConfirmData{Chrome: chrome}
	token := r.URL.Query().Get("token")
	// A tokenless request must never resolve a subscriber: imports preserve
	// legacy token columns verbatim, so a row whose stored token is the empty
	// string would otherwise match. Generated tokens are always non-empty.
	var sub query.Subscriber
	err = sql.ErrNoRows
	if token != "" {
		sub, err = s.Q.GetSubscriberByConfirmationToken(ctx, sql.NullString{String: token, Valid: true})
	}
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
	// Token-bound result page: keep it private and short-lived. A present
	// flash cookie means the page renders a one-time flash, so the response
	// must not be cached (same check as publicArticleIndex). Set only after
	// every fallible query, or http.Error would send a cached 500.
	_, flashCookieErr := r.Cookie(flashCookieName)
	cacheControl := "private, max-age=60"
	if flashCookieErr == nil {
		cacheControl = "private, no-cache"
	}
	w.Header().Set("Cache-Control", cacheControl)
	// PopFlash runs only after every fallible query: its clearing Set-Cookie
	// would otherwise go out with a 500 and destroy the unseen flash.
	data.Flash = s.PopFlash(r, w)
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
	data := unsubscribeConfirmData{Chrome: chrome, Token: r.URL.Query().Get("token")}
	// See subscriptionConfirm: a tokenless request must not resolve a row.
	var sub query.Subscriber
	err = sql.ErrNoRows
	if data.Token != "" {
		sub, err = s.Q.GetSubscriberByUnsubscribeToken(ctx, sql.NullString{String: data.Token, Valid: true})
	}
	switch {
	case err == nil:
		data.Found, data.Email = true, sub.Email
	case errors.Is(err, sql.ErrNoRows):
	default:
		s.Log.Error("find subscriber by unsubscribe token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Token-bound result page: keep it private and short-lived. A present
	// flash cookie means the page renders a one-time flash, so the response
	// must not be cached (same check as publicArticleIndex). Set only after
	// every fallible query, or http.Error would send a cached 500.
	_, flashCookieErr := r.Cookie(flashCookieName)
	cacheControl := "private, max-age=60"
	if flashCookieErr == nil {
		cacheControl = "private, no-cache"
	}
	w.Header().Set("Cache-Control", cacheControl)
	// PopFlash runs only after every fallible query: its clearing Set-Cookie
	// would otherwise go out with a 500 and destroy the unseen flash.
	data.Flash = s.PopFlash(r, w)
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
	// Same capped form parse as subscriptionsCreate (the form has only text
	// fields).
	if !parseCappedForm(w, r) {
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	ctx := r.Context()
	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	data := unsubscribeResultData{Chrome: chrome}
	// See subscriptionConfirm: a tokenless request must not resolve a row.
	token := r.FormValue("token")
	var sub query.Subscriber
	err = sql.ErrNoRows
	if token != "" {
		sub, err = s.Q.GetSubscriberByUnsubscribeToken(ctx, sql.NullString{String: token, Valid: true})
	}
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
	// Token-bound result page: keep it private and short-lived. A present
	// flash cookie means the page renders a one-time flash, so the response
	// must not be cached (same check as publicArticleIndex). Set only after
	// every fallible query, or http.Error would send a cached 500.
	_, flashCookieErr := r.Cookie(flashCookieName)
	cacheControl := "private, max-age=60"
	if flashCookieErr == nil {
		cacheControl = "private, no-cache"
	}
	w.Header().Set("Cache-Control", cacheControl)
	// PopFlash runs only after every fallible query: its clearing Set-Cookie
	// would otherwise go out with a 500 and destroy the unseen flash.
	data.Flash = s.PopFlash(r, w)
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
