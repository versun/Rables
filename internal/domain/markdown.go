package domain

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

// markdownRenderer converts Markdown source to HTML. GFM covers tables,
// strikethrough, task lists and autolinks; WithUnsafe passes raw HTML through
// like CommonMark — the output still goes through SanitizeHTML before it is
// stored, matching the rich_text write path.
var markdownRenderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

// RenderMarkdown renders Markdown source to unsanitized HTML. The result must
// go through SanitizeHTML before it is stored or served.
func RenderMarkdown(src string) string {
	var buf bytes.Buffer
	if err := markdownRenderer.Convert([]byte(src), &buf); err != nil {
		return ""
	}
	return buf.String()
}
