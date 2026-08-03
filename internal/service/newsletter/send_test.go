package newsletter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/jobs"
)

// captureSender is a Sender that records messages instead of dialing SMTP.
type captureSender struct {
	mu   sync.Mutex
	sent []Message
	fail map[string]error // keyed by To
}

func (c *captureSender) Send(_ context.Context, msg Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err, ok := c.fail[msg.To]; ok {
		return err
	}
	c.sent = append(c.sent, msg)
	return nil
}

func (c *captureSender) recipients() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.sent {
		out = append(out, m.To)
	}
	sort.Strings(out)
	return out
}

func openSendDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func seedSettings(t *testing.T, d *sql.DB, title, url string) {
	t.Helper()
	_, err := d.Exec(`INSERT INTO settings (id, title, url, created_at, updated_at) VALUES (1, ?, ?, 1000, 1000)`, title, url)
	if err != nil {
		t.Fatalf("insert settings: %v", err)
	}
}

// seedNewsletterSetting inserts the singleton; smtp fields follow the native
// configured? requirements unless overridden per column by the caller's SQL.
func seedNewsletterSetting(t *testing.T, d *sql.DB, enabled int64, provider string) {
	t.Helper()
	_, err := d.Exec(`INSERT INTO newsletter_settings
		(id, enabled, provider, from_email, smtp_address, smtp_port, smtp_user_name, smtp_password, created_at, updated_at)
		VALUES (1, ?, ?, 'news@example.com', 'smtp.example.com', 587, 'u', 'p', 1000, 1000)`, enabled, provider)
	if err != nil {
		t.Fatalf("insert newsletter_settings: %v", err)
	}
}

func seedArticle(t *testing.T, d *sql.DB, title, slug, contentHTML string) int64 {
	t.Helper()
	res, err := d.Exec(`INSERT INTO articles (title, slug, content_html, status, created_at, updated_at)
		VALUES (?, ?, ?, 1, 1000, 1000)`, title, slug, contentHTML)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("article id: %v", err)
	}
	return id
}

