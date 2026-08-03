package domain

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// PlainText ports Article#plain_text_content for stored HTML, i.e.
// ActionView::Base.full_sanitizer: all tags are stripped and the remaining
// text nodes are concatenated with no separator.
func PlainText(rawHTML string) string {
	if rawHTML == "" {
		return ""
	}
	nodes, err := html.ParseFragment(strings.NewReader(rawHTML), bodyContext)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, n := range nodes {
		writeText(&b, n)
	}
	return b.String()
}

func writeText(b *strings.Builder, n *html.Node) {
	if n.Type == html.TextNode {
		b.WriteString(n.Data)
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		writeText(b, c)
	}
}

// Squish mirrors ActiveSupport's String#squish: strips leading/trailing
// whitespace and collapses internal whitespace runs to a single space.
func Squish(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// IsBlank mirrors Rails' blank? for strings: empty or whitespace-only.
func IsBlank(s string) bool {
	return strings.TrimSpace(s) == ""
}

// bodyContext is the fragment parsing context shared by the HTML helpers.
var bodyContext = &html.Node{Type: html.ElementNode, DataAtom: atom.Body, Data: "body"}
