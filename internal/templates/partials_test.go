package templates

import (
	"database/sql"
	"strings"
	"testing"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/service/comments"
)

// TestPartialsAreNotPages: "_" prefixed files must not become renderable
// pages, while their defines are available inside every page set.
func TestPartialsAreNotPages(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"_comments_tree", "_comment_form"} {
		if err := r.Render(&strings.Builder{}, name, nil); err == nil {
			t.Errorf("partial %q rendered as a page", name)
		}
	}
}

func treeFixture(id int64, platform string, status domain.CommentStatus, parentID, publishedAt int64) query.Comment {
	return query.Comment{
		ID:              id,
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
		ParentID:        sql.NullInt64{Int64: parentID, Valid: parentID != 0},
		AuthorName:      "Ann",
		Content:         "hello <b>world</b>",
		Status:          int64(status),
		Platform:        sql.NullString{String: platform, Valid: platform != ""},
		PublishedAt:     sql.NullInt64{Int64: publishedAt, Valid: true},
		CreatedAt:       1000,
	}
}

// TestCommentsTreePartial renders the tree partial inside a page set,
// covering nesting, sanitizing and the reply-form wiring.
func TestCommentsTreePartial(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	page := r.pages["dummy"] // any page set carries the partial defines

	nodes := comments.BuildTree([]query.Comment{
		treeFixture(1, "", domain.CommentApproved, 0, 100),
		treeFixture(2, "", domain.CommentApproved, 1, 200),
		treeFixture(3, "mastodon", domain.CommentApproved, 0, 300),
	})
	comments.PrepareDisplay(nodes, "UTC", comments.FormData{
		Action: "/comments?article_id=x", Question: "1 + 2 =", Token: "tok", A: 1, B: 2, Op: "+",
	})

	var b strings.Builder
	if err := page.tpl.ExecuteTemplate(&b, "comments_tree", nodes); err != nil {
		t.Fatalf("execute comments_tree: %v", err)
	}
	out := b.String()
	wants := []string{
		`data-comment-id="1"`,
		`data-comment-id="2"`,
		`data-comment-id="3"`,
		"hello world",       // disallowed <b> stripped, text kept (sanitize parity)
		`id="reply-form-1"`, // reply form wired for local root
		`name="captcha[token]" value="tok"`,
		`name="comment[parent_id]" value="1"`,
		"#6364FF", // mastodon accent
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("tree output missing %q\ngot:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<b>") {
		t.Error("disallowed tag survived sanitizing")
	}
	// The platform root must not carry a reply form.
	if strings.Contains(out, `id="reply-form-3"`) {
		t.Error("platform comment got a reply form")
	}
	// Reply (depth 1) renders before the platform root (grouping order).
	if strings.Index(out, `data-comment-id="2"`) > strings.Index(out, `data-comment-id="3"`) {
		t.Error("display order broken: local subtree must precede platform groups")
	}
}

// TestCommentsTreeAuthorLink: the author name is linked only when a non-empty
// author_url was left. Rows may hold "" instead of NULL (migrated data), and
// an empty href would resolve to the article itself.
func TestCommentsTreeAuthorLink(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	noURL := treeFixture(1, "", domain.CommentApproved, 0, 100)
	noURL.AuthorName = "NoLink"
	noURL.AuthorUrl = sql.NullString{String: "", Valid: true}
	withURL := treeFixture(2, "", domain.CommentApproved, 0, 200)
	withURL.AuthorName = "WithLink"
	withURL.AuthorUrl = sql.NullString{String: "https://blog.example.com", Valid: true}

	nodes := comments.BuildTree([]query.Comment{noURL, withURL})
	comments.PrepareDisplay(nodes, "UTC", comments.FormData{
		Action: "/comments?article_id=x", Question: "1 + 2 =", Token: "tok", A: 1, B: 2, Op: "+",
	})

	var b strings.Builder
	if err := r.pages["dummy"].tpl.ExecuteTemplate(&b, "comments_tree", nodes); err != nil {
		t.Fatalf("execute comments_tree: %v", err)
	}
	out := b.String()
	if strings.Contains(out, `<a href=""`) || strings.Contains(out, `>NoLink</a>`) {
		t.Error("empty author_url rendered as a link (resolves to the article page)")
	}
	if !strings.Contains(out, `<a href="https://blog.example.com"`) {
		t.Error("author_url not linked")
	}
}

// TestCommentFormPartial renders the top-level form (no parent_id field).
func TestCommentFormPartial(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var b strings.Builder
	err = r.pages["dummy"].tpl.ExecuteTemplate(&b, "comment_form", comments.FormData{
		Action: "/comments?page_id=about", Question: "4 - 1 =", Token: "tok", A: 4, B: 1, Op: "-",
	})
	if err != nil {
		t.Fatalf("execute comment_form: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		`action="/comments?page_id=about"`, "4 - 1 =", `name="captcha[answer]"`,
		`name="captcha[a]" value="4"`, `name="captcha[op]" value="-"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("form output missing %q\ngot:\n%s", want, out)
		}
	}
	if strings.Contains(out, "comment[parent_id]") {
		t.Error("top-level form must not carry a parent_id")
	}
}

// TestAdminSidebarBadges renders the sidebar partial with the badge
// predicates unset, set, and cleared: the Comments dot follows
// AdminBadges.Comments, the Newsletter dot AdminBadges.Newsletter, and a nil
// predicate never shows.
func TestAdminSidebarBadges(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	render := func() string {
		var b strings.Builder
		if err := r.pages["dummy"].tpl.ExecuteTemplate(&b, "admin_sidebar", nil); err != nil {
			t.Fatalf("execute admin_sidebar: %v", err)
		}
		return b.String()
	}

	if out := render(); strings.Contains(out, `class="nav-dot"`) {
		t.Errorf("no badges installed, but a dot rendered:\n%s", out)
	}

	yes := func() bool { return true }
	r.SetAdminBadges(AdminBadges{Comments: yes})
	out := render()
	if !strings.Contains(out, `title="Pending comments"`) {
		t.Error("Comments badge: dot missing")
	}
	if strings.Contains(out, `title="Unconfirmed subscribers"`) {
		t.Error("Newsletter badge: dot rendered without a predicate")
	}

	r.SetAdminBadges(AdminBadges{Comments: yes, Newsletter: yes})
	out = render()
	if !strings.Contains(out, `title="Pending comments"`) || !strings.Contains(out, `title="Unconfirmed subscribers"`) {
		t.Error("both badges set: want both dots")
	}

	r.SetAdminBadges(AdminBadges{})
	if out := render(); strings.Contains(out, `class="nav-dot"`) {
		t.Errorf("badges cleared, but a dot rendered:\n%s", out)
	}
}
