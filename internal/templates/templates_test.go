package templates

import (
	"strings"
	"testing"
)

type dummyData struct {
	Flash   Flash
	Message string
	Now     int64
}

func renderDummy(t *testing.T, data dummyData) string {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var b strings.Builder
	if err := r.Render(&b, "dummy", data); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return b.String()
}

func TestRenderDummyComposesLayout(t *testing.T) {
	out := renderDummy(t, dummyData{
		Flash:   Flash{Notice: "Article saved", Alert: "Something broke"},
		Message: "hello <world>",
		Now:     1700000000,
	})

	wants := []string{
		"<!DOCTYPE html>",      // layout skeleton
		"<title>Dummy</title>", // page title block
		`<div class="flash flash-notice">Article saved</div>`, // flash area
		`<div class="flash flash-alert">Something broke</div>`,
		"<h1>Dummy page</h1>",          // page content block
		"<p>hello &lt;world&gt;</p>",   // html/template escaping
		"Rendered at 2023-11-14 22:13", // funcmap call in page
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestRenderDummyWithoutFlash(t *testing.T) {
	out := renderDummy(t, dummyData{Message: "hi", Now: 1700000000})
	if strings.Contains(out, `class="flash`) {
		t.Errorf("expected no flash markup, got:\n%s", out)
	}
}

func TestRenderNonAdminPageHasNoAdminChrome(t *testing.T) {
	out := renderDummy(t, dummyData{Message: "hi", Now: 1700000000})
	if strings.Contains(out, "admin-sidebar") || strings.Contains(out, "admin-body") {
		t.Errorf("bare layout leaked admin chrome:\n%s", out)
	}
}

// The account page (/users/current/edit) renders inside the admin shell even
// though its name does not start with "admin_" (Rails UsersController).
func TestRenderAccountPageUsesAdminLayout(t *testing.T) {
	data := struct {
		Flash Flash
		User  struct {
			ID       int64
			UserName string
		}
	}{}
	data.User.ID = 1
	data.User.UserName = "alice"

	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var b strings.Builder
	if err := r.Render(&b, "auth_password_edit", data); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := b.String()

	wants := []string{
		"admin-sidebar",          // admin shell sidebar
		`<link rel="stylesheet" href="/assets/admin.css">`,
		"<title>Account Settings</title>",
		`action="/users/1"`,
		`value="alice"`,
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestRenderUnknownPage(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Render(&strings.Builder{}, "nope", nil); err == nil {
		t.Fatal("expected error for unknown page")
	}
}
