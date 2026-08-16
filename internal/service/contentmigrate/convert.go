// Package contentmigrate converts stored rich_text (Lexxy/Action Text HTML)
// bodies to Markdown source, so articles and pages can move from the retired
// rich-text editor to the markdown editor without changing what readers see.
//
// The converter turns everything Markdown can express into Markdown syntax
// and keeps the rest as raw HTML — goldmark's unsafe renderer passes raw HTML
// through and the sanitizer whitelist allows it, so the hybrid source
// re-renders to the same content_html through the standard write path
// (RenderMarkdown + SanitizeHTML).
package contentmigrate

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"

	"rables/internal/domain"
)

// Convert turns a stored rich_text HTML body into Markdown source. The
// returned string is meant for content_markdown; re-rendering it through the
// standard write path produces the replacement content_html. A body that
// converts to nothing yields "" (not a lone newline), so callers can store
// NULL instead of a blank document.
func Convert(body string) (string, error) {
	md, err := newConverter().ConvertString(body)
	if err != nil {
		return "", fmt.Errorf("convert: %w", err)
	}
	md = strings.TrimSpace(md)
	if md == "" {
		return "", nil
	}
	return md + "\n", nil
}

// ToMarkdown converts a stored rich_text HTML body into the markdown storage
// pair every markdown write path produces: the Markdown source for
// content_markdown, and content_html rendered from it through the standard
// write path (RenderMarkdown + SanitizeHTML + AddLazyLoading).
func ToMarkdown(body string) (markdown, contentHTML string, err error) {
	md, err := Convert(body)
	if err != nil {
		return "", "", err
	}
	return md, domain.AddLazyLoading(domain.SanitizeHTML(domain.RenderMarkdown(md))), nil
}

