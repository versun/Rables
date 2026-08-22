package domain

import (
	"html"
	"net/url"
	"strings"
)

// SourceReferenceHeader renders the header of articles/_source_reference.html.erb,
// shared by the public pages (internal/httpd) and the listmonk campaign body
// (internal/service/newsletter) so the two ports cannot drift: a link (with a
// jump icon) pointing at the source URL, labeled with the source author when
// set, falling back to 引用. source_url and source_author may come from
// attacker-controlled imports/syncs, so the author is escaped and only
// absolute http(s) URLs with a host become a link (the safe_archive_url rule).
func SourceReferenceHeader(rawURL, author string) string {
	label := "引用"
	if !IsBlank(author) {
		label = html.EscapeString(author)
	}
	var b strings.Builder
	if safeURL := safeSourceURL(rawURL); safeURL != "" {
		b.WriteString(`<a href="` + html.EscapeString(safeURL) + `" target="_blank" rel="noopener noreferrer" style="color: #495057; text-decoration: none;">`)
		b.WriteString(`<span style="font-weight: 600; font-size: 0.95rem;">` + label + `</span>`)
		b.WriteString(` <i class="fas fa-external-link-alt" style="font-size: 0.75rem;"></i>`)
		b.WriteString(`</a>`)
	} else {
		b.WriteString(`<span style="font-weight: 600; color: #495057; font-size: 0.95rem;">` + label + `</span>`)
	}
	return b.String()
}

// safeSourceURL mirrors safe_archive_url: only absolute http(s) URLs with a
// host survive; anything else (javascript:, data:, relative) is dropped.
func safeSourceURL(value string) string {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return u.String()
	}
	return ""
}
