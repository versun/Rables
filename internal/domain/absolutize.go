package domain

import (
	"strings"

	"golang.org/x/net/html"
)

// AbsolutizeURLs rewrites root-relative href/src attribute values
// ("/files/...") into absolute URLs against base: stored media and file
// links must resolve in contexts without a same-origin host, like email.
// Values already absolute, protocol-relative ("//host/..."), or otherwise
// not root-relative pass through. base is trimmed and one trailing slash
// chomped, like siteURL; an empty base (site URL unset) returns the input
// unchanged, mirroring the relative-path fallback of the mailers. Input over
// maxHTMLParseBytes or refused by html.ParseFragment is returned unchanged.
func AbsolutizeURLs(rawHTML, base string) string {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	if base == "" || IsBlank(rawHTML) {
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
			if n.Type != html.ElementNode {
				return
			}
			for i, a := range n.Attr {
				if (a.Key == "href" || a.Key == "src") && isRootRelativeURL(a.Val) {
					n.Attr[i].Val = base + a.Val
				}
			}
		})
	}
	return renderFragment(nodes)
}

// isRootRelativeURL reports whether u is a root-relative path ("/x"); a
// protocol-relative URL ("//host/x") already resolves off-site.
func isRootRelativeURL(u string) bool {
	return strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//")
}
