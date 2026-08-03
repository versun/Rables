package domain

import (
	"strings"
	"testing"
)

const (
	testSite   = "https://example.com"
	testSource = "https://example.com/source"
	testSuffix = "\nRead more: https://example.com/s" // 33 chars
)

func TestCountChars(t *testing.T) {
	tests := []struct {
		s      string
		double bool
		want   int
	}{
		{"a中", true, 3},
		{"a中", false, 2},
		{"hello", false, 5},
		{"中文", true, 4},
		{"", true, 0},
	}
	for _, tt := range tests {
		if got := CountChars(tt.s, tt.double); got != tt.want {
			t.Errorf("CountChars(%q, %v) = %d, want %d", tt.s, tt.double, got, tt.want)
		}
	}
}

func TestTruncateText(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		max    int
		double bool
		want   string
	}{
		{name: "ascii cut", s: "abcdef", max: 3, want: "abc"},
		{name: "shorter than max", s: "abc", max: 10, want: "abc"},
		{name: "non-ascii width", s: "a中文", max: 3, double: true, want: "a中"},
		{name: "non-ascii does not split", s: "中文", max: 1, double: true, want: ""},
		{name: "non-ascii exact fit", s: "中文a", max: 5, double: true, want: "中文a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TruncateText(tt.s, tt.max, tt.double); got != tt.want {
				t.Errorf("TruncateText(%q, %d, %v) = %q, want %q", tt.s, tt.max, tt.double, got, tt.want)
			}
		})
	}
}

// Each return branch of ContentBuilder#build_content.
func TestBuildContent(t *testing.T) {
	tests := []struct {
		name     string
		in       ContentInput
		opt      BuildOptions
		want     string
		checkMax bool // result must respect max_length
	}{
		{
			name:     "no link, with title, source last",
			in:       ContentInput{Slug: "s", Title: "Hello", PlainText: "Short content", SourceURL: testSource},
			opt:      BuildOptions{MaxLength: 300, SiteURL: testSite},
			want:     "Hello\nShort content\n" + testSource,
			checkMax: true,
		},
		{
			name:     "no link, no title, source last",
			in:       ContentInput{Slug: "s", PlainText: "Short content", SourceURL: testSource},
			opt:      BuildOptions{MaxLength: 300, SiteURL: testSite},
			want:     "Short content\n" + testSource,
			checkMax: true,
		},
		{
			name:     "link, title and content fit",
			in:       ContentInput{Slug: "s", Title: "Hi", PlainText: "abc"},
			opt:      BuildOptions{MaxLength: 60, AlwaysAddLink: true, SiteURL: testSite},
			want:     "Hi\nabc" + testSuffix,
			checkMax: true,
		},
		{
			name: "link, content truncated",
			in:   ContentInput{Slug: "s", Title: "Title", PlainText: strings.Repeat("a", 200)},
			opt:  BuildOptions{MaxLength: 80, SiteURL: testSite},
			// available 47, remaining 41 -> 38 content chars + "..."
			want:     "Title\n" + strings.Repeat("a", 38) + "..." + testSuffix,
			checkMax: true,
		},
		{
			name: "link, title only when no room for content",
			in:   ContentInput{Slug: "s", Title: "Title", PlainText: "abc"},
			opt:  BuildOptions{MaxLength: 42, AlwaysAddLink: true, SiteURL: testSite},
			// available 9, remaining 3 <= 4
			want:     "Title" + testSuffix,
			checkMax: true,
		},
		{
			name: "link, long title truncated",
			in:   ContentInput{Slug: "s", Title: "VeryLongTitle", PlainText: "c"},
			opt:  BuildOptions{MaxLength: 48, AlwaysAddLink: true, SiteURL: testSite},
			// available 15, title 13 >= 15-3 -> truncate to 12 + "..."
			want:     "VeryLongTitl..." + testSuffix,
			checkMax: true,
		},
		{
			// The link alone can exceed max_length; no length check (same in Ruby).
			name: "link only when no space left",
			in:   ContentInput{Slug: "s", Title: "Title", PlainText: "content"},
			opt:  BuildOptions{MaxLength: 5, AlwaysAddLink: true, SiteURL: testSite},
			want: "Read more: https://example.com/s",
		},
		{
			name:     "no title, content fits",
			in:       ContentInput{Slug: "s", PlainText: "abc"},
			opt:      BuildOptions{MaxLength: 50, AlwaysAddLink: true, SiteURL: testSite},
			want:     "abc" + testSuffix,
			checkMax: true,
		},
		{
			name:     "no title, link only at 4 chars available",
			in:       ContentInput{Slug: "s", PlainText: "abcdef"},
			opt:      BuildOptions{MaxLength: 37, AlwaysAddLink: true, SiteURL: testSite},
			want:     "Read more: https://example.com/s",
			checkMax: true,
		},
		{
			name: "no title, content truncated",
			in:   ContentInput{Slug: "s", PlainText: strings.Repeat("b", 100)},
			opt:  BuildOptions{MaxLength: 50, AlwaysAddLink: true, SiteURL: testSite},
			// available 17 -> 14 content chars + "..."
			want:     strings.Repeat("b", 14) + "..." + testSuffix,
			checkMax: true,
		},
		{
			name:     "description wins over content and forces link",
			in:       ContentInput{Slug: "s", Title: "Test Title", PlainText: "ignored content", Description: "This is desc"},
			opt:      BuildOptions{MaxLength: 100, SiteURL: testSite},
			want:     "Test Title\nThis is desc" + testSuffix,
			checkMax: true,
		},
		{
			name:     "read more before source url",
			in:       ContentInput{Slug: "s", Title: "Hello", PlainText: "x", Description: "Short desc", SourceURL: testSource},
			opt:      BuildOptions{MaxLength: 300, SiteURL: testSite},
			want:     "Hello\nShort desc" + testSuffix + "\n" + testSource,
			checkMax: true,
		},
		{
			name: "link only, source still last",
			in:   ContentInput{Slug: "s", Title: "Hello", PlainText: strings.Repeat("a", 200), SourceURL: testSource},
			opt:  BuildOptions{MaxLength: 60, SiteURL: testSite},
			// available = 60 - 33 - 28 < 0
			want:     "Read more: https://example.com/s\n" + testSource,
			checkMax: true,
		},
		{
			name:     "twitter form: no space, link length 34",
			in:       ContentInput{Slug: "s", Title: "t", PlainText: "c"},
			opt:      BuildOptions{MaxLength: 100, AlwaysAddLink: true, CountNonASCIIDouble: true, SiteURL: testSite},
			want:     "t\nc\nRead more:https://example.com/s",
			checkMax: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildContent(tt.in, tt.opt)
			if got != tt.want {
				t.Errorf("BuildContent() = %q, want %q", got, tt.want)
			}
			if tt.checkMax {
				if n := CountChars(got, tt.opt.CountNonASCIIDouble); n > tt.opt.MaxLength {
					t.Errorf("result length %d exceeds max %d: %q", n, tt.opt.MaxLength, got)
				}
			}
		})
	}
}

