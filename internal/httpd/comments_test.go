package httpd

import (
	"database/sql"
	"encoding/base64"
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
	"rables/internal/service/captcha"
	"rables/internal/templates"
)

// newCommentTestServer builds a Server with only the comment routes mounted
// (the integrator wires them into NewRouter later).
func newCommentTestServer(t *testing.T) (*Server, http.Handler) {
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
	RegisterCommentRoutes(r, s)
	RegisterCommentAdminRoutes(r, s)
	return s, r
}

// insertArticle inserts an article row directly (status/comment per args).
func insertArticle(t *testing.T, s *Server, slug string, status, comment int64) int64 {
	t.Helper()
	res, err := s.DB.Exec(`INSERT INTO articles (title, slug, status, comment, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1000, 1000)`, "Title "+slug, slug, status, comment)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertPage(t *testing.T, s *Server, slug string, status, comment int64) int64 {
	t.Helper()
	res, err := s.DB.Exec(`INSERT INTO pages (title, slug, status, comment, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1000, 1000)`, "Title "+slug, slug, status, comment)
	if err != nil {
		t.Fatalf("insert page: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// insertComment inserts a comment row directly and returns its id.
func insertComment(t *testing.T, s *Server, c query.CreateCommentParams) query.Comment {
	t.Helper()
	if c.CreatedAt == 0 {
		c.CreatedAt = time.Now().UTC().Unix()
	}
	if c.UpdatedAt == 0 {
		c.UpdatedAt = c.CreatedAt
	}
	comment, err := s.Q.CreateComment(t.Context(), c)
	if err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	return comment
}

// validCommentForm returns a submission with a fresh, correctly answered
// captcha for the test server secret ("x").
func validCommentForm(t *testing.T, extra url.Values) url.Values {
	t.Helper()
	cap := captcha.New("x", captcha.TTL)
	_, token := cap.Issue()
	expected, ok := cap.Expected(token)
	if !ok {
		t.Fatal("fresh captcha token rejected")
	}
	form := url.Values{
		"comment[author_name]": {"Ann"},
		"comment[content]":     {"hello"},
		"captcha[token]":       {token},
		"captcha[answer]":      {strconv.Itoa(expected)},
	}
	for k, vs := range extra {
		form[k] = vs
	}
	return form
}

// readFlash decodes the flash cookie of a response.
func readFlash(t *testing.T, rec *httptest.ResponseRecorder) templates.Flash {
	t.Helper()
	c := findCookie(rec, flashCookieName)
	if c == nil || c.Value == "" {
		return templates.Flash{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		t.Fatalf("decode flash cookie: %v", err)
	}
	var flash templates.Flash
	if err := json.Unmarshal(raw, &flash); err != nil {
		t.Fatalf("unmarshal flash: %v", err)
	}
	return flash
}

func commentCount(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM comments").Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	return n
}

func TestCommentSubmitToArticle(t *testing.T) {
	s, h := newCommentTestServer(t)
	insertArticle(t, s, "hello-world", 1, 1) // publish, comment=1

	rec := doRequest(t, h, http.MethodPost, "/comments", validCommentForm(t, url.Values{
		"article_id":            {"hello-world"},
		"comment[author_email]": {"ann@example.com"},
	}))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/hello-world" {
		t.Fatalf("status = %d location = %q, want 302 /hello-world", rec.Code, rec.Header().Get("Location"))
	}
	if flash := readFlash(t, rec); flash.Notice == "" || flash.Alert != "" {
		t.Errorf("flash = %+v, want a notice", flash)
	}

	var c query.Comment
	if err := s.DB.QueryRow("SELECT * FROM comments").Scan(scanCommentDest(&c)...); err != nil {
		t.Fatalf("load comment: %v", err)
	}
	if c.Status != 0 {
		t.Errorf("status = %d, want 0 (pending)", c.Status)
	}
	if c.CommentableType.String != "Article" || c.CommentableID.Int64 != 1 {
		t.Errorf("commentable = %s#%d, want Article#1", c.CommentableType.String, c.CommentableID.Int64)
	}
	if !c.PublishedAt.Valid {
		t.Error("published_at not set at submission")
	}
	if c.AuthorEmail.String != "ann@example.com" {
		t.Errorf("author_email = %q", c.AuthorEmail.String)
	}
}

// scanCommentDest orders destinations like the comments table DDL.
func scanCommentDest(c *query.Comment) []any {
	return []any{
		&c.ID, &c.CommentableType, &c.CommentableID, &c.ArticleID, &c.ParentID,
		&c.AuthorName, &c.AuthorEmail, &c.AuthorUrl, &c.AuthorUsername, &c.AuthorAvatarUrl,
		&c.Content, &c.Status, &c.Platform, &c.ExternalID, &c.Url, &c.PublishedAt,
		&c.CreatedAt, &c.UpdatedAt,
	}
}

func TestCommentSubmitReply(t *testing.T) {
	s, h := newCommentTestServer(t)
	insertArticle(t, s, "hello-world", 1, 1)
	parent := insertComment(t, s, query.CreateCommentParams{
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
		AuthorName:      "First", Content: "top", Status: 1,
	})

	rec := doRequest(t, h, http.MethodPost, "/comments", validCommentForm(t, url.Values{
		"article_id":           {"hello-world"},
		"comment[parent_id]":   {strconv.FormatInt(parent.ID, 10)},
		"comment[author_name]": {"Ann"},
	}))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	var reply query.Comment
	err := s.DB.QueryRow("SELECT * FROM comments WHERE parent_id = ?", parent.ID).Scan(scanCommentDest(&reply)...)
	if err != nil {
		t.Fatalf("load reply: %v", err)
	}
	if !reply.ParentID.Valid || reply.ParentID.Int64 != parent.ID {
		t.Errorf("parent_id = %+v, want %d", reply.ParentID, parent.ID)
	}
}

func TestCommentSubmitGates(t *testing.T) {
	tests := []struct {
		name         string
		setup        func(t *testing.T, s *Server)
		extra        url.Values
		wantLocation string
	}{
		{
			name:         "draft article rejects",
			setup:        func(t *testing.T, s *Server) { insertArticle(t, s, "draft-post", 0, 1) },
			extra:        url.Values{"article_id": {"draft-post"}},
			wantLocation: "/",
		},
		{
			name:         "trash article rejects",
			setup:        func(t *testing.T, s *Server) { insertArticle(t, s, "trashed", 3, 1) },
			extra:        url.Values{"article_id": {"trashed"}},
			wantLocation: "/",
		},
		{
			name:         "comments disabled rejects",
			setup:        func(t *testing.T, s *Server) { insertArticle(t, s, "no-comments", 1, 0) },
			extra:        url.Values{"article_id": {"no-comments"}},
			wantLocation: "/",
		},
		{
			name:         "shared article accepts",
			setup:        func(t *testing.T, s *Server) { insertArticle(t, s, "shared-post", 4, 1) },
			extra:        url.Values{"article_id": {"shared-post"}},
			wantLocation: "/shared-post",
		},
		{
			name:         "unknown slug rejects",
			setup:        func(t *testing.T, s *Server) {},
			extra:        url.Values{"article_id": {"nope"}},
			wantLocation: "/",
		},
		{
			name:         "published page accepts",
			setup:        func(t *testing.T, s *Server) { insertPage(t, s, "about", 1, 1) },
			extra:        url.Values{"page_id": {"about"}},
			wantLocation: "/pages/about",
		},
		{
			name:         "missing commentable rejects",
			setup:        func(t *testing.T, s *Server) {},
			extra:        url.Values{},
			wantLocation: "/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, h := newCommentTestServer(t)
			tt.setup(t, s)
			before := commentCount(t, s)
			rec := doRequest(t, h, http.MethodPost, "/comments", validCommentForm(t, tt.extra))
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != tt.wantLocation {
				t.Fatalf("status = %d location = %q, want 302 %s", rec.Code, rec.Header().Get("Location"), tt.wantLocation)
			}
			accepted := tt.wantLocation != "/"
			if got := commentCount(t, s) - before; (got == 1) != accepted {
				t.Errorf("comments created = %d, want accepted=%v", got, accepted)
			}
			if !accepted {
				if flash := readFlash(t, rec); flash.Alert == "" {
					t.Error("rejection did not flash an alert")
				}
			}
		})
	}
}

func TestCommentSubmitCaptchaFailures(t *testing.T) {
	s, h := newCommentTestServer(t)
	insertArticle(t, s, "hello-world", 1, 1)

	wrongAnswer := validCommentForm(t, url.Values{"article_id": {"hello-world"}})
	cap := captcha.New("x", captcha.TTL)
	expected, _ := cap.Expected(wrongAnswer.Get("captcha[token]"))
	wrongAnswer.Set("captcha[answer]", strconv.Itoa(expected+1))

	expiredCap := captcha.New("x", -time.Hour)
	_, expiredToken := expiredCap.Issue()
	expired := validCommentForm(t, url.Values{"article_id": {"hello-world"}})
	expired.Set("captcha[token]", expiredToken)

	tests := []struct {
		name      string
		form      url.Values
		wantAlert string
	}{
		{name: "wrong answer", form: wrongAnswer, wantAlert: "验证失败：请回答数学题。"},
		{name: "expired token", form: expired, wantAlert: "验证已过期：请刷新页面后重新回答数学题。"},
		{name: "missing token", form: url.Values{
			"article_id": {"hello-world"}, "comment[author_name]": {"Ann"},
			"comment[content]": {"hi"}, "captcha[answer]": {"3"},
		}, wantAlert: "验证已过期：请刷新页面后重新回答数学题。"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := commentCount(t, s)
			rec := doRequest(t, h, http.MethodPost, "/comments", tt.form)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/hello-world" {
				t.Fatalf("status = %d location = %q, want 302 /hello-world", rec.Code, rec.Header().Get("Location"))
			}
			if flash := readFlash(t, rec); flash.Alert != tt.wantAlert {
				t.Errorf("alert = %q, want %q", flash.Alert, tt.wantAlert)
			}
			if got := commentCount(t, s); got != before {
				t.Errorf("comment created despite bad captcha")
			}
		})
	}
}

func TestCommentSubmitValidation(t *testing.T) {
	// Fresh server per case: submissions share the per-IP rate budget (5).
	newServer := func(t *testing.T) (*Server, http.Handler, query.Comment) {
		s, h := newCommentTestServer(t)
		insertArticle(t, s, "hello-world", 1, 1)
		insertArticle(t, s, "other-post", 1, 1)
		otherComment := insertComment(t, s, query.CreateCommentParams{
			CommentableType: sql.NullString{String: "Article", Valid: true},
			CommentableID:   sql.NullInt64{Int64: 2, Valid: true},
			AuthorName:      "Other", Content: "on other post", Status: 1,
		})
		return s, h, otherComment
	}

	tests := []struct {
		name      string
		extra     func(otherComment query.Comment) url.Values
		wantAlert string
	}{
		{
			name: "blank author",
			extra: func(query.Comment) url.Values {
				return url.Values{"article_id": {"hello-world"}, "comment[author_name]": {""}}
			},
			wantAlert: "Author name can't be blank",
		},
		{
			name: "blank content",
			extra: func(query.Comment) url.Values {
				return url.Values{"article_id": {"hello-world"}, "comment[content]": {"  "}}
			},
			wantAlert: "Content can't be blank",
		},
		{
			name: "bad email",
			extra: func(query.Comment) url.Values {
				return url.Values{"article_id": {"hello-world"}, "comment[author_email]": {"nope"}}
			},
			wantAlert: "Author email must be a valid email",
		},
		{
			name: "bad author url",
			extra: func(query.Comment) url.Values {
				return url.Values{"article_id": {"hello-world"}, "comment[author_url]": {"javascript:x"}}
			},
			wantAlert: "Author url must be a valid URL",
		},
		{
			name: "parent of another commentable",
			extra: func(otherComment query.Comment) url.Values {
				return url.Values{"article_id": {"hello-world"},
					"comment[parent_id]": {strconv.FormatInt(otherComment.ID, 10)}}
			},
			wantAlert: "Parent must belong to the same Article",
		},
		{
			name: "missing parent",
			extra: func(query.Comment) url.Values {
				return url.Values{"article_id": {"hello-world"}, "comment[parent_id]": {"999"}}
			},
			wantAlert: "Parent does not exist",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, h, otherComment := newServer(t)
			before := commentCount(t, s)
			rec := doRequest(t, h, http.MethodPost, "/comments", validCommentForm(t, tt.extra(otherComment)))
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/hello-world" {
				t.Fatalf("status = %d location = %q, want 302 /hello-world", rec.Code, rec.Header().Get("Location"))
			}
			if flash := readFlash(t, rec); !strings.Contains(flash.Alert, tt.wantAlert) {
				t.Errorf("alert = %q, want it to contain %q", flash.Alert, tt.wantAlert)
			}
			if got := commentCount(t, s); got != before {
				t.Error("invalid comment was stored")
			}
		})
	}
}

func TestCommentSubmitRateLimited(t *testing.T) {
	s, h := newCommentTestServer(t)
	insertArticle(t, s, "hello-world", 1, 1)

	for i := 1; i <= 5; i++ {
		rec := doRequest(t, h, http.MethodPost, "/comments", validCommentForm(t, url.Values{"article_id": {"hello-world"}}))
		if rec.Code != http.StatusFound {
			t.Fatalf("request %d: status = %d, want 302", i, rec.Code)
		}
	}
	rec := doRequest(t, h, http.MethodPost, "/comments", validCommentForm(t, url.Values{"article_id": {"hello-world"}}))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th request: status = %d, want 429", rec.Code)
	}
	if got := commentCount(t, s); got != 5 {
		t.Errorf("comments = %d, want 5", got)
	}
}
