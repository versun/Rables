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

func TestPlainTextDeepNesting(t *testing.T) {
	// Over 512 open elements the parser errors out; PlainText falls back to
	// "" rather than panicking.
	if got := PlainText(strings.Repeat("<div>", 600) + "x"); got != "" {
		t.Errorf("PlainText(600 nested divs) = %q, want \"\"", got)
	}
}

func TestPlainTextOversized(t *testing.T) {
	// Over maxHTMLParseBytes, PlainText takes its parse-error fallback ("")
	// rather than building a DOM from the untrusted-sized input.
	big := "<p>hi</p>" + strings.Repeat("<p>x</p>", maxHTMLParseBytes/len("<p>x</p>")+1)
	if got := PlainText(big); got != "" {
		t.Errorf("PlainText(oversized) = %.20q, want \"\"", got)
	}
}

func TestHasContent(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty", in: "", want: false},
		{name: "lexxy empty markup", in: "<p><br></p>", want: false},
		{name: "nbsp only", in: "<p>&nbsp;</p>", want: false},
		{name: "text", in: "<p>Hello</p>", want: true},
		{name: "image only", in: `<img src="/files/abc123" alt="pic.png">`, want: true},
		{name: "image only uppercased tag", in: `<IMG SRC="/files/abc123">`, want: true},
		{name: "action-text-attachment only", in: `<action-text-attachment url="/files/abc123" content-type="image/png"></action-text-attachment>`, want: true},
		{name: "video only", in: `<video src="/files/a.mp4" controls></video>`, want: true},
		{name: "br and text", in: "<p><br></p><p>tail</p>", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasContent(tt.in); got != tt.want {
				t.Errorf("HasContent(%q) = %v, want %v", tt.in, got, tt.want)
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
