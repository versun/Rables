package httpd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/service/comments"
	"rables/internal/templates"
)

// adminCommentsPerPage mirrors the will_paginate per_page of the Rails index.
const adminCommentsPerPage = 30

// RegisterCommentAdminRoutes mounts the comment moderation UI (Rails:
// resources :comments under /admin with member approve/reject/reply and
// collection batch_*; PATCH member actions become POST because HTML forms
// cannot send PATCH). Wired into NewRouter by the integrator.
func RegisterCommentAdminRoutes(r chi.Router, s *Server) {
	r.With(s.RequireAuth).Get("/admin/comments", s.adminCommentsIndex)
	r.With(s.RequireAuth).Post("/admin/comments/batch_destroy", s.adminBatchDestroyComments)
	r.With(s.RequireAuth).Post("/admin/comments/batch_approve", s.adminBatchApproveComments)
	r.With(s.RequireAuth).Post("/admin/comments/batch_reject", s.adminBatchRejectComments)
	r.With(s.RequireAuth).Post("/admin/comments/{id}/approve", s.adminApproveComment)
	r.With(s.RequireAuth).Post("/admin/comments/{id}/reject", s.adminRejectComment)
	r.With(s.RequireAuth).Post("/admin/comments/{id}/reply", s.adminReplyComment)
}

// adminCommentsPage feeds admin_comments_index.html.
type adminCommentsPage struct {
	Flash      templates.Flash
	Status     string // current status filter ("all" when unset)
	Comments   []adminCommentRow
	Dir        string // current date sort direction: "desc" (default) or "asc"
	Page       int
	Pages      int
	TimeZone   string
	SiteAuthor string
	SiteURL    string
}

// filterValues carries every active filter/sort param, omitting defaults so
// URLs stay bare. It is the single source for list links.
func (d adminCommentsPage) filterValues() url.Values {
	v := url.Values{}
	if d.Status != "" && d.Status != "all" {
		v.Set("status", d.Status)
	}
	if d.Dir != "desc" {
		v.Set("dir", d.Dir)
	}
	return v
}

// listURL renders a list link preserving every current filter/sort param;
// mutate adjusts the query for the specific link (page jump, filter change,
// sort toggle). Filters/sorts never carry the page over: changing them
// restarts at page 1.
func (d adminCommentsPage) listURL(mutate func(url.Values)) string {
	v := d.filterValues()
	if mutate != nil {
		mutate(v)
	}
	if len(v) == 0 {
		return "/admin/comments"
	}
	return "/admin/comments?" + v.Encode()
}

// PageURL is the pagination link for one page; page 1 stays the bare URL.
func (d adminCommentsPage) PageURL(page int) string {
	return d.listURL(func(v url.Values) {
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
	})
}

// DateURL toggles the Date header sort direction.
func (d adminCommentsPage) DateURL() string {
	return d.listURL(func(v url.Values) {
		if d.Dir == "desc" {
			v.Set("dir", "asc")
		} else {
			v.Set("dir", "desc")
		}
	})
}

// StatusURL filters by one status ("all" clears the filter).
func (d adminCommentsPage) StatusURL(status string) string {
	return d.listURL(func(v url.Values) {
		if status == "all" {
			v.Del("status")
		} else {
			v.Set("status", status)
		}
	})
}

// adminCommentRow is one list row with its display_commentable resolved.
type adminCommentRow struct {
	Comment          query.Comment
	CommentableTitle string
	CommentableURL   string
}

// IsLocal reports a native (non-platform) comment.
func (row adminCommentRow) IsLocal() bool { return !row.Comment.Platform.Valid }

// PlatformTitle is the titleized platform name (via Mastodon, ...).
func (row adminCommentRow) PlatformTitle() string {
	p := row.Comment.Platform.String
	if p == "" {
		return ""
	}
	return strings.ToUpper(p[:1]) + p[1:]
}

// TruncatedContent mirrors content.truncate(100) (omission included).
func (row adminCommentRow) TruncatedContent() string { return truncateRunes(row.Comment.Content, 100) }

// TruncatedTitle mirrors display_title.to_s.truncate(50).
func (row adminCommentRow) TruncatedTitle() string { return truncateRunes(row.CommentableTitle, 50) }

// DateUnix is published_at with the created_at fallback of the Rails view.
func (row adminCommentRow) DateUnix() int64 {
	if row.Comment.PublishedAt.Valid {
		return row.Comment.PublishedAt.Int64
	}
	return row.Comment.CreatedAt
}

