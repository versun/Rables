package domain

import (
	"testing"
	"time"
)

func TestGenerateSlug(t *testing.T) {
	now := time.Date(2026, 8, 3, 2, 3, 4, 0, time.UTC)
	taken := map[string]bool{"你好世界": true, "你好世界-1": true}
	exists := func(s string) bool { return taken[s] }

	tests := []struct {
		name   string
		slug   string
		title  string
		exists func(string) bool
		want   string
	}{
		{name: "from title", title: "My Test Article", want: "my-test-article"},
		{name: "dots removed from title", title: "Article v1.0", want: "article-v1-0"},
		{name: "existing slug kept", slug: "custom-slug", title: "Title", want: "custom-slug"},
		{name: "dots removed from manual slug", slug: "my.slug", want: "myslug"},
		{name: "chinese title kept as-is", title: "你好世界", want: "你好世界"},
		{name: "chinese title unique suffix", title: "你好世界", exists: exists, want: "你好世界-2"},
		{name: "chinese with latin uses parameterized", title: "你好 World", want: "world"},
		{name: "chinese fallback keeps single spaces", title: "你好 世界", want: "你好 世界"},
		{name: "accents transliterated", title: "Café", want: "cafe"},
		{name: "separators collapsed", title: "Hello--World", want: "hello-world"},
		{name: "underscore kept", title: "a_b-c d", want: "a_b-c-d"},
		{name: "blank title falls back to timestamp", title: "  ", want: "2026-08-03-02-03"},
		{name: "no title falls back to timestamp", want: "2026-08-03-02-03"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GenerateSlug(tt.slug, tt.title, now, tt.exists); got != tt.want {
				t.Errorf("GenerateSlug(%q, %q) = %q, want %q", tt.slug, tt.title, got, tt.want)
			}
		})
	}
}

func TestIsReservedSlug(t *testing.T) {
	reserved := []string{
		"admin", "tags", "pages", "users", "session", "setup", "confirm",
		"unsubscribe", "static", "up", "rails", "twitter", "subscriptions",
		"feed.xml", "sitemap.xml",
	}
	for _, r := range reserved {
		if !IsReservedSlug(r) {
			t.Errorf("IsReservedSlug(%q) = false, want true", r)
		}
	}
	for _, ok := range []string{"my-post", "hello", "admin-x", ""} {
		if IsReservedSlug(ok) {
			t.Errorf("IsReservedSlug(%q) = true, want false", ok)
		}
	}
}
