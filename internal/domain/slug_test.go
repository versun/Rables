package domain

import (
	"sync"
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
		{name: "url-unsafe chars removed from manual slug", slug: "a/b?c#d%e\\f", want: "abcdef"},
		{name: "control chars removed from manual slug", slug: "a\tb\x00c", want: "abc"},
		{name: "url-unsafe chars removed from fallback slug", title: "你好?世界", want: "你好世界"},
		{name: "cleaned fallback conflict gets unique suffix", title: "你好?世界", exists: exists, want: "你好世界-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GenerateSlug(tt.slug, tt.title, now, tt.exists); got != tt.want {
				t.Errorf("GenerateSlug(%q, %q) = %q, want %q", tt.slug, tt.title, got, tt.want)
			}
		})
	}
}

func TestIsValidSlug(t *testing.T) {
	for _, ok := range []string{"my-post", "你好世界", "a_b", "hello world", ""} {
		if !IsValidSlug(ok) {
			t.Errorf("IsValidSlug(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"a.b", "a/b", "a?b", "a#b", "a%b", `a\b`, "a\tb", "a\x00b"} {
		if IsValidSlug(bad) {
			t.Errorf("IsValidSlug(%q) = true, want false", bad)
		}
	}
}

// TestParameterizeConcurrent hammers Parameterize from many goroutines; the
// transliterator chain buffers per-call state on the struct, so a shared
// chain would race and corrupt output (run with -race).
func TestParameterizeConcurrent(t *testing.T) {
	inputs := map[string]string{
		"Café":         "cafe",
		"naïve façade": "naive-facade",
		"你好 World":    "world",
		"Hello--World": "hello-world",
	}
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for in, want := range inputs {
				if got := Parameterize(in); got != want {
					t.Errorf("Parameterize(%q) = %q, want %q", in, got, want)
				}
			}
		}()
	}
	wg.Wait()
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
