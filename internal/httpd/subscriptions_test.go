package httpd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/domain"
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
	r.Use(stripTrailingSlash)
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
	if rec.Code != http.StatusFound || locationPath(t, rec) != "/" {
		t.Fatalf("status = %d location = %q, want 302 /", rec.Code, rec.Header().Get("Location"))
	}
	// The redirect target carries the cache-busting parameter so a browser
	// holding a fresh cached copy of the index cannot serve the redirect
	// from its cache and swallow the flash.
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/?_=") {
		t.Errorf("location = %q, want the cache-busting ?_= parameter", loc)
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
			if rec.Code != http.StatusFound || locationPath(t, rec) != "/" {
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

// TestSubscriptionCreateMultipart posts the way the inline navbar/tag forms
// do: fetch(FormData) sends multipart/form-data. ParseForm alone never
// parses a multipart body, so a regression made every async subscribe fail
// with "请输入有效的邮箱地址。".
func TestSubscriptionCreateMultipart(t *testing.T) {
	s, h := newSubscriptionTestServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for key, values := range validSubscriptionForm(t, "multi@example.com", nil) {
		for _, v := range values {
			if err := mw.WriteField(key, v); err != nil {
				t.Fatalf("write multipart field: %v", err)
			}
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/subscriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if !body.Success {
		t.Errorf("json = %+v, want success", body)
	}
	if _, err := s.Q.GetSubscriberByEmail(t.Context(), "multi@example.com"); err != nil {
		t.Errorf("subscriber not stored from multipart submission: %v", err)
	}
}

// TestSubscriptionCreateMultipartMixedCaseContentType: MIME types are
// case-insensitive (RFC 2045), so a mixed-case Multipart/Form-Data header
// must still take the multipart branch — a case-sensitive dispatch fell
// through to ParseForm, which never reads a multipart body, and the
// submission failed validation on the "missing" email field.
func TestSubscriptionCreateMultipartMixedCaseContentType(t *testing.T) {
	s, h := newSubscriptionTestServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for key, values := range validSubscriptionForm(t, "mixedcase@example.com", nil) {
		for _, v := range values {
			if err := mw.WriteField(key, v); err != nil {
				t.Fatalf("write multipart field: %v", err)
			}
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/subscriptions", &buf)
	req.Header.Set("Content-Type", "Multipart/Form-Data; boundary="+mw.Boundary())
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if !body.Success {
		t.Errorf("json = %+v, want success", body)
	}
	if _, err := s.Q.GetSubscriberByEmail(t.Context(), "mixedcase@example.com"); err != nil {
		t.Errorf("subscriber not stored from mixed-case multipart submission: %v", err)
	}
}

// TestSubscriptionCreateBodyTooLarge: the subscription form carries only text
// fields, so the whole request body is capped (http.MaxBytesReader, same as
// the media upload). An oversized multipart body is rejected with 413
// instead of spilling file parts into os.TempDir().
func TestSubscriptionCreateBodyTooLarge(t *testing.T) {
	_, h := newSubscriptionTestServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for key, values := range validSubscriptionForm(t, "big@example.com", nil) {
		for _, v := range values {
			if err := mw.WriteField(key, v); err != nil {
				t.Fatalf("write multipart field: %v", err)
			}
		}
	}
	if err := mw.WriteField("note", strings.Repeat("a", 2<<20)); err != nil {
		t.Fatalf("write multipart field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/subscriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// TestSubscriptionCreateUrlencodedBodyTooLarge: the /subscriptions page posts
// urlencoded, and the 1 MB cap must fire there too — ParseMultipartForm used
// to return ErrNotMultipart for such bodies while discarding the
// *http.MaxBytesError its internal ParseForm produced, so the handler ran on
// query-only params instead of rejecting with 413.
func TestSubscriptionCreateUrlencodedBodyTooLarge(t *testing.T) {
	_, h := newSubscriptionTestServer(t)

	rec := doRequest(t, h, http.MethodPost, "/subscriptions", url.Values{
		"subscription[email]": {"big@example.com"},
		"note":                {strings.Repeat("a", 2<<20)},
	})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// TestSubscriptionUnsubscribeBodyTooLarge: POST /unsubscribe carries only a
// token, so the body is capped like subscriptionsCreate. An oversized
// multipart body is rejected with 413 and spills nothing into os.TempDir().
func TestSubscriptionUnsubscribeBodyTooLarge(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)
	_, h := newSubscriptionTestServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("token", "abc"); err != nil {
		t.Fatalf("write multipart field: %v", err)
	}
	fw, err := mw.CreateFormFile("file", "big.bin")
	if err != nil {
		t.Fatalf("create multipart file: %v", err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("a"), 2<<20)); err != nil {
		t.Fatalf("write multipart file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/unsubscribe", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

// TestSubscriptionUnsubscribeUrlencodedBodyTooLarge: same cap for the
// urlencoded unsubscribe form.
func TestSubscriptionUnsubscribeUrlencodedBodyTooLarge(t *testing.T) {
	_, h := newSubscriptionTestServer(t)

	rec := doRequest(t, h, http.MethodPost, "/unsubscribe", url.Values{
		"token": {"abc"},
		"note":  {strings.Repeat("a", 2<<20)},
	})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
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

// TestSubscriptionCreateDuplicateTagIDs: a crafted form can repeat
// subscription[tag_ids][] values. Without dedupe, ReplaceTags' second
// AddSubscriberTag hits UNIQUE(subscriber_id, tag_id) and rolls back, and a
// new subscriber is left with the row committed but no confirmation email.
func TestSubscriptionCreateDuplicateTagIDs(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	tagA := insertTagRow(t, s, "Go")
	tagB := insertTagRow(t, s, "Rails")

	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "dup@example.com", url.Values{
		"subscription[tag_ids][]": {strconv.FormatInt(tagA.ID, 10), strconv.FormatInt(tagA.ID, 10), strconv.FormatInt(tagB.ID, 10)},
	}))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	sub, err := s.Q.GetSubscriberByEmail(t.Context(), "dup@example.com")
	if err != nil {
		t.Fatalf("subscriber not stored: %v", err)
	}
	if got := subscriberTagIDs(t, s, sub.ID); len(got) != 2 || got[0] != tagA.ID || got[1] != tagB.ID {
		t.Errorf("tags = %v, want [%d %d]", got, tagA.ID, tagB.ID)
	}
	if runs := jobRunRows(t, s); len(runs) != 1 {
		t.Errorf("jobs = %v, want 1 confirmation email", runs)
	}
}

// TestExistingTagIDsDBError: a dangling id is dropped (ErrNoRows), but a
// real DB failure (here: the tags table is gone) propagates instead of
// silently dropping every selected tag.
func TestExistingTagIDsDBError(t *testing.T) {
	s, _ := newSubscriptionTestServer(t)
	tagA := insertTagRow(t, s, "Go")

	ids, err := s.existingTagIDs(t.Context(), []string{strconv.FormatInt(tagA.ID, 10), "99999"})
	if err != nil {
		t.Fatalf("existingTagIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != tagA.ID {
		t.Errorf("ids = %v, want [%d]", ids, tagA.ID)
	}

	if _, err := s.DB.Exec(`DROP TABLE tags`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := s.existingTagIDs(t.Context(), []string{strconv.FormatInt(tagA.ID, 10)}); err == nil {
		t.Error("existingTagIDs swallowed a DB error")
	}
}

// TestSubscriptionCreateTagLookupError: when the tag lookup fails with a real
// DB error, the subscription aborts with a 500 and no confirmation email —
// the user must not get a success flash for a subscription that lost its
// tags.
func TestSubscriptionCreateTagLookupError(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	tagA := insertTagRow(t, s, "Go")
	if _, err := s.DB.Exec(`DROP TABLE tags`); err != nil {
		t.Fatalf("drop table: %v", err)
	}

	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "unlucky@example.com", url.Values{
		"subscription[tag_ids][]": {strconv.FormatInt(tagA.ID, 10)},
	}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if runs := jobRunRows(t, s); len(runs) != 0 {
		t.Errorf("confirmation email sent despite the tag lookup failure: %v", runs)
	}
}

func TestSubscriptionCreateAlreadySubscribed(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	tagA := insertTagRow(t, s, "Go")
	sub := insertSubscriber(t, s, "active@example.com", true, false)
	if err := subscribersvc.ReplaceTags(t.Context(), s.DB, sub.ID, []int64{tagA.ID}); err != nil {
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

// TestSubscriptionCreateCaseVariantAlreadySubscribed: SQLite matches email
// with the BINARY collation, so without normalization a case variant of an
// existing address would slip past the already-subscribed guard and insert a
// duplicate row.
func TestSubscriptionCreateCaseVariantAlreadySubscribed(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	insertSubscriber(t, s, "case@example.com", true, false)

	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, " Case@Example.COM ", nil))
	if flash := readFlash(t, rec); flash.Notice != "您已经订阅了我们的邮件列表。" {
		t.Errorf("notice = %q", flash.Notice)
	}
	if got := subscriberCount(t, s); got != 1 {
		t.Errorf("subscribers = %d, want 1 (no duplicate row)", got)
	}
	if runs := jobRunRows(t, s); len(runs) != 0 {
		t.Errorf("confirmation email re-sent for an active subscriber: %v", runs)
	}
}

// TestSubscriptionCreateLegacyMixedCaseEmail: rows imported verbatim from a
// Rails dump can hold mixed-case emails. The lookup must match them
// case-insensitively, otherwise a re-subscription under any other case would
// slip past the guard and insert a duplicate row.
func TestSubscriptionCreateLegacyMixedCaseEmail(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	now := time.Now().UTC().Unix()
	// Insert a legacy row verbatim (bypassing subscribersvc.Create's
	// normalization), as a Rails import would have produced it.
	if _, err := s.DB.ExecContext(t.Context(),
		`INSERT INTO subscribers (email, confirmation_token, unsubscribe_token, confirmed_at, created_at, updated_at)
		 VALUES ('Legacy@Example.COM', 'ctok', 'utok', ?, ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert legacy subscriber: %v", err)
	}

	rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, "legacy@example.com", nil))
	if flash := readFlash(t, rec); flash.Notice != "您已经订阅了我们的邮件列表。" {
		t.Errorf("notice = %q", flash.Notice)
	}
	if got := subscriberCount(t, s); got != 1 {
		t.Errorf("subscribers = %d, want 1 (no duplicate row)", got)
	}
}

// TestGetSubscriberByEmailPrefersExactMatch: the email UNIQUE index uses the
// BINARY collation, so a legacy mixed-case import and a normalized row for
// the same address can coexist. The NOCASE lookup matches both, and without
// the exact-match ordering it would return whichever row the scan hits first
// (the older mixed-case row) — the already-subscribed guard and the
// confirmation/reset emails would then act on the stale row.
func TestGetSubscriberByEmailPrefersExactMatch(t *testing.T) {
	s, _ := newSubscriptionTestServer(t)
	now := time.Now().UTC().Unix()
	// Insert both rows verbatim (bypassing subscribersvc.Create's
	// normalization), the legacy mixed-case row first so it holds the
	// smaller rowid a plain NOCASE scan would return.
	if _, err := s.DB.ExecContext(t.Context(),
		`INSERT INTO subscribers (email, confirmation_token, unsubscribe_token, created_at, updated_at)
		 VALUES ('Exact@Example.COM', 'ctok1', 'utok1', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert legacy subscriber: %v", err)
	}
	if _, err := s.DB.ExecContext(t.Context(),
		`INSERT INTO subscribers (email, confirmation_token, unsubscribe_token, created_at, updated_at)
		 VALUES ('exact@example.com', 'ctok2', 'utok2', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert exact subscriber: %v", err)
	}

	sub, err := s.Q.GetSubscriberByEmail(t.Context(), "exact@example.com")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if sub.Email != "exact@example.com" {
		t.Errorf("email = %q, want the exact-case row", sub.Email)
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

// TestCreateSubscriberRace covers the TOCTOU recovery: when a concurrent
// request wins the insert race, the UNIQUE violation on the email column
// resolves to the existing row instead of a bare 500. The second direct
// call deterministically hits the conflict path.
func TestCreateSubscriberRace(t *testing.T) {
	s, _ := newSubscriptionTestServer(t)

	first, err := s.createSubscriber(t.Context(), "race@example.com")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := s.createSubscriber(t.Context(), "race@example.com")
	if err != nil {
		t.Fatalf("racing create: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("racing create returned subscriber %d, want existing row %d", second.ID, first.ID)
	}
	if got := subscriberCount(t, s); got != 1 {
		t.Errorf("subscribers = %d, want 1 (no duplicate row)", got)
	}
}

// TestSubscriptionCreateRaceRecoversUnsubscribed covers the insert-race
// recovery when the recovered row is confirmed but unsubscribed (e.g. a
// concurrent import inserted it that way): like the direct re-subscribe
// path, the confirmation state must reset and a fresh confirmation email
// must go out — ConfirmSubscriber alone would leave unsubscribed_at set.
// The race is staged by inserting the row inside an open transaction: the
// handler's lookup misses the uncommitted row (found=false) while its own
// insert blocks until the commit reveals the UNIQUE conflict.
func TestSubscriptionCreateRaceRecoversUnsubscribed(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	form := validSubscriptionForm(t, "imported@example.com", nil)

	now := time.Now().UTC().Unix()
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatalf("begin holding transaction: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO subscribers (email, confirmation_token, unsubscribe_token, confirmed_at, unsubscribed_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"imported@example.com", "old-confirmation-token", "old-unsubscribe-token", now, now, now, now); err != nil {
		t.Fatalf("insert uncommitted subscriber: %v", err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doRequest(t, h, http.MethodPost, "/subscriptions", form)
	}()
	// Give the handler time to pass its lookup before the commit unblocks
	// its insert into a UNIQUE violation. (If the lookup ever ran after the
	// commit, the direct re-subscribe path would produce the same state, so
	// the assertions below hold either way.)
	time.Sleep(500 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit holding transaction: %v", err)
	}
	rec := <-done

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if flash := readFlash(t, rec); flash.Notice != "订阅成功！请检查您的邮箱并点击确认链接。" {
		t.Fatalf("notice = %q", flash.Notice)
	}
	sub, err := s.Q.GetSubscriberByEmail(t.Context(), "imported@example.com")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Confirmation state resets; a fresh confirmation token is issued, while
	// the unsubscribe token is kept (old unsubscribe links stay valid).
	if sub.ConfirmedAt.Valid || sub.UnsubscribedAt.Valid {
		t.Errorf("confirmation state not reset: confirmed=%v unsubscribed=%v", sub.ConfirmedAt, sub.UnsubscribedAt)
	}
	if sub.ConfirmationToken.String == "old-confirmation-token" {
		t.Error("confirmation_token was not regenerated")
	}
	if sub.UnsubscribeToken.String != "old-unsubscribe-token" {
		t.Error("unsubscribe_token should be preserved")
	}
	if runs := jobRunRows(t, s); len(runs) != 1 || runs[0][0] != jobs.KindNewsletterConfirmation {
		t.Errorf("jobs = %v, want one newsletter_confirmation", runs)
	}
	if got := subscriberCount(t, s); got != 1 {
		t.Errorf("subscribers = %d, want 1 (no duplicate row)", got)
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

// TestSubscriptionResubscribeLegacyTokenless: an imported legacy row may
// carry a blank confirmation token (imports keep token columns verbatim).
// Re-subscribing must mint one — otherwise the confirmation email links an
// empty token that /confirm deliberately rejects, and the address could
// never confirm.
func TestSubscriptionResubscribeLegacyTokenless(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	now := time.Now().UTC().Unix()
	// Cover both legacy shapes: empty string and NULL.
	for i, token := range []any{"", nil} {
		email := "legacy" + strconv.Itoa(i) + "@example.com"
		unsubscribeToken := "unsubscribe-token-" + strconv.Itoa(i)
		if _, err := s.DB.Exec(
			`INSERT INTO subscribers (email, confirmation_token, unsubscribe_token, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)`, email, token, unsubscribeToken, now, now); err != nil {
			t.Fatalf("insert legacy subscriber: %v", err)
		}

		rec := doRequest(t, h, http.MethodPost, "/subscriptions", validSubscriptionForm(t, email, nil))
		if flash := readFlash(t, rec); flash.Notice != "订阅成功！请检查您的邮箱并点击确认链接。" {
			t.Fatalf("token %v: notice = %q", token, flash.Notice)
		}
		sub, err := s.Q.GetSubscriberByEmail(t.Context(), email)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if sub.ConfirmationToken.String == "" {
			t.Fatalf("token %v: confirmation_token was not minted", token)
		}
		if sub.UnsubscribeToken.String != unsubscribeToken {
			t.Errorf("token %v: unsubscribe_token should be preserved", token)
		}
		// The confirmation link from the email confirms the subscription.
		rec = doRequest(t, h, http.MethodGet, "/confirm?token="+sub.ConfirmationToken.String, nil)
		if !strings.Contains(rec.Body.String(), "订阅确认成功") {
			t.Errorf("token %v: minted confirmation token should confirm", token)
		}
	}
	if runs := jobRunRows(t, s); len(runs) != 2 {
		t.Errorf("jobs = %v, want 2 confirmation emails", runs)
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

// TestSubscriptionPagesCacheControlFlash: the subscription pages embed a
// per-request captcha token or render a token-bound result, so they stay
// private with a short max-age; a response that renders a one-time flash
// degrades to no-cache (same contract as the article pages).
func TestSubscriptionPagesCacheControlFlash(t *testing.T) {
	s, h := newSubscriptionTestServer(t)

	flashCookie := signedFlashCookie(t, s, templates.Flash{Notice: "subscribed"})

	for _, tc := range []struct {
		method, path string
		form         url.Values
	}{
		{http.MethodGet, "/subscriptions", nil},
		{http.MethodGet, "/confirm", nil},
		{http.MethodGet, "/unsubscribe", nil},
		{http.MethodPost, "/unsubscribe", url.Values{"token": {"nope"}}},
	} {
		rec := doRequest(t, h, tc.method, tc.path, tc.form)
		if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=60" {
			t.Errorf("%s %s Cache-Control = %q, want private, max-age=60", tc.method, tc.path, cc)
		}
		rec = doRequest(t, h, tc.method, tc.path, tc.form, flashCookie)
		if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache" {
			t.Errorf("%s %s Cache-Control with flash = %q, want private, no-cache", tc.method, tc.path, cc)
		}
	}
}

// TestSubscriptionPagesErrorNotCached: Cache-Control is set only after every
// fallible query, because http.Error does not clear headers already set — a
// 500 carrying a cacheable header would be stored by a CDN.
func TestSubscriptionPagesErrorNotCached(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	if _, err := s.DB.Exec(`DROP TABLE subscribers`); err != nil {
		t.Fatalf("drop table: %v", err)
	}

	for _, tc := range []struct {
		method, path string
		form         url.Values
	}{
		{http.MethodGet, "/confirm?token=abc", nil},
		{http.MethodGet, "/unsubscribe?token=abc", nil},
		{http.MethodPost, "/unsubscribe", url.Values{"token": {"abc"}}},
	} {
		rec := doRequest(t, h, tc.method, tc.path, tc.form)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s status = %d, want 500", tc.method, tc.path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "" {
			t.Errorf("%s %s Cache-Control = %q, want empty on a 500", tc.method, tc.path, cc)
		}
	}
}

// TestSubscriptionPagesErrorKeepsFlash: PopFlash runs only after every
// fallible query, so a 500 response must not carry the clearing Set-Cookie —
// the one-time flash survives to render on the next successful page.
func TestSubscriptionPagesErrorKeepsFlash(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	if _, err := s.DB.Exec(`DROP TABLE subscribers`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	flashCookie := signedFlashCookie(t, s, templates.Flash{Notice: "subscribed"})

	for _, tc := range []struct {
		method, path string
		form         url.Values
	}{
		{http.MethodGet, "/confirm?token=abc", nil},
		{http.MethodGet, "/unsubscribe?token=abc", nil},
		{http.MethodPost, "/unsubscribe", url.Values{"token": {"abc"}}},
	} {
		rec := doRequest(t, h, tc.method, tc.path, tc.form, flashCookie)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s status = %d, want 500", tc.method, tc.path, rec.Code)
		}
		if cleared := findCookie(rec, flashCookieName); cleared != nil {
			t.Errorf("%s %s cleared the flash cookie on a 500: %+v", tc.method, tc.path, cleared)
		}
	}
}

// TestTokenlessConfirmAndUnsubscribe: a request without a token must never
// resolve a subscriber — imports keep legacy token columns verbatim, so a
// row whose stored token is the empty string would otherwise match a
// tokenless lookup (leaking the email, or confirming/unsubscribing it).
func TestTokenlessConfirmAndUnsubscribe(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	now := time.Now().UTC().Unix()
	if _, err := s.DB.Exec(
		`INSERT INTO subscribers (email, confirmation_token, unsubscribe_token, created_at, updated_at)
		 VALUES (?, '', '', ?, ?)`, "legacy@example.com", now, now); err != nil {
		t.Fatalf("insert legacy subscriber: %v", err)
	}

	// GET /confirm without a token renders the failure page.
	rec := doRequest(t, h, http.MethodGet, "/confirm", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "订阅确认失败") {
		t.Errorf("tokenless confirm: status = %d, want 200 with failure page", rec.Code)
	}
	// GET /unsubscribe without a token must not disclose the email.
	rec = doRequest(t, h, http.MethodGet, "/unsubscribe", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "取消订阅失败") {
		t.Errorf("tokenless unsubscribe form: status = %d, want 200 with failure page", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "legacy@example.com") {
		t.Error("tokenless unsubscribe form leaked the subscriber email")
	}
	// POST /unsubscribe without a token unsubscribes nobody.
	rec = doRequest(t, h, http.MethodPost, "/unsubscribe", url.Values{})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "取消订阅失败") {
		t.Errorf("tokenless unsubscribe: status = %d, want 200 with failure page", rec.Code)
	}
	sub, err := s.Q.GetSubscriberByEmail(t.Context(), "legacy@example.com")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if sub.ConfirmedAt.Valid || sub.UnsubscribedAt.Valid {
		t.Error("tokenless request changed the subscriber state")
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
// only the native+enabled newsletter shows the form; the navbar form renders
// on every public page (unlike Rails, which gates it to the root page).
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

	t.Run("article page shows the navbar form", func(t *testing.T) {
		s, h := newSubscriptionTestServer(t)
		setNewsletter(t, s, 1, "native")
		insertArticle(t, s, "hello", int64(domain.StatusPublish), 1)
		rec := doRequest(t, h, http.MethodGet, "/hello", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "newsletter-subscription") || !strings.Contains(body, `name="subscription[email]"`) {
			t.Error("navbar form not rendered on the article page")
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
