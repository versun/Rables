package httpd

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/jobs"
	"rables/internal/service/captcha"
	subscribersvc "rables/internal/service/subscribers"
	"rables/internal/templates"
)

// newSubscriptionTestServer mounts the subscription routes (plus the public
// pages carrying the inline form) on a test-local chi router.
func newSubscriptionTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	renderer, err := templates.New()
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := NewServer(database, config.Config{Addr: ":8080", HMACSecret: "x"}, logger, renderer)
	r := chi.NewRouter()
	RegisterSubscriptionRoutes(r, s)
	RegisterSubscriberAdminRoutes(r, s)
	RegisterPublicRoutes(r, s)
	RegisterArticleRoutes(r, s)
	return s, r
}

// validSubscriptionForm returns a subscription submission with a fresh,
// correctly answered captcha for the test server secret ("x").
func validSubscriptionForm(t *testing.T, email string, extra url.Values) url.Values {
	t.Helper()
	cap := captcha.New("x", captcha.TTL)
	_, token := cap.Issue()
	expected, ok := cap.Expected(token)
	if !ok {
		t.Fatal("fresh captcha token rejected")
	}
	form := url.Values{
		"subscription[email]": {email},
		"captcha[token]":      {token},
		"captcha[answer]":     {strconv.Itoa(expected)},
	}
	for k, vs := range extra {
		form[k] = vs
	}
	return form
}

// insertSubscriber stores a subscriber directly in the given state.
func insertSubscriber(t *testing.T, s *Server, email string, confirmed, unsubscribed bool) query.Subscriber {
	t.Helper()
	sub, err := subscribersvc.Create(t.Context(), s.Q, email, "", "")
	if err != nil {
		t.Fatalf("create subscriber: %v", err)
	}
	now := time.Now().UTC().Unix()
	if confirmed {
		if err := s.Q.ConfirmSubscriber(t.Context(), query.ConfirmSubscriberParams{
			ConfirmedAt: sql.NullInt64{Int64: now, Valid: true}, UpdatedAt: now, ID: sub.ID,
		}); err != nil {
			t.Fatalf("confirm subscriber: %v", err)
		}
	}
	if unsubscribed {
		if err := s.Q.UnsubscribeSubscriber(t.Context(), query.UnsubscribeSubscriberParams{
			UnsubscribedAt: sql.NullInt64{Int64: now, Valid: true}, UpdatedAt: now, ID: sub.ID,
		}); err != nil {
			t.Fatalf("unsubscribe subscriber: %v", err)
		}
	}
	sub, err = s.Q.GetSubscriberByEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("reload subscriber: %v", err)
	}
	return sub
}

// jobRunRows dumps the queued jobs as kind+payload pairs.
func jobRunRows(t *testing.T, s *Server) [][2]string {
	t.Helper()
	rows, err := s.DB.Query("SELECT kind, COALESCE(payload, '') FROM job_runs ORDER BY id")
	if err != nil {
		t.Fatalf("query job_runs: %v", err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			t.Fatalf("scan job_run: %v", err)
		}
		out = append(out, pair)
	}
	return out
}

func subscriberCount(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM subscribers").Scan(&n); err != nil {
		t.Fatalf("count subscribers: %v", err)
	}
	return n
}

// insertTagRow stores a tag directly and returns it.
func insertTagRow(t *testing.T, s *Server, name string) query.Tag {
	t.Helper()
	res, err := s.DB.Exec("INSERT INTO tags (name, slug, created_at, updated_at) VALUES (?, ?, 1000, 1000)",
		name, strings.ToLower(name))
	if err != nil {
		t.Fatalf("insert tag: %v", err)
	}
	id, _ := res.LastInsertId()
	return query.Tag{ID: id, Name: name, Slug: strings.ToLower(name)}
}

