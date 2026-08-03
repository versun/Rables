// content rewriting for migrated article/page bodies (spec section 8.4):
// <action-text-attachment sgid> elements and old-style
// /rails/active_storage/blobs/redirect/<signed_id>/... URLs are resolved back
// to ActiveStorage blob ids (base64 JSON decode, signature NOT verified -
// see the plan decision log) and rewritten to /files/<key> references.
// Unresolvable references are kept verbatim and listed in the report.
package railsmigrate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// blobRef is the old active_storage_blobs row needed for rewrites.
type blobRef struct {
	key         string
	filename    string
	contentType string
}

var (
	// gid://<app>/ActiveStorage::Blob/123[?expires_in]
	blobGIDRe = regexp.MustCompile(`ActiveStorage::Blob/(\d+)`)
	// [scheme://host]/rails/active_storage/{blobs|representations}/{redirect|proxy}/<signed_id>/...
	// (old bodies carry both relative and absolute-with-host variants)
	railsURLRe = regexp.MustCompile(`^(?:https?://[^/]+)?/rails/active_storage/(?:blobs|representations)/(?:redirect|proxy)/([^/"]+)`)
)

// KeptRef is one body reference that could not be rewritten; listed in the
// report as the manual fixup list.
type KeptRef struct {
	Record  string // e.g. "Article/123"
	Reason  string
	Snippet string
}

// RewriteStats counts rewritten vs kept references across all bodies.
type RewriteStats struct {
	Rewritten int
	Kept      []KeptRef
}

// rewriteContent rewrites attachment references in one body. blobs maps old
// blob id -> file info. Bodies without any marker are returned byte-identical.
func rewriteContent(body string, blobs map[int64]blobRef, record string, stats *RewriteStats) string {
	if body == "" || !strings.Contains(body, "action-text-attachment") && !strings.Contains(body, "/rails/active_storage/") {
		return body
	}
	ctx := &html.Node{Type: html.ElementNode, DataAtom: atom.Body, Data: "body"}
	nodes, err := html.ParseFragment(strings.NewReader(body), ctx)
	if err != nil {
		stats.Kept = append(stats.Kept, KeptRef{Record: record, Reason: "html parse: " + err.Error(), Snippet: truncate(body, 200)})
		return body
	}
	// ParseFragment returns detached top-level nodes; attach them to a
	// container so node replacement can rely on Parent pointers.
	container := &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"}
	for _, n := range nodes {
		container.AppendChild(n)
	}
	rewriteNode(container, blobs, record, stats)
	var buf bytes.Buffer
	for c := container.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&buf, c); err != nil {
			stats.Kept = append(stats.Kept, KeptRef{Record: record, Reason: "html render: " + err.Error(), Snippet: truncate(body, 200)})
			return body
		}
	}
	return buf.String()
}

func rewriteNode(n *html.Node, blobs map[int64]blobRef, record string, stats *RewriteStats) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		rewriteNode(c, blobs, record, stats)
		c = next
	}
	if n.Type != html.ElementNode {
		return
	}
	switch n.Data {
	case "action-text-attachment":
		rewriteAttachment(n, blobs, record, stats)
	case "img":
		if src := attr(n, "src"); railsURLRe.MatchString(src) {
			rewriteURLAttr(n, "src", src, blobs, record, stats)
		}
	case "a":
		if href := attr(n, "href"); railsURLRe.MatchString(href) {
			rewriteURLAttr(n, "href", href, blobs, record, stats)
		}
	}
}

// rewriteAttachment replaces an <action-text-attachment> element with an
// <img> (image blobs) or an <a> (other blobs) per spec section 8.4.
func rewriteAttachment(n *html.Node, blobs map[int64]blobRef, record string, stats *RewriteStats) {
	id, err := blobIDFromSGID(attr(n, "sgid"))
	if err != nil {
		// fall back to the url attribute, which carries the same signed id
		id, err = blobIDFromSignedURL(attr(n, "url"))
	}
	if err != nil {
		keep(n, record, "unresolvable sgid/url", stats)
		return
	}
	blob, ok := blobs[id]
	if !ok {
		keep(n, record, fmt.Sprintf("blob %d not found", id), stats)
		return
	}
	var repl *html.Node
	if strings.HasPrefix(blob.contentType, "image/") {
		repl = &html.Node{Type: html.ElementNode, Data: "img", Attr: []html.Attribute{
			{Key: "src", Val: "/files/" + blob.key},
			{Key: "alt", Val: blob.filename},
			{Key: "loading", Val: "lazy"}, // Go write-path invariant, see plan decision log
		}}
	} else {
		repl = &html.Node{Type: html.ElementNode, Data: "a", Attr: []html.Attribute{
			{Key: "href", Val: "/files/" + blob.key},
		}}
		repl.AppendChild(&html.Node{Type: html.TextNode, Data: blob.filename})
	}
	n.Parent.InsertBefore(repl, n)
	n.Parent.RemoveChild(n)
	stats.Rewritten++
}