// newConverter builds the html-to-markdown converter with the GFM feature set
// (matching the app's goldmark GFM renderer) plus the fidelity rules the
// production corpus requires.
func newConverter() *converter.Converter {
	conv := converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
			strikethrough.NewStrikethroughPlugin(),
			table.NewTablePlugin(),
		),
	)
	// The commonmark plugin's pre-render MergeAdjacent fuses *adjacent* bold
	// and italic elements into one node (e.g. <strong>a：</strong><em>b</em>
	// becomes a single strong), changing the rendered style. Rename such
	// cross-type-adjacent emphasis elements before its pre-render runs and
	// emit them as raw inline HTML.
	conv.Register.PreRenderer(protectCrossAdjacentEmphasis, converter.PriorityEarly)
	conv.Register.RendererFor("kb", converter.TagTypeInline, renderRenamedEmphasis("strong"), converter.PriorityEarly)
	conv.Register.RendererFor("ki", converter.TagTypeInline, renderRenamedEmphasis("em"), converter.PriorityEarly)
	// Elements Markdown cannot express stay as raw HTML: goldmark's unsafe
	// renderer passes them through and the sanitizer whitelist allows them.
	for _, tag := range []string{"video", "audio", "source", "iframe", "figure", "figcaption", "details", "summary", "dl", "address"} {
		conv.Register.RendererFor(tag, converter.TagTypeBlock, base.RenderAsHTML, converter.PriorityEarly)
	}
	// Inline elements Markdown cannot express either: underline/insert and
	// the semantic inline tags would otherwise be silently unwrapped to
	// plain text (H₂O → H2O, ruby annotation inlined into the text).
	for _, tag := range []string{"mark", "u", "ins", "sub", "sup", "ruby", "rt", "rp", "abbr", "q", "cite", "small", "time"} {
		conv.Register.RendererFor(tag, converter.TagTypeInline, base.RenderAsHTML, converter.PriorityEarly)
	}
	// Spans and divs are structural noise unless they carry styling, which
	// Markdown cannot express; keep those raw (the sanitizer allows
	// class/style), unwrap the rest. The retired editor's own wrapper
	// (div.trix-content) is unwrapped either way.
	keepStyled := func(ctx converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
		for _, a := range n.Attr {
			if a.Key == "style" || (a.Key == "class" && strings.TrimSpace(a.Val) != "trix-content") {
				return base.RenderAsHTML(ctx, w, n)
			}
		}
		return converter.RenderTryNext
	}
	conv.Register.RendererFor("span", converter.TagTypeInline, keepStyled, converter.PriorityEarly)
	conv.Register.RendererFor("div", converter.TagTypeBlock, keepStyled, converter.PriorityEarly)
	// action-text-attachment elements that cannot render are dropped: a
	// missing url means the sanitizer would drop the element anyway, and a
	// /rails/active_storage/ url is a 404 left over from before the Go
	// migration. Any other attachment keeps raw HTML, which SanitizeHTML
	// rewrites to <img>/<a> on the write path.
	conv.Register.RendererFor("action-text-attachment", converter.TagTypeBlock, func(ctx converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
		url := ""
		for _, a := range n.Attr {
			if a.Key == "url" {
				url = a.Val
			}
		}
		if url == "" || strings.Contains(url, "/rails/active_storage/") {
			return converter.RenderSuccess
		}
		return base.RenderAsHTML(ctx, w, n)
	}, converter.PriorityEarly)
	// Images with sizing/style/class attrs keep raw HTML (Markdown has no size syntax).
	conv.Register.RendererFor("img", converter.TagTypeInline, func(ctx converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
		for _, a := range n.Attr {
			if a.Key == "width" || a.Key == "height" || a.Key == "style" || a.Key == "class" {
				return base.RenderAsHTML(ctx, w, n)
			}
		}
		return converter.RenderTryNext
	}, converter.PriorityEarly)
	// Emphasis delimiters follow CommonMark flanking rules, which break on
	// CJK text like <strong>foo：</strong>bar (a closing ** preceded by
	// punctuation and followed by a word char is not right-flanking). Keep
	// such spans as raw inline HTML.
	var emphasisTags = map[string]bool{"strong": true, "b": true, "em": true, "i": true, "del": true, "s": true, "strike": true}
	for tag := range emphasisTags {
		conv.Register.RendererFor(tag, converter.TagTypeInline, func(ctx converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
			if flankingRisk(n) {
				return base.RenderAsHTML(ctx, w, n)
			}
			return converter.RenderTryNext
		}, converter.PriorityEarly)
	}
	// A <pre> holding elements other than code/br (inline styles, images,
	// links — browsers render all of them inside pre) stays raw HTML; blank
	// lines inside are written as &#10; so the raw block survives goldmark's
	// HTML-block rule (type 6 ends at a blank line) while rendering the same
	// newline runs once the entity decodes.
	conv.Register.RendererFor("pre", converter.TagTypeBlock, func(ctx converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
		if preHasNonCodeContent(n) {
			renderRawPre(w, n)
			return converter.RenderSuccess
		}
		return converter.RenderTryNext
	}, converter.PriorityEarly)
	// Empty headings cannot be written in Markdown; keep raw.
	for _, tag := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
		conv.Register.RendererFor(tag, converter.TagTypeBlock, func(ctx converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
			if strings.TrimSpace(nodeText(n)) == "" {
				return base.RenderAsHTML(ctx, w, n)
			}
			return converter.RenderTryNext
		}, converter.PriorityEarly)
	}
	return conv
}

var boldTags = map[string]bool{"strong": true, "b": true}
var italicTags = map[string]bool{"em": true, "i": true}

// protectCrossAdjacentEmphasis renames bold elements directly touching italic
// elements (and vice versa) to kb/ki, so commonmark's MergeAdjacent cannot
// fuse them into a single emphasis run.
func protectCrossAdjacentEmphasis(ctx converter.Context, doc *html.Node) {
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Type == html.ElementNode && (boldTags[c.Data] || italicTags[c.Data]) {
				s := nextNonSpanSibling(c)
				if s != nil && s.Type == html.ElementNode &&
					((boldTags[c.Data] && italicTags[s.Data]) || (italicTags[c.Data] && boldTags[s.Data])) {
					renameEmphasisRun(c)
				}
			}
			walk(c)
			c = next
		}
	}
	walk(doc)
}

// nextNonSpanSibling mirrors MergeAdjacent's span-skipping walker: adjacency
// is what it would cross, not just the direct sibling.
func nextNonSpanSibling(n *html.Node) *html.Node {
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if s.Type == html.ElementNode && s.Data == "span" {
			continue
		}
		return s
	}
	return nil
}