func TestBuildPostURL(t *testing.T) {
	tests := []struct {
		name    string
		siteURL string
		prefix  string
		slug    string
		want    string
	}{
		{name: "plain", siteURL: "https://example.com", slug: "my-post", want: "https://example.com/my-post"},
		{name: "non-default port kept", siteURL: "http://localhost:3000", slug: "my-post", want: "http://localhost:3000/my-post"},
		{name: "default https port omitted", siteURL: "https://example.com:443", slug: "my-post", want: "https://example.com/my-post"},
		{name: "default http port omitted", siteURL: "http://example.com:80", slug: "my-post", want: "http://example.com/my-post"},
		{name: "non-default https port kept", siteURL: "https://example.com:8443", slug: "my-post", want: "https://example.com:8443/my-post"},
		{name: "scheme added", siteURL: "example.com", slug: "my-post", want: "https://example.com/my-post"},
		{name: "trailing slash chomped", siteURL: "https://example.com/", slug: "my-post", want: "https://example.com/my-post"},
		{name: "blank falls back to localhost", siteURL: "", slug: "my-post", want: "http://localhost:3000/my-post"},
		{name: "route prefix", siteURL: "https://example.com", prefix: "blog", slug: "my-post", want: "https://example.com/blog/my-post"},
		{name: "route prefix with slashes", siteURL: "https://example.com", prefix: "/blog/", slug: "my-post", want: "https://example.com/blog/my-post"},
		{name: "chinese slug escaped", siteURL: "https://example.com", slug: "你好 世界", want: "https://example.com/%E4%BD%A0%E5%A5%BD%20%E4%B8%96%E7%95%8C"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildPostURL(tt.siteURL, tt.prefix, tt.slug); got != tt.want {
				t.Errorf("BuildPostURL(%q, %q, %q) = %q, want %q", tt.siteURL, tt.prefix, tt.slug, got, tt.want)
			}
		})
	}
}

func TestEffectiveMaxCharacters(t *testing.T) {
	tests := []struct {
		platform   string
		configured int
		want       int
	}{
		{"mastodon", 0, 500},
		{"twitter", 0, 250},
		{"bluesky", 0, 300},
		{"xiaohongshu", 0, 300},
		{"twitter", 100, 100},
	}
	for _, tt := range tests {
		if got := EffectiveMaxCharacters(tt.platform, tt.configured); got != tt.want {
			t.Errorf("EffectiveMaxCharacters(%q, %d) = %d, want %d", tt.platform, tt.configured, got, tt.want)
		}
	}

	if !PlatformCountNonASCIIDouble("twitter") {
		t.Error("twitter should count non-ASCII double")
	}
	for _, p := range []string{"mastodon", "bluesky", "xiaohongshu"} {
		if PlatformCountNonASCIIDouble(p) {
			t.Errorf("%s should not count non-ASCII double", p)
		}
	}
}
