package domain

import (
	"net/url"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

// AllowedHTMLTags mirrors Sanitization::ALLOWED_HTML_TAGS (48 tags).
var AllowedHTMLTags = []string{
	"p", "br", "div", "span",
	"h1", "h2", "h3", "h4", "h5", "h6",
	"a", "img",
	"ul", "ol", "li", "dl", "dt", "dd",
	"table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption", "colgroup", "col",
	"strong", "b", "em", "i", "u", "s", "strike", "del", "ins", "mark", "small",
	"blockquote", "q", "cite", "pre", "code", "kbd", "samp", "var",
	"hr",
	"figure", "figcaption",
	"article", "section", "aside", "header", "footer", "nav", "main",
	"details", "summary",
	"abbr", "address", "time",
	"sub", "sup",
	"ruby", "rt", "rp",
	"iframe", "video", "audio", "source",
}

// AllowedHTMLAttributes mirrors Sanitization::ALLOWED_HTML_ATTRIBUTES.
var AllowedHTMLAttributes = []string{
	"href", "src", "alt", "title", "class", "id", "style",
	"target", "rel",
	"width", "height",
	"colspan", "rowspan",
	"data-controller", "data-action", "data-target",
	"loading",
	"controls", "autoplay", "loop", "muted",
	"frameborder", "allow", "allowfullscreen",
	"name", "content",
}

// allowedURLSchemes mirrors Loofah's ACCEPTABLE_PROTOCOLS (the protocol
// safelist Rails::Html::SafeListSanitizer applies to href/src), minus "data".
var allowedURLSchemes = []string{
	"afs", "aim", "callto", "ed2k", "fax", "ftp", "gopher", "http", "https",
	"irc", "line", "mailto", "modem", "news", "nntp", "rsync", "rtsp", "sftp",
	"sms", "ssh", "tag", "tel", "telnet", "urn", "webcal", "xmpp",
}

var sanitizePolicy = newSanitizePolicy()

func newSanitizePolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements(AllowedHTMLTags...)
	// `style` passes through verbatim: no AllowStyles policies are
	// registered, so bluemonday does not rewrite the attribute (matching
	// Rails, which allows the attribute without scrubbing the CSS).
	p.AllowAttrs(AllowedHTMLAttributes...).Globally()
	p.AllowURLSchemes(allowedURLSchemes...)
	p.AllowRelativeURLs(true)
	return p
}

// SanitizeHTML ports Article#sanitize_html: bluemonday with the §4.4
// whitelist; src of iframe/video/audio/source is restricted to http/https.
func SanitizeHTML(rawHTML string) string {
	if IsBlank(rawHTML) {
		return ""
	}
	return restrictMediaSrc(sanitizePolicy.Sanitize(rawHTML))
}

// srcHTTPElements are the elements whose src may only be http/https (§4.4).
var srcHTTPElements = map[string]bool{
	"iframe": true,
	"video":  true,
	"audio":  true,
	"source": true,
}

// restrictMediaSrc removes the src attribute from iframe/video/audio/source
// unless it is an absolute http/https URL.
func restrictMediaSrc(rawHTML string) string {
	nodes, err := html.ParseFragment(strings.NewReader(rawHTML), bodyContext)
	if err != nil {
		return rawHTML
	}
	for _, n := range nodes {
		walkNodes(n, func(n *html.Node) {
			if n.Type == html.ElementNode && srcHTTPElements[n.Data] {
				n.Attr = filterNonHTTPSrc(n.Attr)
			}
		})
	}
	return renderFragment(nodes)
}

func filterNonHTTPSrc(attrs []html.Attribute) []html.Attribute {
	out := attrs[:0]
	for _, a := range attrs {
		if a.Key == "src" && !isHTTPURL(a.Val) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// AddLazyLoading ports Sanitization#add_lazy_loading_to_images: sets
// loading="lazy" on every <img> that has no loading attribute. Applied once
// when content_html is written (decision log 2026-08-03); rendering does not
// repeat it.
func AddLazyLoading(rawHTML string) string {
	if IsBlank(rawHTML) {
		return rawHTML
	}
	nodes, err := html.ParseFragment(strings.NewReader(rawHTML), bodyContext)
	if err != nil {
		return rawHTML
	}
	for _, n := range nodes {
		walkNodes(n, func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "img" && !hasAttr(n, "loading") {
				n.Attr = append(n.Attr, html.Attribute{Key: "loading", Val: "lazy"})
			}
		})
	}
	return renderFragment(nodes)
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func walkNodes(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkNodes(c, fn)
	}
}

func renderFragment(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		if err := html.Render(&b, n); err != nil {
			return ""
		}
	}
	return b.String()
}
