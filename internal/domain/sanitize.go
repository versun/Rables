package domain

import (
	"net/url"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
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

// maxHTMLParseBytes caps the input to the html.ParseFragment-based
// post-processing below. x/net/html bounds nesting depth (512) but not the
// total node count, so a multi-MB document of sibling elements explodes
// into millions of *html.Node (hundreds of MB of heap) and can OOM a small
// VPS. Over the cap the DOM pass is skipped: restrictMediaSrc re-scans with
// the tokenizer-based stripMediaSrcFallback (its media-src rule is a
// security control, so it may not just return the input unchanged), while
// AddLazyLoading returns the input unchanged (lazy loading is a rendering
// nicety, not a security control).
const maxHTMLParseBytes = 5 << 20

// SanitizeHTML ports Article#sanitize_html: bluemonday with the §4.4
// whitelist; src of iframe is restricted to absolute http/https URLs, and
// src of video/audio/source to http/https or /files/ root-relative paths.
// Submitted <action-text-attachment> elements (what the lexxy editor emits
// for uploaded files) are rewritten to plain <img>/<a> first: that is the
// canonical storage markup (same as the Rails migration rewrite produces),
// and the unknown element would otherwise not survive the whitelist.
func SanitizeHTML(rawHTML string) string {
	if IsBlank(rawHTML) {
		return ""
	}
	return restrictMediaSrc(sanitizePolicy.Sanitize(rewriteActionTextAttachments(rawHTML)))
}

// rewriteActionTextAttachments replaces every <action-text-attachment> that
// carries a url attribute per attachmentReplacement. Attachments without a
// url (e.g. a failed upload left in the markup) are left in place for the
// sanitizer to drop. Like the other DOM passes, input over maxHTMLParseBytes
// is returned unchanged.
func rewriteActionTextAttachments(rawHTML string) string {
	if !strings.Contains(rawHTML, "action-text-attachment") || len(rawHTML) > maxHTMLParseBytes {
		return rawHTML
	}
	nodes, err := html.ParseFragment(strings.NewReader(rawHTML), bodyContext)
	if err != nil {
		return rawHTML
	}
	// ParseFragment returns detached top-level nodes; attach them to a
	// container so node replacement can rely on Parent pointers.
	container := &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"}
	for _, n := range nodes {
		container.AppendChild(n)
	}
	var attachments []*html.Node
	walkNodes(container, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "action-text-attachment" {
			attachments = append(attachments, n)
		}
	})
	for _, n := range attachments {
		if repl := attachmentReplacement(n); repl != nil {
			n.Parent.InsertBefore(repl, n)
			n.Parent.RemoveChild(n)
		}
	}
	return renderFragment(containerChildren(container))
}

// attachmentReplacement builds the storage node for one
// <action-text-attachment>: an <img> for image types, a download <a> for
// everything else — mirroring the Rails migration rewrite. SVG is treated as
// a file, not an image: serveFile forces it to download (active content
// type), so an <img> would only render broken. Nil when the attachment has no
// url attribute.
func attachmentReplacement(n *html.Node) *html.Node {
	u := getAttr(n, "url")
	if u == "" {
		return nil
	}
	contentType := getAttr(n, "content-type")
	filename := getAttr(n, "filename")
	if strings.HasPrefix(contentType, "image/") && !strings.HasPrefix(contentType, "image/svg") {
		alt := getAttr(n, "caption")
		if alt == "" {
			alt = filename
		}
		img := &html.Node{Type: html.ElementNode, DataAtom: atom.Img, Data: "img", Attr: []html.Attribute{
			{Key: "src", Val: u},
			{Key: "alt", Val: alt},
		}}
		for _, key := range []string{"width", "height"} {
			if v := getAttr(n, key); v != "" {
				img.Attr = append(img.Attr, html.Attribute{Key: key, Val: v})
			}
		}
		return img
	}
	text := filename
	if text == "" {
		text = u
	}
	a := &html.Node{Type: html.ElementNode, DataAtom: atom.A, Data: "a", Attr: []html.Attribute{
		{Key: "href", Val: u},
	}}
	a.AppendChild(&html.Node{Type: html.TextNode, Data: text})
	return a
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func containerChildren(container *html.Node) []*html.Node {
	var out []*html.Node
	for c := container.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
	}
	return out
}

