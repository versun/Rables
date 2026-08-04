package domain

import (
	"strings"
	"testing"
)

func TestRenderMarkdown(t *testing.T) {
	html := RenderMarkdown("# Hi\n\nsome **bold** and ~~gone~~\n")
	for _, want := range []string{"<h1>Hi</h1>", "<strong>bold</strong>", "<del>gone</del>"} {
		if !strings.Contains(html, want) {
			t.Errorf("RenderMarkdown output missing %q: %q", want, html)
		}
	}
}

// Raw HTML passes through the renderer (CommonMark); SanitizeHTML downstream
// is what strips dangerous markup, same as the rich_text write path.
func TestRenderMarkdownRawHTMLPassthrough(t *testing.T) {
	if got := RenderMarkdown("<div>raw</div>"); !strings.Contains(got, "<div>raw</div>") {
		t.Errorf("raw HTML not passed through: %q", got)
	}
}
