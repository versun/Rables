package domain

import (
	"strings"
	"testing"
)

func TestPlainText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "tags stripped", in: "<p>Hello world</p>", want: "Hello world"},
		{name: "inline tags keep surrounding text", in: "<p>Hello <b>bold</b> tail</p>", want: "Hello bold tail"},
		// full_sanitizer concatenates text nodes without a separator.
		{name: "block boundaries not separated", in: "<p>Hello</p><p>World</p>", want: "HelloWorld"},
		{name: "br contributes nothing", in: "<div>a<br>b</div>", want: "ab"},
		{name: "whitespace preserved", in: "<p>a  b</p>", want: "a  b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlainText(tt.in); got != tt.want {
				t.Errorf("PlainText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSquish(t *testing.T) {
	if got := Squish("  a  b\n\tc  "); got != "a b c" {
		t.Errorf("Squish = %q, want %q", got, "a b c")
	}
}

func TestBuildExcerpt(t *testing.T) {
	longWords := strings.TrimSpace(strings.Repeat("word ", 200))
	longRunes := strings.Repeat("汉", 250)

	tests := []struct {
		name        string
		description string
		contentHTML string
		want        string
	}{
		{name: "description wins", description: "Short description", contentHTML: "<p>Body</p>", want: "Short description"},
		{name: "from content", description: "", contentHTML: "<p>Hello world</p>", want: "Hello world"},
		{name: "blank description falls back to content", description: "  ", contentHTML: "<p>Hello world</p>", want: "Hello world"},
		{name: "both blank", description: "", contentHTML: "", want: ""},
		{name: "squished", description: "  a   b ", contentHTML: "", want: "a b"},
		{name: "short text unchanged", description: strings.Repeat("x", 200), contentHTML: "", want: strings.Repeat("x", 200)},
		{name: "long words truncated at boundary", description: longWords, contentHTML: "", want: longWords[:194] + "..."},
		{name: "long run without spaces hard-cut", description: longRunes, contentHTML: "", want: string([]rune(longRunes)[:197]) + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildExcerpt(tt.description, tt.contentHTML)
			if got != tt.want {
				t.Errorf("BuildExcerpt(%q, %q) = %q, want %q", tt.description, tt.contentHTML, got, tt.want)
			}
			if n := len([]rune(got)); n > ExcerptLength {
				t.Errorf("excerpt length %d exceeds %d", n, ExcerptLength)
			}
		})
	}
}