// mediaSrcElements are the elements whose src restrictMediaSrc hardens (§4.4).
var mediaSrcElements = map[string]bool{
	"iframe": true,
	"video":  true,
	"audio":  true,
	"source": true,
}

// restrictMediaSrc removes the src attribute from iframe/video/audio/source
// unless it satisfies allowedMediaSrc. This is more than hardening:
// bluemonday allows relative URLs globally, so the iframe rule (absolute
// http/https only — a same-origin iframe is a UI redress channel) is
// enforced nowhere else. Input over maxHTMLParseBytes or refused by
// html.ParseFragment (e.g. x/net/html's 512-node open-element stack cap)
// goes through stripMediaSrcFallback, which applies the same rule without
// building a DOM.
func restrictMediaSrc(rawHTML string) string {
	if len(rawHTML) > maxHTMLParseBytes {
		return stripMediaSrcFallback(rawHTML)
	}
	nodes, err := html.ParseFragment(strings.NewReader(rawHTML), bodyContext)
	if err != nil {
		return stripMediaSrcFallback(rawHTML)
	}
	for _, n := range nodes {
		walkNodes(n, func(n *html.Node) {
			if n.Type == html.ElementNode && mediaSrcElements[n.Data] {
				n.Attr = filterNonHTTPSrc(n.Data, n.Attr)
			}
		})
	}
	return renderFragment(nodes)
}

// stripMediaSrcFallback is the fail-closed path of restrictMediaSrc for
// inputs over maxHTMLParseBytes or that html.ParseFragment refuses. A
// tokenizer keeps no DOM and has no nesting limit, so oversized or deep
// input cannot defeat it; the src of every iframe/video/audio/source is
// filtered by the same allowedMediaSrc rule, and all other bytes pass
// through verbatim.
func stripMediaSrcFallback(rawHTML string) string {
	z := html.NewTokenizer(strings.NewReader(rawHTML))
	var b strings.Builder
	b.Grow(len(rawHTML))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return b.String()
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			b.Write(z.Raw())
			continue
		}
		// Copy the raw bytes first: Token may rewrite them in place.
		raw := append([]byte(nil), z.Raw()...)
		if tok := z.Token(); mediaSrcElements[tok.Data] {
			tok.Attr = filterNonHTTPSrc(tok.Data, tok.Attr)
			b.WriteString(tok.String())
		} else {
			b.Write(raw)
		}
	}
}

func filterNonHTTPSrc(elem string, attrs []html.Attribute) []html.Attribute {
	out := attrs[:0]
	for _, a := range attrs {
		if a.Key == "src" && !allowedMediaSrc(elem, a.Val) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// allowedMediaSrc reports whether src may stay on elem. iframe src must be
// an absolute http/https URL: a root-relative iframe can embed any
// same-origin page (e.g. an invisible full-viewport /admin overlay), a UI
// redress channel with no legitimate in-app use. video/audio/source may
// additionally use root-relative paths, but only under /files/ — the app's
// own stored media (twittersync's <video src="/files/...">).
func allowedMediaSrc(elem, src string) bool {
	if isHTTPURL(src) {
		return true
	}
	return elem != "iframe" && strings.HasPrefix(src, "/files/")
}

// isHTTPURL reports whether s is an absolute http(s) URL with a host. A
// bare "https:/admin" parses as {Scheme: https, Path: /admin} with no
// error, but browsers treat it as same-origin https://<site>/admin.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// AddLazyLoading ports Sanitization#add_lazy_loading_to_images: sets
// loading="lazy" on every <img> that has no loading attribute. Applied once
// when content_html is written (decision log 2026-08-03); rendering does not
// repeat it. Input over maxHTMLParseBytes is returned unchanged: lazy
// loading is a rendering nicety, not a security control.
func AddLazyLoading(rawHTML string) string {
	if IsBlank(rawHTML) {
		return rawHTML
	}
	if len(rawHTML) > maxHTMLParseBytes {
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
