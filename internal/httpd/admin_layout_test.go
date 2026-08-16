package httpd

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
)

// TestAdminLayoutSidebar: every admin page renders inside the admin shell
// (sidebar nav + main column), mirroring the Rails admin layout and its
// _sidebar partial.
func TestAdminLayoutSidebar(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)

	rec := doRequest(t, h, http.MethodGet, "/admin/posts", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	wants := []string{
		`<body class="admin-body">`,
		`<aside id="admin-sidebar" class="admin-sidebar"`,
		`<main class="admin-main">`,
		`class="sidebar-brand">Rables</a>`,
		`href="/admin/posts"`,
		`href="/admin/pages"`,
		`href="/admin/tags"`,
		`href="/admin/comments"`,
		`href="/admin/static_files"`,
		`href="/admin/redirects"`,
		`href="/admin/setting/edit"`,
		`href="/admin/migrates"`,
		`href="/admin/crossposts"`,
		`href="/admin/newsletter"`,
		`href="/admin/jobs"`,
		`href="/admin/twitter_sync"`,
		`href="/users/current/edit"`,
		`href="/admin/activities"`,
		`action="/session/destroy"`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("admin page missing %q", want)
		}
	}
}

// TestAdminSidebarPendingBadges: the sidebar marks Comments with a red dot
// while comments await moderation, and Newsletter while subscribers await
// confirmation.
func TestAdminSidebarPendingBadges(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	insertArticle(t, s, "post", 1, 1)

	get := func() string {
		rec := doRequest(t, h, http.MethodGet, "/admin/posts", nil, session)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		return rec.Body.String()
	}

	if body := get(); strings.Contains(body, `class="nav-dot"`) {
		t.Fatalf("no pending items, but a badge dot rendered:\n%s", body)
	}

	// A pending comment dots Comments only.
	localComment(t, s, 1, "ann", domain.CommentPending)
	body := get()
	if !strings.Contains(body, `title="Pending comments"`) {
		t.Errorf("pending comment: Comments badge missing")
	}
	if strings.Contains(body, `title="Unconfirmed subscribers"`) {
		t.Errorf("pending comment: Newsletter badge should not show")
	}

	// An unconfirmed subscriber dots the Newsletter link (Comments stays
	// dotted while its comment is still pending).
	now := time.Now().UTC().Unix()
	if _, err := s.Q.CreateSubscriber(t.Context(), query.CreateSubscriberParams{
		Email:     "sub@example.com",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create subscriber: %v", err)
	}
	body = get()
	if !strings.Contains(body, `title="Unconfirmed subscribers"`) {
		t.Errorf("unconfirmed subscriber: Newsletter badge missing")
	}
	if !strings.Contains(body, `title="Pending comments"`) {
		t.Errorf("unconfirmed subscriber: Comments badge should still show")
	}
}

// TestAdminSidebarBadgesClear: once nothing is pending, neither dot renders.
func TestAdminSidebarBadgesClear(t *testing.T) {
	s, h := newArticlesTestServer(t)
	session := articlesSessionCookie(t, s)
	insertArticle(t, s, "post", 1, 1)

	// An approved comment and a confirmed subscriber must not light the dots.
	localComment(t, s, 1, "ann", domain.CommentApproved)
	now := time.Now().UTC().Unix()
	sub, err := s.Q.CreateSubscriber(t.Context(), query.CreateSubscriberParams{
		Email:     "sub@example.com",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create subscriber: %v", err)
	}
	if err := s.Q.ConfirmSubscriber(t.Context(), query.ConfirmSubscriberParams{
		ConfirmedAt: sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:   now,
		ID:          sub.ID,
	}); err != nil {
		t.Fatalf("confirm subscriber: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/admin/posts", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, `class="nav-dot"`) {
		t.Errorf("nothing pending, but a badge dot rendered:\n%s", body)
	}
}
