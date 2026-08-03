package httpd

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	subscribersvc "rables/internal/service/subscribers"
)

// TestAdminSubscribersAuth: every admin subscriber route sits behind
// RequireAuth.
func TestAdminSubscribersAuth(t *testing.T) {
	_, h := newSubscriptionTestServer(t)
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/subscribers"},
		{http.MethodPost, "/admin/subscribers/batch_create"},
		{http.MethodPost, "/admin/subscribers/batch_confirm"},
		{http.MethodPost, "/admin/subscribers/batch_destroy"},
		{http.MethodPost, "/admin/subscribers/1/destroy"},
	}
	for _, tt := range tests {
		rec := doRequest(t, h, tt.method, tt.path, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
			t.Errorf("%s %s: status = %d location = %q, want 302 /session/new",
				tt.method, tt.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestAdminSubscribersIndexFilters(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	session := tagsSessionCookie(t, s)
	tag := insertTagRow(t, s, "Go")

	active := insertSubscriber(t, s, "active@example.com", true, false)
	insertSubscriber(t, s, "pending@example.com", false, false)
	insertSubscriber(t, s, "gone@example.com", true, true)
	if err := subscribersvc.ReplaceTags(t.Context(), s.Q, active.ID, []int64{tag.ID}); err != nil {
		t.Fatalf("assign tag: %v", err)
	}

	tests := []struct {
		name    string
		query   string
		want    []string
		notWant []string
	}{
		{"all", "", []string{"active@example.com", "pending@example.com", "gone@example.com", "所有内容"}, nil},
		{"active", "?status=active", []string{"active@example.com"}, []string{"pending@example.com", "gone@example.com"}},
		{"unconfirmed", "?status=unconfirmed", []string{"pending@example.com"}, []string{"active@example.com"}},
		{"unsubscribed", "?status=unsubscribed", []string{"gone@example.com"}, []string{"active@example.com", "pending@example.com"}},
		{"tag filter", "?tag_ids[]=" + strconv.FormatInt(tag.ID, 10), []string{"active@example.com"}, []string{"pending@example.com", "gone@example.com"}},
		{"include_all only", "?include_all=1", []string{"pending@example.com", "gone@example.com"}, []string{"active@example.com"}},
		{"tag plus include_all", "?tag_ids[]=" + strconv.FormatInt(tag.ID, 10) + "&include_all=1",
			[]string{"active@example.com", "pending@example.com", "gone@example.com"}, nil},
		{"unknown tag id", "?tag_ids[]=99999", nil, []string{"active@example.com", "pending@example.com", "gone@example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, h, http.MethodGet, "/admin/subscribers"+tt.query, nil, session)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, want := range tt.want {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q", want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(body, notWant) {
					t.Errorf("body should not contain %q", notWant)
				}
			}
		})
	}
}

func TestAdminSubscribersIndexPagination(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	session := tagsSessionCookie(t, s)
	for i := 1; i <= 31; i++ {
		insertSubscriber(t, s, "user"+strconv.Itoa(i)+"@example.com", true, false)
	}

	rec := doRequest(t, h, http.MethodGet, "/admin/subscribers", nil, session)
	if got := strings.Count(rec.Body.String(), `name="ids[]"`); got != 30 {
		t.Errorf("page 1 rows = %d, want 30", got)
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/subscribers?page=2", nil, session)
	if got := strings.Count(rec.Body.String(), `name="ids[]"`); got != 1 {
		t.Errorf("page 2 rows = %d, want 1", got)
	}
	// will_paginate raises InvalidPage on garbage page params (404 here).
	rec = doRequest(t, h, http.MethodGet, "/admin/subscribers?page=abc", nil, session)
	if rec.Code != http.StatusNotFound {
		t.Errorf("page=abc: status = %d, want 404", rec.Code)
	}
}

func TestAdminSubscribersDestroy(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	session := tagsSessionCookie(t, s)
	tag := insertTagRow(t, s, "Go")
	sub := insertSubscriber(t, s, "doomed@example.com", true, false)
	if err := subscribersvc.ReplaceTags(t.Context(), s.Q, sub.ID, []int64{tag.ID}); err != nil {
		t.Fatalf("assign tag: %v", err)
	}

	rec := doRequest(t, h, http.MethodPost, "/admin/subscribers/"+strconv.FormatInt(sub.ID, 10)+"/destroy", nil, session)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/subscribers" {
		t.Fatalf("status = %d, want 303 /admin/subscribers", rec.Code)
	}
	if flash := readFlash(t, rec); flash.Notice != "订阅者已删除。" {
		t.Errorf("notice = %q", flash.Notice)
	}
	if got := subscriberCount(t, s); got != 0 {
		t.Errorf("subscribers = %d, want 0", got)
	}
	var joins int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM subscriber_tags").Scan(&joins); err != nil {
		t.Fatalf("count joins: %v", err)
	}
	if joins != 0 {
		t.Errorf("subscriber_tags = %d, want 0 (dependent destroy)", joins)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/subscribers/99999/destroy", nil, session)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing subscriber: status = %d, want 404", rec.Code)
	}
}