func subscriberTagIDs(t *testing.T, s *Server, subscriberID int64) []int64 {
	t.Helper()
	rows, err := s.DB.Query("SELECT tag_id FROM subscriber_tags WHERE subscriber_id = ? ORDER BY tag_id", subscriberID)
	if err != nil {
		t.Fatalf("query subscriber tags: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func TestSubscriptionCreate(t *testing.T) {
	s, h := newSubscriptionTestServer(t)

	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "new@example.com", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("status = %d location = %q, want 302 /", rec.Code, rec.Header().Get("Location"))
	}
	if flash := readFlash(t, rec); flash.Notice != "订阅成功！请检查您的邮箱并点击确认链接。" {
		t.Errorf("notice = %q", flash.Notice)
	}

	sub, err := s.Q.GetSubscriberByEmail(t.Context(), "new@example.com")
	if err != nil {
		t.Fatalf("subscriber not stored: %v", err)
	}
	// Tokens are generated at creation; the subscriber starts unconfirmed.
	if !sub.ConfirmationToken.Valid || len(sub.ConfirmationToken.String) != 43 {
		t.Errorf("confirmation_token = %+v, want a 43-char token", sub.ConfirmationToken)
	}
	if !sub.UnsubscribeToken.Valid || len(sub.UnsubscribeToken.String) != 43 {
		t.Errorf("unsubscribe_token = %+v, want a 43-char token", sub.UnsubscribeToken)
	}
	if sub.ConfirmedAt.Valid || sub.UnsubscribedAt.Valid {
		t.Errorf("new subscriber should be pending, got confirmed_at=%v unsubscribed_at=%v", sub.ConfirmedAt, sub.UnsubscribedAt)
	}

	// The confirmation email is enqueued with the exact T19 payload contract.
	runs := jobRunRows(t, s)
	if len(runs) != 1 || runs[0][0] != jobs.KindNewsletterConfirmation {
		t.Fatalf("job_runs = %v, want one newsletter_confirmation", runs)
	}
	wantPayload := `{"subscriber_id":` + strconv.FormatInt(sub.ID, 10) + `}`
	if runs[0][1] != wantPayload {
		t.Errorf("payload = %q, want %q", runs[0][1], wantPayload)
	}
}

func TestSubscriptionCreateBlankEmail(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	for _, form := range []url.Values{
		validSubscriptionForm(t, "", nil),
		validSubscriptionForm(t, "   ", nil),
	} {
		rec := doRequest(t, h, http.MethodPost, "/subscriptions", form)
		if flash := readFlash(t, rec); flash.Alert != "请输入有效的邮箱地址。" {
			t.Errorf("alert = %q", flash.Alert)
		}
	}
	if got := subscriberCount(t, s); got != 0 {
		t.Errorf("subscribers = %d, want 0", got)
	}
}

func TestSubscriptionCreateCaptchaFailures(t *testing.T) {
	s, h := newSubscriptionTestServer(t)

	wrong := validSubscriptionForm(t, "a@example.com", nil)
	cap := captcha.New("x", captcha.TTL)
	expected, _ := cap.Expected(wrong.Get("captcha[token]"))
	wrong.Set("captcha[answer]", strconv.Itoa(expected+1))

	missingToken := validSubscriptionForm(t, "a@example.com", nil)
	missingToken.Del("captcha[token]")

	tests := []struct {
		name      string
		form      url.Values
		wantAlert string
	}{
		{"wrong answer", wrong, "验证失败：请回答数学题。"},
		{"missing token", missingToken, "验证已过期：请刷新页面后重新回答数学题。"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, h, http.MethodPost, "/subscriptions", tt.form)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
				t.Fatalf("status = %d, want 302 /", rec.Code)
			}
			if flash := readFlash(t, rec); flash.Alert != tt.wantAlert {
				t.Errorf("alert = %q, want %q", flash.Alert, tt.wantAlert)
			}
			if got := subscriberCount(t, s); got != 0 {
				t.Errorf("subscriber stored despite bad captcha")
			}
		})
	}
}

func TestSubscriptionCreateInvalidEmail(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "not-an-email", nil))
	if flash := readFlash(t, rec); flash.Alert != "Email is invalid" {
		t.Errorf("alert = %q, want %q", flash.Alert, "Email is invalid")
	}
	if got := subscriberCount(t, s); got != 0 {
		t.Errorf("subscribers = %d, want 0", got)
	}
	if runs := jobRunRows(t, s); len(runs) != 0 {
		t.Errorf("jobs enqueued for invalid email: %v", runs)
	}
}

