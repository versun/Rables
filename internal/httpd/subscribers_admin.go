package httpd

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/domain"
	subscribersvc "rables/internal/service/subscribers"
	tagsvc "rables/internal/service/tags"
	"rables/internal/templates"
)

// adminSubscribersPerPage mirrors the will_paginate per_page of the Rails
// subscribers index.
const adminSubscribersPerPage = 30

// RegisterSubscriberAdminRoutes mounts the admin subscriber pages behind
// RequireAuth, mirroring namespace :admin resources :subscribers (index,
// destroy) with collection batch_create/batch_confirm/batch_destroy. HTML
// forms cannot DELETE, so destroy maps to POST /admin/subscribers/{id}/destroy.
// Wired into NewRouter by the integrator.
func RegisterSubscriberAdminRoutes(r chi.Router, s *Server) {
	r.Route("/admin/subscribers", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminSubscribersIndex)
		r.Post("/batch_create", s.adminSubscribersBatchCreate)
		r.Post("/batch_confirm", s.adminSubscribersBatchConfirm)
		r.Post("/batch_destroy", s.adminSubscribersBatchDestroy)
		r.Post("/{id}/destroy", s.adminSubscribersDestroy)
	})
}

// adminSubscriberRow is one subscriber with its tags for the index table.
type adminSubscriberRow struct {
	query.Subscriber
	Tags []query.Tag
}

// StatusLabel mirrors the status badge text of the Rails index view.
func (row adminSubscriberRow) StatusLabel() string {
	switch {
	case subscribersvc.Active(row.Subscriber):
		return "已确认"
	case subscribersvc.Confirmed(row.Subscriber):
		return "已取消"
	default:
		return "待确认"
	}
}

// StatusBadgeClass mirrors the status badge CSS class of the Rails index view.
func (row adminSubscriberRow) StatusBadgeClass() string {
	switch {
	case subscribersvc.Active(row.Subscriber):
		return "badge-success"
	case subscribersvc.Confirmed(row.Subscriber):
		return "badge-warning"
	default:
		return "badge-secondary"
	}
}