func TestAdminSubscribersBatchCreate(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	session := tagsSessionCookie(t, s)
	existing := insertSubscriber(t, s, "existing@example.com", true, false)

	text := "new1@example.com,Go, Rails\n" + // new with tags (tags are created)
		"new2@example.com\n" + // new without tags = all content
		"\n" + // blank lines skipped
		"bad-email\n" + // invalid
		",,,\n" + // only commas: invalid (blank) email, like the Ruby split
		"existing@example.com,Go\n" // existing with tags: tags replaced
	rec := doRequest(t, h, http.MethodPost, "/admin/subscribers/batch_create",
		url.Values{"emails_text": {text}}, session)
	flash := readFlash(t, rec)
	if flash.Notice != "成功添加 2 个订阅者。 1 个已存在并更新 tags。 2 个失败。" {
		t.Errorf("notice = %q", flash.Notice)
	}

	// New subscribers are auto-confirmed; no confirmation email is sent.
	new1, err := s.Q.GetSubscriberByEmail(t.Context(), "new1@example.com")
	if err != nil {
		t.Fatalf("new1 not stored: %v", err)
	}
	if !new1.ConfirmedAt.Valid || new1.UnsubscribedAt.Valid {
		t.Error("batch-created subscriber should be auto-confirmed")
	}
	if !new1.ConfirmationToken.Valid || !new1.UnsubscribeToken.Valid {
		t.Error("batch-created subscriber should carry both tokens")
	}
	goTag, err := s.Q.GetTagByLowerName(t.Context(), "Go")
	if err != nil {
		t.Fatalf("tag Go not auto-created: %v", err)
	}
	railsTag, err := s.Q.GetTagByLowerName(t.Context(), "Rails")
	if err != nil {
		t.Fatalf("tag Rails not auto-created: %v", err)
	}
	if got := subscriberTagIDs(t, s, new1.ID); len(got) != 2 || got[0] != goTag.ID || got[1] != railsTag.ID {
		t.Errorf("new1 tags = %v, want [%d %d]", got, goTag.ID, railsTag.ID)
	}
	new2, _ := s.Q.GetSubscriberByEmail(t.Context(), "new2@example.com")
	if got := subscriberTagIDs(t, s, new2.ID); len(got) != 0 {
		t.Errorf("new2 tags = %v, want empty (all content)", got)
	}
	if got := subscriberTagIDs(t, s, existing.ID); len(got) != 1 || got[0] != goTag.ID {
		t.Errorf("existing tags = %v, want replaced with [%d]", got, goTag.ID)
	}
	if runs := jobRunRows(t, s); len(runs) != 0 {
		t.Errorf("batch_create should not send confirmation emails: %v", runs)
	}

	// An existing address without tags on the line is skipped (a trailing
	// comma splits away, like Ruby's String#split).
	rec = doRequest(t, h, http.MethodPost, "/admin/subscribers/batch_create",
		url.Values{"emails_text": {"existing@example.com,"}}, session)
	if flash := readFlash(t, rec); flash.Notice != "成功添加 0 个订阅者。 1 个已存在跳过。" {
		t.Errorf("notice = %q", flash.Notice)
	}
	if got := subscriberTagIDs(t, s, existing.ID); len(got) != 1 || got[0] != goTag.ID {
		t.Errorf("existing tags changed by a tagless line: %v", got)
	}
}

func TestAdminSubscribersBatchCreateFailures(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	session := tagsSessionCookie(t, s)

	rec := doRequest(t, h, http.MethodPost, "/admin/subscribers/batch_create",
		url.Values{"emails_text": {"  "}}, session)
	if flash := readFlash(t, rec); flash.Alert != "请输入邮箱地址。" {
		t.Errorf("blank text alert = %q", flash.Alert)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/subscribers/batch_create",
		url.Values{"emails_text": {"bad1\nbad2"}}, session)
	if flash := readFlash(t, rec); flash.Alert != "添加失败: 无效的邮箱格式: bad1; 无效的邮箱格式: bad2" {
		t.Errorf("alert = %q", flash.Alert)
	}
	if got := subscriberCount(t, s); got != 0 {
		t.Errorf("subscribers = %d, want 0", got)
	}
}

func TestAdminSubscribersBatchConfirm(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	session := tagsSessionCookie(t, s)
	pending := insertSubscriber(t, s, "pending@example.com", false, false)
	active := insertSubscriber(t, s, "active@example.com", true, false)
	gone := insertSubscriber(t, s, "gone@example.com", true, true)

	ids := url.Values{"ids[]": {
		strconv.FormatInt(pending.ID, 10),
		strconv.FormatInt(active.ID, 10),
		strconv.FormatInt(gone.ID, 10),
		"99999", // missing records are skipped
	}}
	rec := doRequest(t, h, http.MethodPost, "/admin/subscribers/batch_confirm", ids, session)
	if flash := readFlash(t, rec); flash.Notice != "已确认 1 个订阅者。" {
		t.Errorf("notice = %q", flash.Notice)
	}
	got, _ := s.Q.GetSubscriberByEmail(t.Context(), "pending@example.com")
	if !got.ConfirmedAt.Valid {
		t.Error("pending subscriber not confirmed")
	}
	got, _ = s.Q.GetSubscriberByEmail(t.Context(), "gone@example.com")
	if !got.UnsubscribedAt.Valid {
		t.Error("unsubscribed subscriber was reactivated")
	}
}

func TestAdminSubscribersBatchDestroy(t *testing.T) {
	s, h := newSubscriptionTestServer(t)
	session := tagsSessionCookie(t, s)
	a := insertSubscriber(t, s, "a@example.com", true, false)
	b := insertSubscriber(t, s, "b@example.com", false, false)

	ids := url.Values{"ids[]": {
		strconv.FormatInt(a.ID, 10), strconv.FormatInt(b.ID, 10), "99999",
	}}
	rec := doRequest(t, h, http.MethodPost, "/admin/subscribers/batch_destroy", ids, session)
	if flash := readFlash(t, rec); flash.Notice != "已删除 2 个订阅者。" {
		t.Errorf("notice = %q", flash.Notice)
	}
	if got := subscriberCount(t, s); got != 0 {
		t.Errorf("subscribers = %d, want 0", got)
	}
}
