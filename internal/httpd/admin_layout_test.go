package httpd

import (
	"net/http"
	"strings"
	"testing"
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
		`href="/admin/twitter_archives"`,
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