// renameEmphasisRun renames start and every emphasis element adjacent to it
// so the whole run survives as raw HTML.
func renameEmphasisRun(start *html.Node) {
	rename := func(n *html.Node) {
		switch {
		case boldTags[n.Data]:
			n.Data = "kb"
			n.DataAtom = 0
		case italicTags[n.Data]:
			n.Data = "ki"
			n.DataAtom = 0
		}
	}
	rename(start)
	for s := nextNonSpanSibling(start); s != nil && s.Type == html.ElementNode && (boldTags[s.Data] || italicTags[s.Data]); s = nextNonSpanSibling(s) {
		rename(s)
	}
}

// renderRenamedEmphasis writes the node back with the given real tag name,
// children serialized as HTML.
func renderRenamedEmphasis(tag string) converter.HandleRenderFunc {
	return func(ctx converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
		w.WriteString("<" + tag + ">")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			html.Render(w, c)
		}
		w.WriteString("</" + tag + ">")
		return converter.RenderSuccess
	}
}

// preHasNonCodeContent reports whether a pre block holds elements other than
// code/br (inline styles, images, links) that plain code text would lose.
func preHasNonCodeContent(n *html.Node) bool {
	complex := false
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.ElementNode && x != n {
			switch x.Data {
			case "code", "br":
			default:
				complex = true
			}
		}
		for c := x.FirstChild; c != nil && !complex; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return complex
}

// renderRawPre writes a pre block as raw HTML, escaping each newline run as
// "\n&#10;" so goldmark's HTML-block rule — which ends type-6 blocks at a
// blank line — keeps the whole block intact. The entity decodes to a newline,
// so the rendered newline runs are identical to the source ("\n\n" would
// render one newline too many if written "\n&#10;\n"). Element subtrees are
// serialized to a buffer first so the blank-line escaping also reaches text
// nested inside them (and blank lines inside attribute values, which would
// end the HTML block just the same).
func renderRawPre(w converter.Writer, n *html.Node) {
	w.WriteString("\n\n<pre>")
	var ser func(x *html.Node)
	ser = func(x *html.Node) {
		if x.Type == html.TextNode {
			s := html.EscapeString(x.Data)
			s = strings.ReplaceAll(s, "\n\n", "\n&#10;")
			w.WriteString(s)
			return
		}
		if x.Type == html.ElementNode {
			var buf strings.Builder
			html.Render(&buf, x)
			w.WriteString(strings.ReplaceAll(buf.String(), "\n\n", "\n&#10;"))
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			ser(c)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		ser(c)
	}
	w.WriteString("</pre>\n\n")
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func isPunctRune(r rune) bool {
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}

// flankingRisk reports whether writing node n with ** or * delimiters would
// hit CommonMark flanking-rule failures: an opening run followed by
// punctuation can only open if preceded by whitespace/punctuation, and a
// closing run preceded by punctuation can only close if followed by
// whitespace/punctuation.
func flankingRisk(n *html.Node) bool {
	content := strings.TrimSpace(nodeText(n))
	if content == "" {
		return true
	}
	first, _ := utf8.DecodeRuneInString(content)
	last, _ := utf8.DecodeLastRuneInString(content)
	prev := ""
	if n.PrevSibling != nil {
		prev = n.PrevSibling.Data
		if n.PrevSibling.Type != html.TextNode {
			prev = nodeText(n.PrevSibling)
		}
	}
	next := ""
	if n.NextSibling != nil {
		next = n.NextSibling.Data
		if n.NextSibling.Type != html.TextNode {
			next = nodeText(n.NextSibling)
		}
	}
	if isPunctRune(first) && prev != "" {
		if r, _ := utf8.DecodeLastRuneInString(strings.TrimRight(prev, " \t\n")); !unicode.IsSpace(r) && !isPunctRune(r) {
			return true
		}
	}
	if isPunctRune(last) && next != "" {
		if r, _ := utf8.DecodeRuneInString(strings.TrimLeft(next, " \t\n")); !unicode.IsSpace(r) && !isPunctRune(r) {
			return true
		}
	}
	return false
}