func TestSubscriptionCreateRateLimited(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	for i := 1; i <= 5; i++ {
		email := "user" + strconv.Itoa(i) + "@example.com"
		rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, email, nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("request %d: status = %d, want 302", i, rec.Code)
		}
	}
	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "user6@example.com", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th request: status = %d, want 429", rec.Code)
	}
	if got := subscriberCount(t, s); got != 5 {
		t.Errorf("subscribers = %d, want 5", got)
	}
}

func TestSubscriptionCreateJSON(t *testing.T) {
	_, h := newSubscriptionTestServer(t)
	post := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/subscriptions", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := post(validSubscriptionForm(t, "json@example.com", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if !body.Success || body.Message != "订阅成功！请检查您的邮箱并点击确认链接。" {
		t.Errorf("json = %+v", body)
	}

	rec = post(validSubscriptionForm(t, "bad", nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if body.Success || body.Message != "Email is invalid" {
		t.Errorf("json = %+v", body)
	}
}

func TestSubscriptionCreateTagAssignment(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	tagA := insertTagRow(t, s, "Go")
	tagB := insertTagRow(t, s, "Rails")

	// Blanks, dangling ids and non-numeric values are dropped (Tag.where(id:)).
	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "tagged@example.com", url.Values{
		"subscription[tag_ids][]": {strconv.FormatInt(tagA.ID, 10), strconv.FormatInt(tagB.ID, 10), "99999", "abc", ""},
	}))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	sub, err := s.Q.GetSubscriberByEmail(t.Context(), "tagged@example.com")
	if err != nil {
		t.Fatalf("subscriber not stored: %v", err)
	}
	if got := subscriberTagIDs(t, s, sub.ID); len(got) != 2 || got[0] != tagA.ID || got[1] != tagB.ID {
		t.Errorf("tags = %v, want [%d %d]", got, tagA.ID, tagB.ID)
	}

	// No tag selection subscribes to all content (empty tag set).
	rec = doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "all@example.com", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	all, err := s.Q.GetSubscriberByEmail(t.Context(), "all@example.com")
	if err != nil {
		t.Fatalf("subscriber not stored: %v", err)
	}
	if got := subscriberTagIDs(t, s, all.ID); len(got) != 0 {
		t.Errorf("tags = %v, want empty (all content)", got)
	}
}

func TestSubscriptionCreateAlreadySubscribed(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	tagA := insertTagRow(t, s, "Go")
	sub := insertSubscriber(t, s, "active@example.com", true, false)
	if err := subscribersvc.ReplaceTags(t.Context(), s.Q, sub.ID, []int64{tagA.ID}); err != nil {
		t.Fatalf("replace tags: %v", err)
	}

	// Re-submitting with a different tag selection changes nothing.
	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "active@example.com", nil))
	if flash := readFlash(t, rec); flash.Notice != "您已经订阅了我们的邮件列表。" {
		t.Errorf("notice = %q", flash.Notice)
	}
	if got := subscriberTagIDs(t, s, sub.ID); len(got) != 1 || got[0] != tagA.ID {
		t.Errorf("tags = %v, want unchanged [%d]", got, tagA.ID)
	}
	if runs := jobRunRows(t, s); len(runs) != 0 {
		t.Errorf("confirmation email re-sent for an active subscriber: %v", runs)
	}
}

func TestSubscriptionCreatePendingResubmit(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	tagA := insertTagRow(t, s, "Go")

	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "pending@example.com", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("first submit: status = %d", rec.Code)
	}
	// A pending subscriber re-submitting gets fresh tags and a new
	// confirmation email; the tokens are kept.
	rec = doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "pending@example.com", url.Values{
		"subscription[tag_ids][]": {strconv.FormatInt(tagA.ID, 10)},
	}))
	if flash := readFlash(t, rec); flash.Notice != "订阅成功！请检查您的邮箱并点击确认链接。" {
		t.Errorf("notice = %q", flash.Notice)
	}
	sub, err := s.Q.GetSubscriberByEmail(t.Context(), "pending@example.com")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if sub.ConfirmedAt.Valid {
		t.Error("pending subscriber got confirmed by re-submission")
	}
	if got := subscriberTagIDs(t, s, sub.ID); len(got) != 1 || got[0] != tagA.ID {
		t.Errorf("tags = %v, want [%d]", got, tagA.ID)
	}
	if runs := jobRunRows(t, s); len(runs) != 2 {
		t.Errorf("jobs = %v, want 2 confirmation emails", runs)
	}
}