// truncateRunes mirrors String#truncate: at most n runes, "..." included.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-3]) + "..."
}

// adminCommentsIndex renders GET /admin/comments, mirroring
// Admin::CommentsController#index: optional status filter in the Status
// header, sortable Date header (COALESCE(published_at, created_at) DESC by
// default), 30 per page. Invalid page params 404 like
// WillPaginate::InvalidPage.
func (s *Server) adminCommentsIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	statusName := r.URL.Query().Get("status")
	statusFilter, filtered := parseCommentStatusFilter(statusName)
	if !filtered {
		// Unknown statuses filter nothing, so the view must not present them
		// as an active filter either.
		statusName = "all"
	}
	statusValue := int64(-1) // -1: no status filter
	if filtered {
		statusValue = statusFilter
	}

	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || int64(n) > maxAdminPageNumber {
			http.NotFound(w, r)
			return
		}
		page = n
	}
	offset := int64(page-1) * adminCommentsPerPage

	dir := r.URL.Query().Get("dir")
	if dir != "asc" {
		dir = "desc" // unknown directions fall back to the default DESC order
	}

	total, err := s.Q.CountAdminCommentsFiltered(ctx, statusValue)
	if err != nil {
		s.Log.Error("count comments", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var list []query.Comment
	if dir == "asc" {
		list, err = s.Q.ListAdminCommentsFilteredDateAsc(ctx, query.ListAdminCommentsFilteredDateAscParams{
			StatusFilter: statusValue, Limit: adminCommentsPerPage, Offset: offset,
		})
	} else {
		list, err = s.Q.ListAdminCommentsFilteredDateDesc(ctx, query.ListAdminCommentsFilteredDateDescParams{
			StatusFilter: statusValue, Limit: adminCommentsPerPage, Offset: offset,
		})
	}
	if err != nil {
		s.Log.Error("list comments", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	settings, err := s.Settings().Get(ctx)
	if err != nil {
		s.Log.Error("load settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rows := make([]adminCommentRow, 0, len(list))
	for _, c := range list {
		title, url := s.displayCommentable(ctx, c)
		rows = append(rows, adminCommentRow{Comment: c, CommentableTitle: title, CommentableURL: url})
	}

	pages := int((total + adminCommentsPerPage - 1) / adminCommentsPerPage)
	s.render(w, http.StatusOK, "admin_comments_index", adminCommentsPage{
		Flash:      s.PopFlash(r, w),
		Status:     statusName,
		Comments:   rows,
		Dir:        dir,
		Page:       page,
		Pages:      pages,
		TimeZone:   settings.TimeZone,
		SiteAuthor: strings.TrimSpace(settings.Author.String),
		SiteURL:    strings.TrimSpace(settings.Url.String),
	})
}

// parseCommentStatusFilter mirrors the status filter: only the three
// known statuses filter; anything else (including "all") lists everything.
func parseCommentStatusFilter(name string) (int64, bool) {
	switch name {
	case "pending":
		return int64(domain.CommentPending), true
	case "approved":
		return int64(domain.CommentApproved), true
	case "rejected":
		return int64(domain.CommentRejected), true
	}
	return 0, false
}

// displayCommentable resolves the row's public title and URL, mirroring
// Comment#display_commentable: commentable, else the parent's commentable,
// else the legacy article association.
func (s *Server) displayCommentable(ctx context.Context, c query.Comment) (title, url string) {
	typ, id := c.CommentableType, c.CommentableID
	if (!typ.Valid || !id.Valid) && c.ParentID.Valid {
		if parent, err := s.Q.GetCommentByID(ctx, c.ParentID.Int64); err == nil {
			typ, id = parent.CommentableType, parent.CommentableID
		}
	}
	if typ.Valid && id.Valid {
		switch typ.String {
		case "Article":
			if a, err := s.Q.GetCommentableArticleByID(ctx, id.Int64); err == nil {
				return titleOrSlug(a.Title, a.Slug), comments.ArticlePath(s.routePrefix(ctx), a.Slug.String)
			}
		case "Page":
			if p, err := s.Q.GetCommentablePageByID(ctx, id.Int64); err == nil {
				return titleOrSlug(p.Title, p.Slug), comments.PagePath(p.Slug.String)
			}
		}
		return "", ""
	}
	if c.ArticleID.Valid {
		if a, err := s.Q.GetCommentableArticleByID(ctx, c.ArticleID.Int64); err == nil {
			return titleOrSlug(a.Title, a.Slug), comments.ArticlePath(s.routePrefix(ctx), a.Slug.String)
		}
	}
	return "", ""
}

func titleOrSlug(title, slug sql.NullString) string {
	if title.String != "" {
		return title.String
	}
	return slug.String
}

// commentIDParam extracts the {id} route param; zero means malformed.
func commentIDParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id
}

// backToAdminComments redirects to the moderation list with a flash.
func (s *Server) backToAdminComments(w http.ResponseWriter, r *http.Request, flash templates.Flash) {
	s.SetFlash(w, flash)
	http.Redirect(w, r, "/admin/comments", http.StatusFound)
}

// adminApproveComment handles POST /admin/comments/{id}/approve (Rails PATCH).
func (s *Server) adminApproveComment(w http.ResponseWriter, r *http.Request) {
	s.moderateComment(w, r, domain.CommentApproved)
}

// adminRejectComment handles POST /admin/comments/{id}/reject (Rails PATCH).
func (s *Server) adminRejectComment(w http.ResponseWriter, r *http.Request) {
	s.moderateComment(w, r, domain.CommentRejected)
}

// moderateComment sets one comment's status, mirroring approve/reject.
func (s *Server) moderateComment(w http.ResponseWriter, r *http.Request, st domain.CommentStatus) {
	verb := "approve"
	if st == domain.CommentRejected {
		verb = "reject"
	}
	c, err := s.Q.GetCommentByID(r.Context(), commentIDParam(r))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get comment", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	updated, err := s.Q.UpdateCommentStatus(r.Context(), query.UpdateCommentStatusParams{
		Status:    int64(st),
		UpdatedAt: time.Now().UTC().Unix(),
		ID:        c.ID,
	})
	if err != nil {
		s.Log.Error("moderate comment", "error", err)
		s.backToAdminComments(w, r, templates.Flash{Alert: fmt.Sprintf("Failed to %s comment.", verb)})
		return
	}
	s.notifyReplyApproved(r, c, updated)
	if st == domain.CommentApproved {
		s.backToAdminComments(w, r, templates.Flash{Notice: "Comment approved successfully."})
	} else {
		s.backToAdminComments(w, r, templates.Flash{Notice: "Comment rejected."})
	}
}

// notifyReplyApproved queues the reply-notification job when a local reply
// transitioned to approved and its parent left an email (the model's
// after_commit on saved_change_to_status).
func (s *Server) notifyReplyApproved(r *http.Request, before, after query.Comment) {
	if before.Status == int64(domain.CommentApproved) {
		return // no status transition to approved
	}
	if _, err := comments.EnqueueReplyNotification(r.Context(), s.Q, s.Enqueuer(), after); err != nil {
		s.Log.Error("enqueue reply notification", "error", err)
	}
}

// adminBatchApproveComments handles POST /admin/comments/batch_approve.
func (s *Server) adminBatchApproveComments(w http.ResponseWriter, r *http.Request) {
	s.batchModerateComments(w, r, domain.CommentApproved)
}

// adminBatchRejectComments handles POST /admin/comments/batch_reject.
func (s *Server) adminBatchRejectComments(w http.ResponseWriter, r *http.Request) {
	s.batchModerateComments(w, r, domain.CommentRejected)
}

// batchModerateComments mirrors batch_approve/batch_reject: best-effort per
// id, reporting how many succeeded.
func (s *Server) batchModerateComments(w http.ResponseWriter, r *http.Request, st domain.CommentStatus) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	verb := "approved"
	if st == domain.CommentRejected {
		verb = "rejected"
	}
	count := 0
	for _, id := range commentIDsFromForm(r) {
		c, err := s.Q.GetCommentByID(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			s.Log.Error("batch moderate comment", "id", id, "error", err)
			continue
		}
		updated, err := s.Q.UpdateCommentStatus(r.Context(), query.UpdateCommentStatusParams{
			Status:    int64(st),
			UpdatedAt: time.Now().UTC().Unix(),
			ID:        c.ID,
		})
		if err != nil {
			s.Log.Error("batch moderate comment", "id", id, "error", err)
			continue
		}
		count++
		s.notifyReplyApproved(r, c, updated)
	}
	s.backToAdminComments(w, r, templates.Flash{Notice: fmt.Sprintf("Successfully %s %d comment(s).", verb, count)})
}

// adminBatchDestroyComments mirrors batch_destroy: delete each found id
// (replies cascade via ON DELETE CASCADE), reporting the count.
func (s *Server) adminBatchDestroyComments(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	count := 0
	for _, id := range commentIDsFromForm(r) {
		_, err := s.Q.GetCommentByID(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			s.Log.Error("batch destroy comment", "id", id, "error", err)
			continue
		}
		if err := s.Q.DeleteComment(r.Context(), id); err != nil {
			s.Log.Error("batch destroy comment", "id", id, "error", err)
			continue
		}
		count++
	}
	s.backToAdminComments(w, r, templates.Flash{Notice: fmt.Sprintf("Successfully deleted %d comment(s).", count)})
}

// commentIDsFromForm parses the ids[] checkbox values; malformed entries are
// skipped like Rails' find_by returning nil.
func commentIDsFromForm(r *http.Request) []int64 {
	var ids []int64
	for _, raw := range r.PostForm["ids"] {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

// adminReplyComment handles POST /admin/comments/{id}/reply, mirroring
// Admin::CommentsController#reply: an approved reply under the site author's
// name; external or rejected comments cannot be answered.
func (s *Server) adminReplyComment(w http.ResponseWriter, r *http.Request) {
	fail := func(alert string) { s.backToAdminComments(w, r, templates.Flash{Alert: alert}) }

	c, err := s.Q.GetCommentByID(r.Context(), commentIDParam(r))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get comment", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if c.Platform.Valid {
		fail("Cannot reply to external comments.")
		return
	}
	if c.Status == int64(domain.CommentRejected) {
		fail("Cannot reply to rejected comments.")
		return
	}
	if !c.CommentableType.Valid || !c.CommentableID.Valid {
		fail("Commentable not found.")
		return
	}
	exists, err := s.commentableExists(r, c)
	if err != nil {
		s.Log.Error("check commentable", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !exists {
		fail("Commentable not found.")
		return
	}

	settings, err := s.Settings().Get(r.Context())
	if err != nil {
		s.Log.Error("load settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	author := strings.TrimSpace(settings.Author.String)
	if author == "" {
		fail("Please set the site author name in Settings before replying.")
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Unix()
	reply := query.Comment{
		CommentableType: c.CommentableType,
		CommentableID:   c.CommentableID,
		ParentID:        sql.NullInt64{Int64: c.ID, Valid: true},
		AuthorName:      author,
		AuthorUrl:       sql.NullString{String: replyAuthorURL(settings.Url.String), Valid: replyAuthorURL(settings.Url.String) != ""},
		Content:         r.FormValue("comment[content]"),
		Status:          int64(domain.CommentApproved),
		PublishedAt:     sql.NullInt64{Int64: now, Valid: true},
	}
	if msgs := comments.ValidateNew(reply, &c); len(msgs) > 0 {
		fail("Failed to reply: " + strings.Join(msgs, ", "))
		return
	}
	created, err := s.Q.CreateComment(r.Context(), query.CreateCommentParams{
		CommentableType: reply.CommentableType,
		CommentableID:   reply.CommentableID,
		ParentID:        reply.ParentID,
		AuthorName:      reply.AuthorName,
		AuthorUrl:       reply.AuthorUrl,
		Content:         reply.Content,
		Status:          reply.Status,
		PublishedAt:     reply.PublishedAt,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		s.Log.Error("create reply", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The reply is created approved, so the parent gets the notification
	// (comment.rb enqueue_reply_notification on create).
	if _, err := comments.EnqueueReplyNotification(r.Context(), s.Q, s.Enqueuer(), created); err != nil {
		s.Log.Error("enqueue reply notification", "error", err)
	}
	s.backToAdminComments(w, r, templates.Flash{Notice: "Reply posted successfully."})
}

// commentableExists reports whether the comment's commentable row is present.
// A missing row is (false, nil); a real DB error is (false, err).
func (s *Server) commentableExists(r *http.Request, c query.Comment) (bool, error) {
	var err error
	switch c.CommentableType.String {
	case "Article":
		_, err = s.Q.GetCommentableArticleByID(r.Context(), c.CommentableID.Int64)
	case "Page":
		_, err = s.Q.GetCommentablePageByID(r.Context(), c.CommentableID.Int64)
	default:
		return false, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// replyAuthorURL mirrors reply_author_url: blank stays blank, otherwise the
// site URL with the trailing slash chomped and an https:// scheme default.
func replyAuthorURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(u, "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return u
}