// rewriteURLAttr points an old /rails/active_storage/... attribute at
// /files/<key>, keeping every other attribute untouched.
func rewriteURLAttr(n *html.Node, key, val string, blobs map[int64]blobRef, record string, stats *RewriteStats) {
	id, err := blobIDFromSignedURL(val)
	if err != nil {
		keep(n, record, "unresolvable signed url", stats)
		return
	}
	blob, ok := blobs[id]
	if !ok {
		keep(n, record, fmt.Sprintf("blob %d not found", id), stats)
		return
	}
	setAttr(n, key, "/files/"+blob.key)
	stats.Rewritten++
}

func keep(n *html.Node, record, reason string, stats *RewriteStats) {
	var buf bytes.Buffer
	if err := html.Render(&buf, n); err != nil {
		buf.WriteString("<unrenderable>")
	}
	stats.Kept = append(stats.Kept, KeptRef{Record: record, Reason: reason, Snippet: truncate(buf.String(), 200)})
}

// blobIDFromSGID decodes an ActionText sgid and extracts the blob id from the
// gid payload (gid://<app>/ActiveStorage::Blob/<id>).
func blobIDFromSGID(sgid string) (int64, error) {
	if sgid == "" {
		return 0, fmt.Errorf("empty sgid")
	}
	payload, err := decodeSigned(sgid)
	if err != nil {
		return 0, err
	}
	if m := blobGIDRe.FindStringSubmatch(payload); m != nil {
		return strconv.ParseInt(m[1], 10, 64)
	}
	// older format: the payload itself is JSON carrying the gid
	var v struct {
		GID string `json:"gid"`
	}
	if json.Unmarshal([]byte(payload), &v) == nil && v.GID != "" {
		if m := blobGIDRe.FindStringSubmatch(v.GID); m != nil {
			return strconv.ParseInt(m[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("no blob id in sgid payload")
}

// blobIDFromSignedURL decodes the signed_id segment of an old-style
// /rails/active_storage/... URL into a blob id.
func blobIDFromSignedURL(u string) (int64, error) {
	m := railsURLRe.FindStringSubmatch(u)
	if m == nil {
		return 0, fmt.Errorf("not a rails active_storage url")
	}
	seg, err := url.PathUnescape(m[1])
	if err != nil {
		seg = m[1]
	}
	payload, err := decodeSigned(seg)
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(payload), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("signed id payload %q is not a blob id", truncate(payload, 40))
	}
	return id, nil
}

// decodeSigned strips the "--signature" trailer and unwraps the
// ActiveSupport::MessageVerifier JSON envelope ({_rails:{data|message:...}}),
// returning the inner payload. The signature is intentionally not verified.
func decodeSigned(s string) (string, error) {
	if i := strings.LastIndex(s, "--"); i > 0 {
		s = s[:i]
	}
	raw, err := b64decode(s)
	if err != nil {
		return "", fmt.Errorf("base64: %w", err)
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw), nil // payload is not the verifier envelope
	}
	m, ok := v.(map[string]any)
	if !ok {
		return scalarString(v), nil // bare JSON scalar (e.g. a blob id)
	}
	env, ok := m["_rails"].(map[string]any)
	if !ok {
		return string(raw), nil
	}
	inner, ok := env["data"]
	if !ok {
		inner = env["message"]
	}
	switch t := inner.(type) {
	case string:
		// string payloads are base64-encoded inside the envelope
		if b, err := b64decode(t); err == nil && printable(b) {
			return string(b), nil
		}
		return t, nil
	default:
		return scalarString(t), nil
	}
}

// b64decode accepts standard/URL alphabets with or without padding.
func b64decode(s string) ([]byte, error) {
	encs := []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	}
	var err error
	for _, enc := range encs {
		var b []byte
		if b, err = enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, err
}

func scalarString(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case string:
		return t
	default:
		return ""
	}
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return len(b) > 0
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i, a := range n.Attr {
		if a.Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
