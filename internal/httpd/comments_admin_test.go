package httpd

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
)

// commentSession creates a user + session row directly and returns the
// session cookie for authenticated admin requests.
func commentSession(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	now := time.Now().UTC().Unix()
	if _, err := s.Q.CreateUser(t.Context(), query.CreateUserParams{
		UserName: "admin", PasswordDigest: "x", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sess, err := s.Q.CreateSession(t.Context(), query.CreateSessionParams{
		Token: "comment-test-session", UserID: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: sess.Token}
}

// setSiteAuthor configures settings.author/url via the setup update query.
func setSiteAuthor(t *testing.T, s *Server, author, siteURL string) {
	t.Helper()
	now := time.Now().UTC().Unix()
	if err := s.Q.EnsureSettings(t.Context(), query.EnsureSettingsParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	if err := s.Q.CompleteSetup(t.Context(), query.CompleteSetupParams{
		Title:       sql.NullString{String: "Blog", Valid: true},
		Description: sql.NullString{String: "d", Valid: true},
		Author:      sql.NullString{String: author, Valid: author != ""},
		Url:         sql.NullString{String: siteURL, Valid: siteURL != ""},
		TimeZone:    "UTC",
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("complete setup: %v", err)
	}
}

func localComment(t *testing.T, s *Server, articleID int64, author string, status domain.CommentStatus) query.Comment {
	t.Helper()
	return insertComment(t, s, query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: articleID, Valid: true},
		AuthorName:      author,
		Content:         "body of " + author,
		Status:          int64(status),
		PublishedAt:     sql.NullInt64{Int64: time.Now().UTC().Unix(), Valid: true},
	})
}

func TestAdminCommentsRequiresAuth(t *testing.T) {
	_, h := newCommentTestServer(t)
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/admin/comments"},
		{http.MethodPost, "/admin/comments/1/approve"},
		{http.MethodPost, "/admin/comments/1/reject"},
		{http.MethodPost, "/admin/comments/1/reply"},
		{http.MethodPost, "/admin/comments/batch_approve"},
		{http.MethodPost, "/admin/comments/batch_reject"},
		{http.MethodPost, "/admin/comments/batch_destroy"},
	} {
		rec := doRequest(t, h, req.method, req.path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("%s %s: status = %d location = %q, want 302 /session/new",
				req.method, req.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestAdminCommentsIndexOrder(t *testing.T) {
	s, h := newCommentTestServer(t)
	session := commentSession(t, s)
	insertArticle(t, s, "post", 1, 1)

	base := query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
		Status:          int64(domain.CommentPending),
		Content:         "x",
	}
	// COALESCE(published_at, created_at) DESC: middle(300) > newest-name(200) > oldest(100).
	mk := func(name string, publishedAt, createdAt int64) {
		p := base
		p.AuthorName = name
		p.PublishedAt = sql.NullInt64{Int64: publishedAt, Valid: publishedAt != 0}
		p.CreatedAt = createdAt
		p.UpdatedAt = createdAt
		insertComment(t, s, p)
	}
	mk("oldest", 100, 100)
	mk("middle-via-created", 0, 300) // no published_at: created_at wins
	mk("newest", 200, 200)

	rec := doRequest(t, h, http.MethodGet, "/admin/comments", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	iMiddle := strings.Index(body, "middle-via-created")
	iNewest := strings.Index(body, "newest")
	iOldest := strings.Index(body, "oldest")
	if iMiddle < 0 || iNewest < 0 || iOldest < 0 {
		t.Fatalf("missing rows in output:\n%s", body)
	}
	if !(iMiddle < iNewest && iNewest < iOldest) {
		t.Errorf("order = middle@%d newest@%d oldest@%d, want COALESCE DESC", iMiddle, iNewest, iOldest)
	}
	if !strings.Contains(body, "Title post") { // commentable title resolved
		t.Error("commentable title missing")
	}
}

func TestAdminCommentsIndexStatusFilter(t *testing.T) {
	s, h := newCommentTestServer(t)
	session := commentSession(t, s)
	insertArticle(t, s, "post", 1, 1)
	localComment(t, s, 1, "pending-one", domain.CommentPending)
	localComment(t, s, 1, "approved-one", domain.CommentApproved)
	localComment(t, s, 1, "rejected-one", domain.CommentRejected)

	rec := doRequest(t, h, http.MethodGet, "/admin/comments?status=pending", nil, session)
	body := rec.Body.String()
	if !strings.Contains(body, "pending-one") {
		t.Error("pending filter hides the pending comment")
	}
	if strings.Contains(body, "approved-one") || strings.Contains(body, "rejected-one") {
		t.Error("pending filter shows other statuses")
	}
}

func TestAdminCommentsPagination(t *testing.T) {
	s, h := newCommentTestServer(t)
	session := commentSession(t, s)
	insertArticle(t, s, "post", 1, 1)
	for i := range 31 {
		localComment(t, s, 1, fmt.Sprintf("user-%02d", i), domain.CommentPending)
	}

	rec := doRequest(t, h, http.MethodGet, "/admin/comments", nil, session)
	if got := strings.Count(rec.Body.String(), `type="checkbox" name="ids"`); got != 30 {
		t.Errorf("page 1 rows = %d, want 30", got)
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/comments?page=2", nil, session)
	if got := strings.Count(rec.Body.String(), `type="checkbox" name="ids"`); got != 1 {
		t.Errorf("page 2 rows = %d, want 1", got)
	}
}

func TestAdminApproveReject(t *testing.T) {
	s, h := newCommentTestServer(t)
	session := commentSession(t, s)
	insertArticle(t, s, "post", 1, 1)
	c := localComment(t, s, 1, "Ann", domain.CommentPending)

	rec := doRequest(t, h, http.MethodPost, "/admin/comments/"+strconv.FormatInt(c.ID, 10)+"/approve", nil, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/comments" {
		t.Fatalf("approve: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if flash := readFlash(t, rec); flash.Notice != "Comment approved successfully." {
		t.Errorf("approve flash = %+v", flash)
	}
	got, _ := s.Q.GetCommentByID(t.Context(), c.ID)
	if got.Status != int64(domain.CommentApproved) {
		t.Errorf("status = %d, want approved", got.Status)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/comments/"+strconv.FormatInt(c.ID, 10)+"/reject", nil, session)
	if flash := readFlash(t, rec); flash.Notice != "Comment rejected." {
		t.Errorf("reject flash = %+v", flash)
	}
	got, _ = s.Q.GetCommentByID(t.Context(), c.ID)
	if got.Status != int64(domain.CommentRejected) {
		t.Errorf("status = %d, want rejected", got.Status)
	}

	// Unknown id 404s like Rails' Comment.find.
	rec = doRequest(t, h, http.MethodPost, "/admin/comments/999/approve", nil, session)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown comment: status = %d, want 404", rec.Code)
	}
}

func TestAdminApproveReplyEnqueuesNotification(t *testing.T) {
	s, h := newCommentTestServer(t)
	session := commentSession(t, s)
	insertArticle(t, s, "post", 1, 1)
	parent := insertComment(t, s, query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
		AuthorName:      "Parent",
		AuthorEmail:     sql.NullString{String: "parent@example.com", Valid: true},
		Content:         "top", Status: int64(domain.CommentApproved),
	})
	reply := insertComment(t, s, query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
		ParentID:        sql.NullInt64{Int64: parent.ID, Valid: true},
		AuthorName:      "Replier", Content: "sub", Status: int64(domain.CommentPending),
	})

	rec := doRequest(t, h, http.MethodPost, "/admin/comments/"+strconv.FormatInt(reply.ID, 10)+"/approve", nil, session)
	if rec.Code != http.StatusFound {
		t.Fatalf("approve reply: status = %d", rec.Code)
	}
	var payload string
	err := s.DB.QueryRow("SELECT payload FROM job_runs WHERE kind = ?", jobs.KindCommentReplyNotification).Scan(&payload)
	if err != nil {
		t.Fatalf("notification job missing: %v", err)
	}
	if want := `{"comment_id":` + strconv.FormatInt(reply.ID, 10) + `}`; payload != want {
		t.Errorf("payload = %s, want %s", payload, want)
	}
}

func TestAdminBatchActions(t *testing.T) {
	s, h := newCommentTestServer(t)
	session := commentSession(t, s)
	insertArticle(t, s, "post", 1, 1)
	c1 := localComment(t, s, 1, "u1", domain.CommentPending)
	c2 := localComment(t, s, 1, "u2", domain.CommentPending)
	c3 := localComment(t, s, 1, "u3", domain.CommentPending)

	ids := url.Values{"ids": {
		strconv.FormatInt(c1.ID, 10), strconv.FormatInt(c2.ID, 10), "bogus", "999",
	}}
	rec := doRequest(t, h, http.MethodPost, "/admin/comments/batch_approve", ids, session)
	if flash := readFlash(t, rec); flash.Notice != "Successfully approved 2 comment(s)." {
		t.Errorf("batch approve flash = %+v", flash)
	}
	for _, id := range []int64{c1.ID, c2.ID} {
		got, _ := s.Q.GetCommentByID(t.Context(), id)
		if got.Status != int64(domain.CommentApproved) {
			t.Errorf("comment %d status = %d, want approved", id, got.Status)
		}
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/comments/batch_reject",
		url.Values{"ids": {strconv.FormatInt(c1.ID, 10)}}, session)
	if flash := readFlash(t, rec); flash.Notice != "Successfully rejected 1 comment(s)." {
		t.Errorf("batch reject flash = %+v", flash)
	}

	// Destroy cascades replies (foreign_keys on).
	child := insertComment(t, s, query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
		ParentID:        sql.NullInt64{Int64: c3.ID, Valid: true},
		AuthorName:      "child", Content: "x", Status: 1,
	})
	rec = doRequest(t, h, http.MethodPost, "/admin/comments/batch_destroy",
		url.Values{"ids": {strconv.FormatInt(c3.ID, 10)}}, session)
	if flash := readFlash(t, rec); flash.Notice != "Successfully deleted 1 comment(s)." {
		t.Errorf("batch destroy flash = %+v", flash)
	}
	if _, err := s.Q.GetCommentByID(t.Context(), c3.ID); err == nil {
		t.Error("parent comment still present after destroy")
	}
	if _, err := s.Q.GetCommentByID(t.Context(), child.ID); err == nil {
		t.Error("reply did not cascade-delete")
	}
}

func TestAdminReply(t *testing.T) {
	s, h := newCommentTestServer(t)
	session := commentSession(t, s)
	insertArticle(t, s, "post", 1, 1)
	setSiteAuthor(t, s, "Site Author", "blog.example.com/")

	parent := insertComment(t, s, query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
		AuthorName:      "Ann",
		AuthorEmail:     sql.NullString{String: "ann@example.com", Valid: true},
		Content:         "top", Status: int64(domain.CommentPending),
	})
	replyPath := "/admin/comments/" + strconv.FormatInt(parent.ID, 10) + "/reply"

	rec := doRequest(t, h, http.MethodPost, replyPath, url.Values{"comment[content]": {"thanks!"}}, session)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/comments" {
		t.Fatalf("reply: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if flash := readFlash(t, rec); flash.Notice != "Reply posted successfully." {
		t.Errorf("reply flash = %+v", flash)
	}

	var reply query.Comment
	if err := s.DB.QueryRow("SELECT * FROM comments WHERE parent_id = ?", parent.ID).Scan(scanCommentDest(&reply)...); err != nil {
		t.Fatalf("load reply: %v", err)
	}
	if reply.Status != int64(domain.CommentApproved) {
		t.Errorf("reply status = %d, want approved", reply.Status)
	}
	if reply.AuthorName != "Site Author" {
		t.Errorf("reply author = %q, want Site Author", reply.AuthorName)
	}
	if reply.AuthorUrl.String != "https://blog.example.com" {
		t.Errorf("reply author_url = %q, want https://blog.example.com", reply.AuthorUrl.String)
	}
	// Parent left an email: the notification job is queued for the reply.
	var payload string
	if err := s.DB.QueryRow("SELECT payload FROM job_runs WHERE kind = ?", jobs.KindCommentReplyNotification).Scan(&payload); err != nil {
		t.Fatalf("notification job missing: %v", err)
	}
	if want := `{"comment_id":` + strconv.FormatInt(reply.ID, 10) + `}`; payload != want {
		t.Errorf("payload = %s, want %s", payload, want)
	}
}

func TestAdminReplyRejections(t *testing.T) {
	setup := func(t *testing.T) (*Server, http.Handler, *http.Cookie) {
		s, h := newCommentTestServer(t)
		session := commentSession(t, s)
		insertArticle(t, s, "post", 1, 1)
		setSiteAuthor(t, s, "Site Author", "blog.example.com")
		return s, h, session
	}

	t.Run("external comment", func(t *testing.T) {
		s, h, session := setup(t)
		ext := insertComment(t, s, query.CreateCommentParams{
			CommentableType: sql.NullString{String: "Article", Valid: true},
			CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
			AuthorName:      "Ext", Content: "x", Status: 1,
			Platform:   sql.NullString{String: "mastodon", Valid: true},
			ExternalID: sql.NullString{String: "e1", Valid: true},
		})
		before := commentCount(t, s)
		rec := doRequest(t, h, http.MethodPost, "/admin/comments/"+strconv.FormatInt(ext.ID, 10)+"/reply",
			url.Values{"comment[content]": {"hi"}}, session)
		if flash := readFlash(t, rec); flash.Alert != "Cannot reply to external comments." {
			t.Errorf("flash = %+v", flash)
		}
		if got := commentCount(t, s); got != before {
			t.Error("reply to external comment was stored")
		}
	})

	t.Run("rejected comment", func(t *testing.T) {
		s, h, session := setup(t)
		c := localComment(t, s, 1, "Ann", domain.CommentRejected)
		rec := doRequest(t, h, http.MethodPost, "/admin/comments/"+strconv.FormatInt(c.ID, 10)+"/reply",
			url.Values{"comment[content]": {"hi"}}, session)
		if flash := readFlash(t, rec); flash.Alert != "Cannot reply to rejected comments." {
			t.Errorf("flash = %+v", flash)
		}
	})

	t.Run("blank content", func(t *testing.T) {
		s, h, session := setup(t)
		c := localComment(t, s, 1, "Ann", domain.CommentPending)
		before := commentCount(t, s)
		rec := doRequest(t, h, http.MethodPost, "/admin/comments/"+strconv.FormatInt(c.ID, 10)+"/reply",
			url.Values{"comment[content]": {""}}, session)
		if flash := readFlash(t, rec); !strings.Contains(flash.Alert, "Content can't be blank") {
			t.Errorf("flash = %+v", flash)
		}
		if got := commentCount(t, s); got != before {
			t.Error("blank reply was stored")
		}
	})

	t.Run("author not configured", func(t *testing.T) {
		s, h := newCommentTestServer(t)
		session := commentSession(t, s)
		insertArticle(t, s, "post", 1, 1)
		setSiteAuthor(t, s, "", "")
		c := localComment(t, s, 1, "Ann", domain.CommentPending)
		rec := doRequest(t, h, http.MethodPost, "/admin/comments/"+strconv.FormatInt(c.ID, 10)+"/reply",
			url.Values{"comment[content]": {"hi"}}, session)
		if flash := readFlash(t, rec); flash.Alert != "Please set the site author name in Settings before replying." {
			t.Errorf("flash = %+v", flash)
		}
	})
}
