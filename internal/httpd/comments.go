package httpd

import (
	"database/sql"
	"errors"
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
	"rables/internal/service/comments"
	"rables/internal/templates"
)

// commentCreateBurst mirrors the comment submission budget:
// rate_limit to: 5, within: 3.minutes (plan section 4.5).
const commentCreateBurst = 5

// RegisterCommentRoutes mounts the public comment submission endpoint
// (Rails: resources :comments, only: [:create]). Wired into NewRouter by
// the integrator.
func RegisterCommentRoutes(r chi.Router, s *Server) {
	limiter := NewIPRateLimiter(rate.Every(3*time.Minute/commentCreateBurst), commentCreateBurst)
	r.With(RateLimit(limiter, ClientIP)).Post("/comments", s.createComment)
}

// Enqueuer returns the shared job enqueuer, creating it on first use.
// Later features (T09/T14/...) reuse it through this accessor.
func (s *Server) Enqueuer() *jobs.Enqueuer {
	v, _ := s.Ext.LoadOrStore("enqueuer", jobs.NewEnqueuer(s.DB))
	return v.(*jobs.Enqueuer)
}

// errCommentableNotFound mirrors the RecordNotFound raised by
// set_commentable (unknown slug, or comments closed on the target).
var errCommentableNotFound = errors.New("commentable not found")

// commentableTarget is the resolved article/page a comment belongs to.
type commentableTarget struct {
	typ          string // "Article" | "Page"
	id           int64
	redirectPath string // public URL the form redirects back to
}

// createComment handles POST /comments, mirroring CommentsController#create
// (HTML form path only): captcha check, moderation-safe defaults, redirect
// back with a flash. Successful submissions are always pending.
func (s *Server) createComment(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	target, err := s.resolveCommentable(r)
	if err != nil {
		SetFlash(w, templates.Flash{Alert: "Article or page not found."})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	back := func(alert string) {
		SetFlash(w, templates.Flash{Alert: alert})
		http.Redirect(w, r, target.redirectPath, http.StatusFound)
	}

	cap := captcha.New(s.Cfg.HMACSecret, captcha.TTL)
	token, answer := r.FormValue("captcha[token]"), r.FormValue("captcha[answer]")
	if _, ok := cap.Expected(token); !ok {
		back("验证已过期：请刷新页面后重新回答数学题。")
		return
	}
	if !cap.Verify(token, answer) {
		back("验证失败：请回答数学题。")
		return
	}

	now := time.Now().UTC().Unix()
	comment := query.Comment{
		CommentableType: sql.NullString{String: target.typ, Valid: true},
		CommentableID:   sql.NullInt64{Int64: target.id, Valid: true},
		AuthorName:      r.FormValue("comment[author_name]"),
		AuthorEmail:     sql.NullString{String: r.FormValue("comment[author_email]"), Valid: r.FormValue("comment[author_email]") != ""},
		AuthorUrl:       sql.NullString{String: r.FormValue("comment[author_url]"), Valid: r.FormValue("comment[author_url]") != ""},
		Content:         r.FormValue("comment[content]"),
		Status:          int64(domain.CommentPending), // manual approval required
		PublishedAt:     sql.NullInt64{Int64: now, Valid: true},
	}
	// A non-numeric parent_id type-casts to nil in Rails (top-level comment).
	var parent *query.Comment
	if raw := strings.TrimSpace(r.FormValue("comment[parent_id]")); raw != "" {
		if parentID, err := strconv.ParseInt(raw, 10, 64); err == nil && parentID > 0 {
			comment.ParentID = sql.NullInt64{Int64: parentID, Valid: true}
			if p, err := s.Q.GetCommentByID(r.Context(), parentID); err == nil {
				parent = &p
			}
		}
	}
	if msgs := comments.ValidateNew(comment, parent); len(msgs) > 0 {
		back("提交评论时出错：" + strings.Join(msgs, "，"))
		return
	}

	if _, err := s.Q.CreateComment(r.Context(), query.CreateCommentParams{
		CommentableType: comment.CommentableType,
		CommentableID:   comment.CommentableID,
		ParentID:        comment.ParentID,
		AuthorName:      comment.AuthorName,
		AuthorEmail:     comment.AuthorEmail,
		AuthorUrl:       comment.AuthorUrl,
		Content:         comment.Content,
		Status:          comment.Status,
		PublishedAt:     comment.PublishedAt,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		s.Log.Error("create comment", "error", err)
		back("提交评论时发生错误，请稍后重试。")
		return
	}

	SetFlash(w, templates.Flash{Notice: "Your comment will be reviewed before being published."})
	http.Redirect(w, r, target.redirectPath, http.StatusFound)
}

// resolveCommentable mirrors set_commentable: find the article/page by slug
// and accept comments only for publish/shared content with comment=1.
func (s *Server) resolveCommentable(r *http.Request) (commentableTarget, error) {
	if slug := r.FormValue("article_id"); slug != "" {
		article, err := s.Q.GetCommentableArticleBySlug(r.Context(), sql.NullString{String: slug, Valid: true})
		if err != nil {
			return commentableTarget{}, err
		}
		if !comments.AcceptsComments(article.Status, article.Comment) {
			return commentableTarget{}, errCommentableNotFound
		}
		return commentableTarget{
			typ:          "Article",
			id:           article.ID,
			redirectPath: comments.ArticlePath(s.Cfg.ArticleRoutePrefix, slug),
		}, nil
	}
	if slug := r.FormValue("page_id"); slug != "" {
		page, err := s.Q.GetCommentablePageBySlug(r.Context(), sql.NullString{String: slug, Valid: true})
		if err != nil {
			return commentableTarget{}, err
		}
		if !comments.AcceptsComments(page.Status, page.Comment) {
			return commentableTarget{}, errCommentableNotFound
		}
		return commentableTarget{
			typ:          "Page",
			id:           page.ID,
			redirectPath: comments.PagePath(slug),
		}, nil
	}
	return commentableTarget{}, errCommentableNotFound
}