func TestSubscriptionResubscribeAfterUnsubscribe(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	old := insertSubscriber(t, s, "gone@example.com", true, true)

	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "gone@example.com", nil))
	if flash := readFlash(t, rec); flash.Notice != "订阅成功！请检查您的邮箱并点击确认链接。" {
		t.Fatalf("notice = %q", flash.Notice)
	}
	sub, err := s.Q.GetSubscriberByEmail(t.Context(), "gone@example.com")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Confirmation state resets; a fresh confirmation token is issued, while
	// the unsubscribe token is kept (old unsubscribe links stay valid).
	if sub.ConfirmedAt.Valid || sub.UnsubscribedAt.Valid {
		t.Errorf("confirmation state not reset: confirmed=%v unsubscribed=%v", sub.ConfirmedAt, sub.UnsubscribedAt)
	}
	if sub.ConfirmationToken.String == old.ConfirmationToken.String {
		t.Error("confirmation_token was not regenerated")
	}
	if sub.UnsubscribeToken.String != old.UnsubscribeToken.String {
		t.Error("unsubscribe_token should be preserved")
	}
	if runs := jobRunRows(t, s); len(runs) != 1 || runs[0][0] != jobs.KindNewsletterConfirmation {
		t.Errorf("jobs = %v, want one newsletter_confirmation", runs)
	}

	// The old confirmation link is dead; the new one confirms.
	rec = doRequest(t, h, http.MethodGet, "/confirm?token="+old.ConfirmationToken.String, nil)
	if !strings.Contains(rec.Body.String(), "订阅确认失败") {
		t.Error("old confirmation token should be invalid after resubscribe")
	}
	rec = doRequest(t, h, http.MethodGet, "/confirm?token="+sub.ConfirmationToken.String, nil)
	if !strings.Contains(rec.Body.String(), "订阅确认成功") {
		t.Error("new confirmation token should confirm")
	}
}

func TestSubscriptionConfirm(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	sub := insertSubscriber(t, s, "confirm@example.com", false, false)

	rec := doRequest(t, h, http.MethodGet, "/confirm?token="+sub.ConfirmationToken.String, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "订阅确认成功") {
		t.Fatalf("status = %d, want 200 with success page", rec.Code)
	}
	reloaded, _ := s.Q.GetSubscriberByEmail(t.Context(), "confirm@example.com")
	if !reloaded.ConfirmedAt.Valid {
		t.Error("confirmed_at not set")
	}

	// Confirming again reports the address as already confirmed.
	rec = doRequest(t, h, http.MethodGet, "/confirm?token="+sub.ConfirmationToken.String, nil)
	if !strings.Contains(rec.Body.String(), "您的邮箱已经确认过了。") {
		t.Error("second confirm should report already-confirmed")
	}

	// An unknown token renders the failure page with a 200.
	rec = doRequest(t, h, http.MethodGet, "/confirm?token=nope", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "订阅确认失败") {
		t.Errorf("status = %d, want 200 with failure page", rec.Code)
	}
}