// TagNames joins the tag names like tags.map(&:name).join(", "); empty means
// subscribed to all content.
func (row adminSubscriberRow) TagNames() string {
	names := make([]string, len(row.Tags))
	for i, t := range row.Tags {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// adminSubscribersPage feeds admin_subscribers_index.html.
type adminSubscribersPage struct {
	Flash       templates.Flash
	Status      string
	TagIDs      []int64
	IncludeAll  bool
	Tags        []query.Tag // filter multi-select options (Tag.alphabetical)
	Subscribers []adminSubscriberRow
	Page        int
	Pages       int
	FilterQuery string // filter params carried by the pagination links
	TimeZone    string
}

// TagSelected reports whether a tag is checked in the filter form.
func (p adminSubscribersPage) TagSelected(id int64) bool {
	for _, tagID := range p.TagIDs {
		if tagID == id {
			return true
		}
	}
	return false
}

// adminSubscribersIndex renders GET /admin/subscribers, mirroring
// Admin::SubscribersController#index: status filter, tag filter
// (tag_ids / include_all), created_at DESC, 30 per page. The subscriber
// table is small, so the tag filter and the page window are applied over
// the ordered status-filtered list (see queries/subscribers.sql).
func (s *Server) adminSubscribersIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "all"
	}
	var tagIDs []int64
	for _, raw := range r.URL.Query()["tag_ids[]"] {
		if domain.IsBlank(raw) {
			continue
		}
		tagIDs = append(tagIDs, rubyToI(raw))
	}
	includeAll := r.URL.Query().Get("include_all") == "1"
	page, ok := parseStrictPage(r.URL.Query())
	if !ok {
		http.NotFound(w, r)
		return
	}

	// apply_status_filter; any other status value means unfiltered.
	sqlStatus := ""
	switch status {
	case "active", "unconfirmed", "unsubscribed":
		sqlStatus = status
	}
	rows, err := s.Q.ListAdminSubscribers(ctx, sqlStatus)
	if err != nil {
		s.Log.Error("list subscribers", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// apply_subscription_filter
	var candidates map[int64]bool
	if len(tagIDs) > 0 || includeAll {
		candidates = make(map[int64]bool)
		for _, tagID := range tagIDs {
			ids, err := s.Q.ListSubscriberIDsByTagID(ctx, tagID)
			if err != nil {
				s.Log.Error("list subscribers by tag", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			for _, id := range ids {
				candidates[id] = true
			}
		}
		if includeAll {
			ids, err := s.Q.ListSubscriberIDsWithoutTags(ctx)
			if err != nil {
				s.Log.Error("list tagless subscribers", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			for _, id := range ids {
				candidates[id] = true
			}
		}
	}
	var filtered []query.Subscriber
	for _, row := range rows {
		if candidates != nil && !candidates[row.ID] {
			continue
		}
		filtered = append(filtered, row)
	}

	total := int64(len(filtered))
	start := (page - 1) * adminSubscribersPerPage
	window := filtered
	if start >= total {
		window = nil // will_paginate renders an empty out-of-range page
	} else {
		end := start + adminSubscribersPerPage
		if end > total {
			end = total
		}
		window = filtered[start:end]
	}

	tags, err := s.Q.ListPublicTags(ctx)
	if err != nil {
		s.Log.Error("list tags", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	st, err := s.Settings().Get(ctx)
	if err != nil {
		s.Log.Error("load settings", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	pageRows := make([]adminSubscriberRow, 0, len(window))
	for _, sub := range window {
		subTags, err := s.Q.ListTagsForSubscriber(ctx, sub.ID)
		if err != nil {
			s.Log.Error("list subscriber tags", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		pageRows = append(pageRows, adminSubscriberRow{Subscriber: sub, Tags: subTags})
	}

	filter := url.Values{}
	filter.Set("status", status)
	for _, id := range tagIDs {
		filter.Add("tag_ids[]", strconv.FormatInt(id, 10))
	}
	if includeAll {
		filter.Set("include_all", "1")
	}

	s.render(w, http.StatusOK, "admin_subscribers_index", adminSubscribersPage{
		Flash:       PopFlash(r, w),
		Status:      status,
		TagIDs:      tagIDs,
		IncludeAll:  includeAll,
		Tags:        tags,
		Subscribers: pageRows,
		Page:        int(page),
		Pages:       int((total + adminSubscribersPerPage - 1) / adminSubscribersPerPage),
		FilterQuery: filter.Encode(),
		TimeZone:    st.TimeZone,
	})
}

// adminSubscribersDestroy handles POST /admin/subscribers/{id}/destroy
// (Rails DELETE /admin/subscribers/:id), mirroring
// Admin::SubscribersController#destroy.
func (s *Server) adminSubscribersDestroy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := tagIDParam(r)
	sub, err := s.Q.GetSubscriberByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get subscriber", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := subscribersvc.Destroy(ctx, s.DB, id); err != nil {
		s.Log.Error("destroy subscriber", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logSubscriptionActivity(ctx, "deleted", "subscriber", 0, "email="+activityQuote(sub.Email))
	SetFlash(w, templates.Flash{Notice: "订阅者已删除。"})
	http.Redirect(w, r, "/admin/subscribers", http.StatusSeeOther)
}

// adminSubscribersBatchCreate handles POST /admin/subscribers/batch_create,
// mirroring Admin::SubscribersController#batch_create: one email per line,
// optional comma-separated tag names; new subscribers are auto-confirmed.
func (s *Server) adminSubscribersBatchCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	emailsText := r.FormValue("emails_text")
	if domain.IsBlank(emailsText) {
		SetFlash(w, templates.Flash{Alert: "请输入邮箱地址。"})
		http.Redirect(w, r, "/admin/subscribers", http.StatusFound)
		return
	}

	var newCount, updatedCount, skippedCount, errorCount int
	var errs []string
	for _, line := range strings.Split(emailsText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: email,tag1,tag2 or plain email. A line of only commas
		// splits to nothing (Ruby split drops trailing empty fields), which
		// Rails reports as an invalid blank email.
		parts := rubySplitComma(line)
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		email, tagNames := "", []string(nil)
		if len(parts) > 0 {
			email, tagNames = parts[0], parts[1:]
		}

		if !subscribersvc.ValidEmail(email) {
			errorCount++
			errs = append(errs, "无效的邮箱格式: "+email)
			continue
		}

		sub, err := s.Q.GetSubscriberByEmail(ctx, email)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			sub, err = subscribersvc.Create(ctx, s.Q, email, "", "")
			if err != nil {
				errorCount++
				errs = append(errs, email+": "+err.Error())
				continue
			}
			// New subscribers are auto-confirmed; no confirmation email.
			if err := s.Q.ConfirmSubscriber(ctx, query.ConfirmSubscriberParams{
				ConfirmedAt: sql.NullInt64{Int64: time.Now().UTC().Unix(), Valid: true},
				UpdatedAt:   time.Now().UTC().Unix(),
				ID:          sub.ID,
			}); err != nil {
				s.Log.Error("confirm subscriber", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			tagIDs, err := tagsvc.FindOrCreateByNames(ctx, s.Q, tagNames)
			if err != nil {
				s.Log.Error("find or create tags", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if err := subscribersvc.ReplaceTags(ctx, s.Q, sub.ID, tagIDs); err != nil {
				s.Log.Error("replace subscriber tags", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			newCount++
		case err != nil:
			s.Log.Error("find subscriber", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		default:
			// Existing subscribers keep their tags unless the line explicitly
			// provides new ones.
			if len(tagNames) > 0 {
				tagIDs, err := tagsvc.FindOrCreateByNames(ctx, s.Q, tagNames)
				if err != nil {
					s.Log.Error("find or create tags", "error", err)
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				if err := subscribersvc.ReplaceTags(ctx, s.Q, sub.ID, tagIDs); err != nil {
					s.Log.Error("replace subscriber tags", "error", err)
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				updatedCount++
			} else {
				skippedCount++
			}
		}
	}

	handled := newCount + updatedCount + skippedCount
	if handled == 0 {
		s.logSubscriptionActivity(ctx, "failed", "subscriber", 2,
			fmt.Sprintf("error_count=%d errors=%s", errorCount, activityQuote(strings.Join(errs, "; "))))
		SetFlash(w, templates.Flash{Alert: "添加失败: " + strings.Join(errs, "; ")})
		http.Redirect(w, r, "/admin/subscribers", http.StatusFound)
		return
	}

	level := int64(0)
	if errorCount > 0 {
		level = 1
	}
	s.logSubscriptionActivity(ctx, "created", "subscriber", level, fmt.Sprintf(
		"success_count=%d error_count=%d errors=%s skipped_count=%d updated_count=%d",
		newCount, errorCount, activityQuote(strings.Join(errs, "; ")), skippedCount, updatedCount))
	notice := fmt.Sprintf("成功添加 %d 个订阅者。", newCount)
	if updatedCount > 0 {
		notice += fmt.Sprintf(" %d 个已存在并更新 tags。", updatedCount)
	}
	if skippedCount > 0 {
		notice += fmt.Sprintf(" %d 个已存在跳过。", skippedCount)
	}
	if errorCount > 0 {
		notice += fmt.Sprintf(" %d 个失败。", errorCount)
	}
	SetFlash(w, templates.Flash{Notice: notice})
	http.Redirect(w, r, "/admin/subscribers", http.StatusFound)
}

// rubySplitComma mirrors Ruby's String#split(","): trailing empty fields are
// dropped (inner blanks are kept).
func rubySplitComma(s string) []string {
	parts := strings.Split(s, ",")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// adminSubscribersBatchConfirm handles POST /admin/subscribers/batch_confirm,
// mirroring Admin::SubscribersController#batch_confirm: only pending
// addresses are confirmed; unsubscribed ones are never reactivated.
func (s *Server) adminSubscribersBatchConfirm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	count := 0
	for _, raw := range batchIDs(r) {
		if domain.IsBlank(raw) {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			continue
		}
		sub, err := s.Q.GetSubscriberByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			s.Log.Error("get subscriber", "error", err)
			SetFlash(w, templates.Flash{Alert: "批量确认失败: " + err.Error()})
			http.Redirect(w, r, "/admin/subscribers", http.StatusFound)
			return
		}
		if subscribersvc.Confirmed(sub) || sub.UnsubscribedAt.Valid {
			continue
		}
		if err := s.Q.ConfirmSubscriber(ctx, query.ConfirmSubscriberParams{
			ConfirmedAt: sql.NullInt64{Int64: time.Now().UTC().Unix(), Valid: true},
			UpdatedAt:   time.Now().UTC().Unix(),
			ID:          sub.ID,
		}); err != nil {
			s.Log.Error("confirm subscriber", "error", err)
			SetFlash(w, templates.Flash{Alert: "批量确认失败: " + err.Error()})
			http.Redirect(w, r, "/admin/subscribers", http.StatusFound)
			return
		}
		count++
	}
	s.logSubscriptionActivity(ctx, "updated", "subscriber", 0, fmt.Sprintf("count=%d", count))
	SetFlash(w, templates.Flash{Notice: fmt.Sprintf("已确认 %d 个订阅者。", count)})
	http.Redirect(w, r, "/admin/subscribers", http.StatusFound)
}

// adminSubscribersBatchDestroy handles POST /admin/subscribers/batch_destroy,
// mirroring Admin::SubscribersController#batch_destroy: missing records are
// skipped.
func (s *Server) adminSubscribersBatchDestroy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	count := 0
	for _, raw := range batchIDs(r) {
		if domain.IsBlank(raw) {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			continue
		}
		if _, err := s.Q.GetSubscriberByID(ctx, id); errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			s.Log.Error("get subscriber", "error", err)
			SetFlash(w, templates.Flash{Alert: "批量删除失败: " + err.Error()})
			http.Redirect(w, r, "/admin/subscribers", http.StatusFound)
			return
		}
		if err := subscribersvc.Destroy(ctx, s.DB, id); err != nil {
			s.Log.Error("destroy subscriber", "error", err)
			SetFlash(w, templates.Flash{Alert: "批量删除失败: " + err.Error()})
			http.Redirect(w, r, "/admin/subscribers", http.StatusFound)
			return
		}
		count++
	}
	s.logSubscriptionActivity(ctx, "deleted", "subscriber", 0, fmt.Sprintf("count=%d", count))
	SetFlash(w, templates.Flash{Notice: fmt.Sprintf("已删除 %d 个订阅者。", count)})
	http.Redirect(w, r, "/admin/subscribers", http.StatusFound)
}