func seedTag(t *testing.T, d *sql.DB, name string) int64 {
	t.Helper()
	res, err := d.Exec(`INSERT INTO tags (name, slug, created_at, updated_at) VALUES (?, ?, 1000, 1000)`, name, name)
	if err != nil {
		t.Fatalf("insert tag: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func linkArticleTag(t *testing.T, d *sql.DB, articleID, tagID int64) {
	t.Helper()
	_, err := d.Exec(`INSERT INTO article_tags (article_id, tag_id, created_at, updated_at) VALUES (?, ?, 1000, 1000)`, articleID, tagID)
	if err != nil {
		t.Fatalf("insert article_tag: %v", err)
	}
}

// seedSubscriber inserts one subscriber; confirmed/unsubscribed drive the
// active flag, tagIDs the subscription filter. Tokens are derived from the
// address with urlsafe characters, like the base64url originals.
func seedSubscriber(t *testing.T, d *sql.DB, email string, confirmed, unsubscribed bool, tagIDs []int64) int64 {
	t.Helper()
	tokenPart := strings.NewReplacer("@", "-", ".", "-").Replace(email)
	var confirmedAt, unsubscribedAt any
	if confirmed {
		confirmedAt = int64(1000)
	}
	if unsubscribed {
		unsubscribedAt = int64(1000)
	}
	res, err := d.Exec(`INSERT INTO subscribers
		(email, confirmation_token, unsubscribe_token, confirmed_at, unsubscribed_at, created_at, updated_at)
		VALUES (?, 'confirm-`+tokenPart+`', 'unsub-`+tokenPart+`', ?, ?, 1000, 1000)`, email, confirmedAt, unsubscribedAt)
	if err != nil {
		t.Fatalf("insert subscriber %s: %v", email, err)
	}
	id, _ := res.LastInsertId()
	for _, tagID := range tagIDs {
		if _, err := d.Exec(`INSERT INTO subscriber_tags (subscriber_id, tag_id, created_at, updated_at)
			VALUES (?, ?, 1000, 1000)`, id, tagID); err != nil {
			t.Fatalf("insert subscriber_tag: %v", err)
		}
	}
	return id
}

// runOneJob wires the worker with a capture sender, enqueues one due job and
// runs a single poll, returning the final job_runs row.
func runOneJob(t *testing.T, d *sql.DB, cap *captureSender, kind string, payload any) (status string, attempts int64) {
	t.Helper()
	w := jobs.NewWorker(d)
	RegisterSendHandlers(w, d, t.TempDir(), SendConfig{
		RoutePrefix: "posts",
		NewSender:   func(SMTPConfig) Sender { return cap },
	})
	if _, err := jobs.NewEnqueuer(d).Enqueue(t.Context(), kind, payload, time.Now()); err != nil {
		t.Fatalf("enqueue %s: %v", kind, err)
	}
	claimed, err := w.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !claimed {
		t.Fatal("RunOnce claimed no job")
	}
	var lastError sql.NullString
	if err := d.QueryRow(`SELECT status, attempts, last_error FROM job_runs ORDER BY id DESC LIMIT 1`).
		Scan(&status, &attempts, &lastError); err != nil {
		t.Fatalf("load job_run: %v", err)
	}
	if status == "failed" {
		t.Logf("job failed: %s", lastError.String)
	}
	return status, attempts
}

func activityDescriptions(t *testing.T, d *sql.DB, action string) []string {
	t.Helper()
	rows, err := d.Query(`SELECT description FROM activity_logs WHERE action = ? ORDER BY id`, action)
	if err != nil {
		t.Fatalf("query activity_logs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var desc sql.NullString
		if err := rows.Scan(&desc); err != nil {
			t.Fatalf("scan activity_logs: %v", err)
		}
		out = append(out, desc.String)
	}
	return out
}

func TestSendNewsletterNativeRecipients(t *testing.T) {
	d := openSendDB(t)
	seedSettings(t, d, "My Blog", "https://blog.example.com")
	seedNewsletterSetting(t, d, 1, "native")
	golang := seedTag(t, d, "golang")
	rust := seedTag(t, d, "rust")
	articleID := seedArticle(t, d, "Go News", "go-news", "<p>hello <b>world</b></p>")
	linkArticleTag(t, d, articleID, golang)

	seedSubscriber(t, d, "all@example.com", true, false, nil)             // no tags = all content
	seedSubscriber(t, d, "hit@example.com", true, false, []int64{golang}) // tag intersects
	seedSubscriber(t, d, "miss@example.com", true, false, []int64{rust})  // no intersection
	seedSubscriber(t, d, "pending@example.com", false, false, nil)        // unconfirmed
	seedSubscriber(t, d, "gone@example.com", true, true, nil)             // unsubscribed

	cap := &captureSender{}
	status, _ := runOneJob(t, d, cap, jobs.KindSendNewsletter, map[string]int64{"article_id": articleID})
	if status != "done" {
		t.Fatalf("job status = %s, want done", status)
	}
	want := []string{"all@example.com", "hit@example.com"}
	if got := cap.recipients(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	msg := cap.sent[0]
	if msg.From != "news@example.com" {
		t.Errorf("From = %q, want news@example.com", msg.From)
	}
	if msg.Subject != "Go News | My Blog" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	for _, fragment := range []string{
		`https://blog.example.com/posts/go-news`,
		`https://blog.example.com/unsubscribe?token=unsub-` + strings.NewReplacer("@", "-", ".", "-").Replace(msg.To),
		`<p>hello <b>world</b></p>`,
	} {
		if !strings.Contains(msg.HTML, fragment) {
			t.Errorf("html body missing %q", fragment)
		}
	}
	if !strings.Contains(msg.Text, "hello world") {
		t.Errorf("text body missing plain content: %q", msg.Text)
	}
}

func TestSendNewsletterNativeSkips(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, d *sql.DB) int64 // returns the article id
	}{
		{
			name: "disabled",
			setup: func(t *testing.T, d *sql.DB) int64 {
				seedNewsletterSetting(t, d, 0, "native")
				articleID := seedArticle(t, d, "T", "t", "<p>x</p>")
				seedSubscriber(t, d, "all@example.com", true, false, nil)
				return articleID
			},
		},
		{
			name: "smtp not configured",
			setup: func(t *testing.T, d *sql.DB) int64 {
				seedNewsletterSetting(t, d, 1, "native")
				if _, err := d.Exec(`UPDATE newsletter_settings SET smtp_user_name = NULL WHERE id = 1`); err != nil {
					t.Fatalf("clear smtp_user_name: %v", err)
				}
				articleID := seedArticle(t, d, "T", "t", "<p>x</p>")
				seedSubscriber(t, d, "all@example.com", true, false, nil)
				return articleID
			},
		},
		{
			name: "no relevant subscribers",
			setup: func(t *testing.T, d *sql.DB) int64 {
				seedNewsletterSetting(t, d, 1, "native")
				golang := seedTag(t, d, "golang")
				rust := seedTag(t, d, "rust")
				articleID := seedArticle(t, d, "T", "t", "<p>x</p>")
				linkArticleTag(t, d, articleID, rust)
				seedSubscriber(t, d, "miss@example.com", true, false, []int64{golang})
				seedSubscriber(t, d, "pending@example.com", false, false, nil)
				return articleID
			},
		},
		{
			name: "unknown provider",
			setup: func(t *testing.T, d *sql.DB) int64 {
				seedNewsletterSetting(t, d, 1, "carrier-pigeon")
				articleID := seedArticle(t, d, "T", "t", "<p>x</p>")
				seedSubscriber(t, d, "all@example.com", true, false, nil)
				return articleID
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openSendDB(t)
			seedSettings(t, d, "My Blog", "https://blog.example.com")
			articleID := tt.setup(t, d)
			cap := &captureSender{}
			status, _ := runOneJob(t, d, cap, jobs.KindSendNewsletter, map[string]int64{"article_id": articleID})
			if status != "done" {
				t.Errorf("job status = %s, want done", status)
			}
			if got := cap.recipients(); len(got) != 0 {
				t.Errorf("recipients = %v, want none", got)
			}
		})
	}
}

// A single failed delivery is logged and counted; the batch completes.
func TestSendNewsletterNativePartialFailure(t *testing.T) {
	d := openSendDB(t)
	seedSettings(t, d, "My Blog", "https://blog.example.com")
	seedNewsletterSetting(t, d, 1, "native")
	articleID := seedArticle(t, d, "Go News", "go-news", "<p>x</p>")
	seedSubscriber(t, d, "a@example.com", true, false, nil)
	seedSubscriber(t, d, "b@example.com", true, false, nil)

	cap := &captureSender{fail: map[string]error{"a@example.com": errors.New("smtp boom")}}
	status, _ := runOneJob(t, d, cap, jobs.KindSendNewsletter, map[string]int64{"article_id": articleID})
	if status != "done" {
		t.Fatalf("job status = %s, want done", status)
	}
	if got := cap.recipients(); strings.Join(got, ",") != "b@example.com" {
		t.Fatalf("recipients = %v, want [b@example.com]", got)
	}
	failed := activityDescriptions(t, d, "failed")
	if len(failed) != 1 || !strings.Contains(failed[0], `email="a@example.com"`) || !strings.Contains(failed[0], "smtp boom") {
		t.Errorf("failed activity = %v", failed)
	}
	completed := activityDescriptions(t, d, "completed")
	if len(completed) != 1 || !strings.Contains(completed[0], "success_count=1") || !strings.Contains(completed[0], "error_count=1") {
		t.Errorf("completed activity = %v", completed)
	}
}

// NativeNewsletterSenderJob does not rescue RecordNotFound: the job errors.
func TestSendNewsletterNativeMissingArticle(t *testing.T) {
	d := openSendDB(t)
	seedNewsletterSetting(t, d, 1, "native")
	cap := &captureSender{}
	status, attempts := runOneJob(t, d, cap, jobs.KindSendNewsletter, map[string]int64{"article_id": 999})
	if status != "queued" || attempts != 1 {
		t.Errorf("job = (%s, %d), want a rescheduled (queued, 1)", status, attempts)
	}
}

type listmonkRequest struct {
	method string
	path   string
	user   string
	pass   string
	body   string
}

// fakeListmonk records requests and answers create/status per script.
func fakeListmonk(t *testing.T, createStatus int, createBody string) (*httptest.Server, *[]listmonkRequest) {
	t.Helper()
	var requests []listmonkRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		user, pass, _ := r.BasicAuth()
		requests = append(requests, listmonkRequest{r.Method, r.URL.Path, user, pass, string(body)})
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(createStatus)
			_, _ = io.WriteString(w, createBody)
			return
		}
		_, _ = io.WriteString(w, `{"data":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func seedListmonk(t *testing.T, d *sql.DB, url string, listID, templateID any) {
	t.Helper()
	_, err := d.Exec(`INSERT INTO listmonks (id, url, username, api_key, list_id, template_id, enabled, created_at, updated_at)
		VALUES (1, ?, 'admin', 'secret', ?, ?, 1, 1000, 1000)`, url, listID, templateID)
	if err != nil {
		t.Fatalf("insert listmonks: %v", err)
	}
}

func TestSendNewsletterListmonk(t *testing.T) {
	d := openSendDB(t)
	seedSettings(t, d, "My Blog", "https://blog.example.com")
	seedNewsletterSetting(t, d, 1, "listmonk")
	srv, requests := fakeListmonk(t, http.StatusOK, `{"data":{"id":42}}`)
	seedListmonk(t, d, srv.URL, int64(7), int64(9))
	articleID := seedArticle(t, d, "Go News", "go-news", "<p>hello</p>")
	if _, err := d.Exec(`UPDATE articles SET source_author = 'Jane', source_url = 'https://x.test/a', source_content = 'quoted' WHERE id = ?`, articleID); err != nil {
		t.Fatalf("set source: %v", err)
	}

	cap := &captureSender{}
	status, _ := runOneJob(t, d, cap, jobs.KindSendNewsletter, map[string]int64{"article_id": articleID})
	if status != "done" {
		t.Fatalf("job status = %s, want done", status)
	}
	if len(*requests) != 2 {
		t.Fatalf("requests = %v, want 2", *requests)
	}

	create := (*requests)[0]
	if create.method != http.MethodPost || create.path != "/api/campaigns" {
		t.Errorf("create = %s %s", create.method, create.path)
	}
	if create.user != "admin" || create.pass != "secret" {
		t.Errorf("create basic auth = %s/%s", create.user, create.pass)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(create.body), &body); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	for key, want := range map[string]any{
		"name":         "Go News",
		"subject":      "Go News | My Blog",
		"type":         "regular",
		"content_type": "html",
		"messenger":    "email",
		"send_later":   false,
		"template_id":  float64(9),
	} {
		if body[key] != want {
			t.Errorf("create body[%s] = %v, want %v", key, body[key], want)
		}
	}
	if lists, ok := body["lists"].([]any); !ok || len(lists) != 1 || lists[0] != float64(7) {
		t.Errorf("create body lists = %v", body["lists"])
	}
	campaignBody, _ := body["body"].(string)
	for _, fragment := range []string{"source-reference__quote", "Jane", "quoted", "https://x.test/a", "<p>hello</p>"} {
		if !strings.Contains(campaignBody, fragment) {
			t.Errorf("campaign body missing %q", fragment)
		}
	}
	if !strings.HasSuffix(campaignBody, "\n<p>hello</p>") {
		t.Errorf("campaign body = %q, want reference html + newline + content", campaignBody)
	}

	run := (*requests)[1]
	// listmonk.rb issues a PUT for the status update (the plan's PATCH
	// mention defers to the source).
	if run.method != http.MethodPut || run.path != "/api/campaigns/42/status" {
		t.Errorf("status = %s %s", run.method, run.path)
	}
	if !strings.Contains(run.body, `"status":"running"`) {
		t.Errorf("status body = %s", run.body)
	}
	if got := cap.recipients(); len(got) != 0 {
		t.Errorf("smtp sends = %v, want none (listmonk delivers)", got)
	}
}

func TestSendNewsletterListmonkSkips(t *testing.T) {
	tests := []struct {
		name       string
		articleID  int64 // 0 reuses the seeded article
		listID     any
		templateID any
		seedRow    bool
	}{
		{name: "article missing", articleID: 999, listID: int64(7), templateID: int64(9), seedRow: true},
		{name: "listmonk row missing", seedRow: false},
		{name: "list id missing", listID: nil, templateID: int64(9), seedRow: true},
		{name: "template id missing", listID: int64(7), templateID: nil, seedRow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openSendDB(t)
			seedNewsletterSetting(t, d, 1, "listmonk")
			srv, requests := fakeListmonk(t, http.StatusOK, `{"data":{"id":42}}`)
			if tt.seedRow {
				seedListmonk(t, d, srv.URL, tt.listID, tt.templateID)
			}
			articleID := tt.articleID
			if articleID == 0 {
				articleID = seedArticle(t, d, "T", "t", "<p>x</p>")
			}
			status, _ := runOneJob(t, d, &captureSender{}, jobs.KindSendNewsletter, map[string]int64{"article_id": articleID})
			if status != "done" {
				t.Errorf("job status = %s, want done", status)
			}
			if len(*requests) != 0 {
				t.Errorf("requests = %v, want none", *requests)
			}
		})
	}
}

// A failed campaign create is rescued inside Listmonk#send_newsletter: the
// job logs the failure and completes.
func TestSendNewsletterListmonkCreateFails(t *testing.T) {
	d := openSendDB(t)
	seedNewsletterSetting(t, d, 1, "listmonk")
	srv, requests := fakeListmonk(t, http.StatusInternalServerError, `{"message":"nope"}`)
	seedListmonk(t, d, srv.URL, int64(7), int64(9))
	articleID := seedArticle(t, d, "Go News", "go-news", "<p>x</p>")

	status, _ := runOneJob(t, d, &captureSender{}, jobs.KindSendNewsletter, map[string]int64{"article_id": articleID})
	if status != "done" {
		t.Fatalf("job status = %s, want done", status)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests = %v, want only the create", *requests)
	}
	failed := activityDescriptions(t, d, "failed")
	if len(failed) != 1 || !strings.Contains(failed[0], `operation="campaign"`) || !strings.Contains(failed[0], "Create Campaign failed!") {
		t.Errorf("failed activity = %v", failed)
	}
}

func TestNewsletterConfirmation(t *testing.T) {
	d := openSendDB(t)
	seedSettings(t, d, "My Blog", "https://blog.example.com")
	seedNewsletterSetting(t, d, 1, "native")
	subID := seedSubscriber(t, d, "new@example.com", false, false, nil)

	cap := &captureSender{}
	status, _ := runOneJob(t, d, cap, jobs.KindNewsletterConfirmation, map[string]int64{"subscriber_id": subID})
	if status != "done" {
		t.Fatalf("job status = %s, want done", status)
	}
	if len(cap.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(cap.sent))
	}
	msg := cap.sent[0]
	if msg.To != "new@example.com" {
		t.Errorf("To = %q", msg.To)
	}
	if msg.Subject != "请确认您的订阅 | My Blog" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	link := "https://blog.example.com/confirm?token=confirm-new-example-com"
	if !strings.Contains(msg.HTML, link) || !strings.Contains(msg.Text, link) {
		t.Errorf("confirmation link %q missing from bodies", link)
	}
}

// NewsletterConfirmationJob rescues RecordNotFound: drop quietly.
func TestNewsletterConfirmationMissingSubscriber(t *testing.T) {
	d := openSendDB(t)
	seedNewsletterSetting(t, d, 1, "native")
	cap := &captureSender{}
	status, _ := runOneJob(t, d, cap, jobs.KindNewsletterConfirmation, map[string]int64{"subscriber_id": 999})
	if status != "done" {
		t.Errorf("job status = %s, want done", status)
	}
	if len(cap.sent) != 0 {
		t.Errorf("sent %d messages, want 0", len(cap.sent))
	}
}

func seedComment(t *testing.T, d *sql.DB, articleID int64, parentID any, author, email string, status int64, platform any) int64 {
	t.Helper()
	res, err := d.Exec(`INSERT INTO comments
		(commentable_type, commentable_id, article_id, parent_id, author_name, author_email, content, status, platform, created_at, updated_at)
		VALUES ('Article', ?, ?, ?, ?, ?, 'content of ' || ?, ?, ?, 1000, 1000)`,
		articleID, articleID, parentID, author, email, author, status, platform)
	if err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestCommentReplyNotification(t *testing.T) {
	d := openSendDB(t)
	seedSettings(t, d, "My Blog", "https://blog.example.com")
	seedNewsletterSetting(t, d, 1, "native")
	articleID := seedArticle(t, d, "Hello", "hello", "<p>x</p>")
	parentID := seedComment(t, d, articleID, nil, "Alice", "alice@example.com", 1, nil)
	replyID := seedComment(t, d, articleID, parentID, "Bob", "bob@example.com", 1, nil)

	cap := &captureSender{}
	status, _ := runOneJob(t, d, cap, jobs.KindCommentReplyNotification, map[string]int64{"comment_id": replyID})
	if status != "done" {
		t.Fatalf("job status = %s, want done", status)
	}
	if len(cap.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(cap.sent))
	}
	msg := cap.sent[0]
	if msg.To != "alice@example.com" {
		t.Errorf("To = %q, want the parent author", msg.To)
	}
	if msg.Subject != "New reply to your comment | My Blog" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	for _, fragment := range []string{"Bob", "content of Bob", "content of Alice", "https://blog.example.com/posts/hello"} {
		if !strings.Contains(msg.HTML, fragment) {
			t.Errorf("html body missing %q", fragment)
		}
	}
	sent := activityDescriptions(t, d, "sent")
	if len(sent) != 1 || !strings.Contains(sent[0], `email="alice@example.com"`) || !strings.Contains(sent[0], `author="Bob"`) {
		t.Errorf("sent activity = %v", sent)
	}
}

func TestCommentReplyNotificationIneligible(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, d *sql.DB, articleID, parentID int64) int64 // returns the reply id
	}{
		{
			name: "reply not approved",
			setup: func(t *testing.T, d *sql.DB, articleID, parentID int64) int64 {
				return seedComment(t, d, articleID, parentID, "Bob", "bob@example.com", 0, nil)
			},
		},
		{
			name: "reply has platform",
			setup: func(t *testing.T, d *sql.DB, articleID, parentID int64) int64 {
				return seedComment(t, d, articleID, parentID, "Bob", "bob@example.com", 1, "mastodon")
			},
		},
		{
			name: "parent without email",
			setup: func(t *testing.T, d *sql.DB, articleID, _ int64) int64 {
				parentID := seedComment(t, d, articleID, nil, "Anon", "", 1, nil)
				return seedComment(t, d, articleID, parentID, "Bob", "bob@example.com", 1, nil)
			},
		},
		{
			name: "parent has platform",
			setup: func(t *testing.T, d *sql.DB, articleID, _ int64) int64 {
				parentID := seedComment(t, d, articleID, nil, "Alice", "alice@example.com", 1, "bluesky")
				return seedComment(t, d, articleID, parentID, "Bob", "bob@example.com", 1, nil)
			},
		},
		{
			name: "same author email case-insensitive",
			setup: func(t *testing.T, d *sql.DB, articleID, parentID int64) int64 {
				return seedComment(t, d, articleID, parentID, "Alice2", "ALICE@example.com", 1, nil)
			},
		},
		{
			name: "newsletter disabled",
			setup: func(t *testing.T, d *sql.DB, articleID, parentID int64) int64 {
				if _, err := d.Exec(`UPDATE newsletter_settings SET enabled = 0 WHERE id = 1`); err != nil {
					t.Fatalf("disable newsletter: %v", err)
				}
				return seedComment(t, d, articleID, parentID, "Bob", "bob@example.com", 1, nil)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openSendDB(t)
			seedSettings(t, d, "My Blog", "https://blog.example.com")
			seedNewsletterSetting(t, d, 1, "native")
			articleID := seedArticle(t, d, "Hello", "hello", "<p>x</p>")
			parentID := seedComment(t, d, articleID, nil, "Alice", "alice@example.com", 1, nil)
			replyID := tt.setup(t, d, articleID, parentID)

			cap := &captureSender{}
			status, _ := runOneJob(t, d, cap, jobs.KindCommentReplyNotification, map[string]int64{"comment_id": replyID})
			if status != "done" {
				t.Errorf("job status = %s, want done", status)
			}
			if len(cap.sent) != 0 {
				t.Errorf("sent %d messages, want 0", len(cap.sent))
			}
		})
	}
}

// The find_by early return for a missing comment.
func TestCommentReplyNotificationMissingComment(t *testing.T) {
	d := openSendDB(t)
	cap := &captureSender{}
	status, _ := runOneJob(t, d, cap, jobs.KindCommentReplyNotification, map[string]int64{"comment_id": 999})
	if status != "done" {
		t.Errorf("job status = %s, want done", status)
	}
}

// The placeholder keeps legacy password_reset rows from failing as an
// unknown kind (no producer exists in the Go rewrite; see T06 decision).
func TestPasswordResetPlaceholder(t *testing.T) {
	d := openSendDB(t)
	status, _ := runOneJob(t, d, &captureSender{}, jobs.KindPasswordReset, nil)
	if status != "done" {
		t.Errorf("job status = %s, want done", status)
	}
}