func TestUnsubscribeGetVsPost(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	sub := insertSubscriber(t, s, "bye@example.com", true, false)

	// GET only renders the confirmation page (link scanners must not
	// unsubscribe anyone).
	rec := doRequest(t, h, http.MethodGet, "/unsubscribe?token="+sub.UnsubscribeToken.String, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "确认取消订阅") || !strings.Contains(body, `action="/unsubscribe"`) {
		t.Error("GET should render the unsubscribe confirmation form")
	}
	reloaded, _ := s.Q.GetSubscriberByEmail(t.Context(), "bye@example.com")
	if reloaded.UnsubscribedAt.Valid {
		t.Error("GET /unsubscribe changed the subscription state")
	}

	// POST performs the unsubscribe.
	rec = doRequest(t, h, http.MethodPost, "/unsubscribe", url.Values{"token": {sub.UnsubscribeToken.String}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "取消订阅成功") {
		t.Fatalf("POST status = %d, want 200 with success page", rec.Code)
	}
	reloaded, _ = s.Q.GetSubscriberByEmail(t.Context(), "bye@example.com")
	if !reloaded.UnsubscribedAt.Valid {
		t.Error("POST /unsubscribe did not set unsubscribed_at")
	}

	// Unknown tokens render the failure page on both verbs.
	rec = doRequest(t, h, http.MethodGet, "/unsubscribe?token=nope", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "取消订阅失败") {
		t.Errorf("GET invalid: status = %d, want 200 with failure page", rec.Code)
	}
	rec = doRequest(t, h, http.MethodPost, "/unsubscribe", url.Values{"token": {"nope"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "取消订阅失败") {
		t.Errorf("POST invalid: status = %d, want 200 with failure page", rec.Code)
	}
}

func TestSubscriptionsIndexPage(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	insertTagRow(t, s, "Go")

	rec := doRequest(t, h, http.MethodGet, "/subscriptions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="subscription[email]"`, `name="captcha[token]"`,
		`name="subscription[tag_ids][]"`, ">Go</label>", `action="/subscriptions"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("subscriptions page missing %s", want)
		}
	}
}

// TestInlineSubscribeFormGate covers the navbar/tag-page form visibility:
// only the native+enabled newsletter shows the form, and only on the pages
// the Rails views gate it to.
func TestInlineSubscribeFormGate(t *testing.T) {
	setNewsletter := func(t *testing.T, s *Server, enabled int, provider string) {
		t.Helper()
		if _, err := s.DB.Exec("DELETE FROM newsletter_settings"); err != nil {
			t.Fatalf("clear newsletter settings: %v", err)
		}
		if _, err := s.DB.Exec(
			"INSERT INTO newsletter_settings (id, enabled, provider, created_at, updated_at) VALUES (1, ?, ?, 1000, 1000)",
			enabled, provider); err != nil {
			t.Fatalf("insert newsletter settings: %v", err)
		}
	}

	t.Run("no settings row hides the form", func(t *testing.T) {
		_, h := newSubscriptionTestServer(t)
		rec := doRequest(t, h, http.MethodGet, "/", nil)
		if strings.Contains(rec.Body.String(), "newsletter-subscription") {
			t.Error("form rendered without newsletter settings")
		}
	})

	t.Run("listmonk provider hides the form", func(t *testing.T) {
		s, h := newSubscriptionTestServer(t)
		setNewsletter(t, s, 1, "listmonk")
		rec := doRequest(t, h, http.MethodGet, "/", nil)
		if strings.Contains(rec.Body.String(), "newsletter-subscription") {
			t.Error("form rendered for the listmonk provider")
		}
	})

	t.Run("native enabled shows the navbar form", func(t *testing.T) {
		s, h := newSubscriptionTestServer(t)
		setNewsletter(t, s, 1, "native")
		rec := doRequest(t, h, http.MethodGet, "/", nil)
		body := rec.Body.String()
		if !strings.Contains(body, "newsletter-subscription") || !strings.Contains(body, `name="subscription[email]"`) {
			t.Error("navbar form not rendered on /")
		}
		if !strings.Contains(body, `name="captcha[token]"`) {
			t.Error("navbar form misses the captcha token")
		}
	})

	t.Run("tag page preselects its tag", func(t *testing.T) {
		s, h := newSubscriptionTestServer(t)
		setNewsletter(t, s, 1, "native")
		tag := insertTagRow(t, s, "Go")
		rec := doRequest(t, h, http.MethodGet, "/tags/"+tag.Slug, nil)
		want := `name="subscription[tag_ids][]" value="` + strconv.FormatInt(tag.ID, 10) + `"`
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("tag page form missing hidden tag input %q", want)
		}
	})

	t.Run("tag page hides the form when disabled", func(t *testing.T) {
		s, h := newSubscriptionTestServer(t)
		tag := insertTagRow(t, s, "Go")
		rec := doRequest(t, h, http.MethodGet, "/tags/"+tag.Slug, nil)
		if strings.Contains(rec.Body.String(), "newsletter-subscription") {
			t.Error("tag page form rendered with the newsletter disabled")
		}
	})
}
