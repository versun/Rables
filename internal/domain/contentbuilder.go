package domain

import (
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Platform default max_characters (Crosspost#default_max_characters).
const (
	DefaultMaxCharactersMastodon = 500
	DefaultMaxCharactersTwitter  = 250
	DefaultMaxCharactersBluesky  = 300
	DefaultMaxCharactersOther    = 300
)

// EffectiveMaxCharacters ports Crosspost#effective_max_characters: a
// configured value wins, otherwise the platform default applies.
func EffectiveMaxCharacters(platform string, configured int) int {
	if configured > 0 {
		return configured
	}
	switch platform {
	case "mastodon":
		return DefaultMaxCharactersMastodon
	case "twitter":
		return DefaultMaxCharactersTwitter
	default: // bluesky and anything else
		return DefaultMaxCharactersOther
	}
}

// PlatformCountNonASCIIDouble reports whether the platform counts non-ASCII
// characters as two (twitter_service.rb passes count_non_ascii_double: true).
func PlatformCountNonASCIIDouble(platform string) bool {
	return platform == "twitter"
}

// ContentInput carries the article fields BuildContent needs (the Ruby
// version reads them off the Article object).
type ContentInput struct {
	Slug        string
	Title       string
	PlainText   string // article.plain_text_content
	Description string // article.description; non-blank wins over PlainText
	SourceURL   string // article.source_url; non-blank means has_source?
}

// BuildOptions mirrors build_content's keyword arguments.
type BuildOptions struct {
	MaxLength           int    // <= 0 falls back to the Ruby default of 300
	AlwaysAddLink       bool   // always_add_link
	CountNonASCIIDouble bool   // count_non_ascii_double (Twitter)
	SiteURL             string // Setting.url; blank falls back to http://localhost:3000
	RoutePrefix         string // ARTICLE_ROUTE_PREFIX
}

// BuildContent ports ContentBuilder#build_content line by line.
func BuildContent(in ContentInput, opt BuildOptions) string {
	maxLength := opt.MaxLength
	if maxLength <= 0 {
		maxLength = 300
	}
	double := opt.CountNonASCIIDouble

	title := in.Title
	contentText := in.PlainText
	if !IsBlank(in.Description) {
		contentText = in.Description
	}

	// Source reference always goes last, after the Read more link.
	sourceURLText := ""
	if !IsBlank(in.SourceURL) {
		sourceURLText = "\n" + in.SourceURL
	}

	titleLength := CountChars(title, double)
	contentLength := CountChars(contentText, double)
	sourceURLLength := CountChars(sourceURLText, double)

	bodyLength := contentLength
	if !IsBlank(title) {
		bodyLength = titleLength + contentLength + 1 // +1 for newline
	}
	totalLength := bodyLength + sourceURLLength
	needsLink := opt.AlwaysAddLink || totalLength >= maxLength || !IsBlank(in.Description)

	if !needsLink {
		if !IsBlank(title) {
			return title + "\n" + contentText + sourceURLText
		}
		return contentText + sourceURLText
	}

	postURL := BuildPostURL(opt.SiteURL, opt.RoutePrefix, in.Slug)
	linkText := "\nRead more: " + postURL
	linkLength := CountChars(linkText, false)
	if double {
		linkText = "\nRead more:" + postURL
		linkLength = 34
	}

	suffixText := linkText + sourceURLText
	suffixLength := linkLength + sourceURLLength

	availableLength := maxLength - suffixLength
	if availableLength <= 0 {
		return lstrip(suffixText)
	}

	if !IsBlank(title) {
		if titleLength >= availableLength-3 {
			var truncatedTitle string
			if availableLength > 3 {
				truncatedTitle = TruncateText(title, availableLength-3, double) + "..."
			} else {
				truncatedTitle = TruncateText(title, availableLength, double)
			}
			return truncatedTitle + suffixText
		}

		remainingLength := availableLength - titleLength - 1 // -1 for newline after title

		if remainingLength <= 4 {
			return title + suffixText
		}
		if contentLength <= remainingLength {
			return title + "\n" + contentText + suffixText
		}
		return title + "\n" + TruncateText(contentText, remainingLength-3, double) + "..." + suffixText
	}

	if contentLength <= availableLength {
		return contentText + suffixText
	}
	if availableLength <= 4 {
		return lstrip(suffixText)
	}
	return TruncateText(contentText, availableLength-3, double) + "..." + suffixText
}

// CountChars ports ContentBuilder#count_chars: rune count, with non-ASCII
// runes counting as 2 when countNonASCIIDouble is set.
func CountChars(s string, countNonASCIIDouble bool) int {
	if !countNonASCIIDouble {
		return utf8.RuneCountInString(s)
	}
	n := 0
	for _, r := range s {
		if r < 128 {
			n++
		} else {
			n += 2
		}
	}
	return n
}

// TruncateText ports ContentBuilder#truncate_text.
func TruncateText(s string, maxLength int, countNonASCIIDouble bool) string {
	if maxLength <= 0 {
		return ""
	}
	if !countNonASCIIDouble {
		r := []rune(s)
		if maxLength >= len(r) {
			return s
		}
		return string(r[:maxLength])
	}

	currentLength := 0
	var chars []rune
	for _, r := range s {
		charLength := 1
		if r >= 128 {
			charLength = 2
		}
		if currentLength+charLength > maxLength {
			break
		}
		currentLength += charLength
		chars = append(chars, r)
	}
	return string(chars)
}

var httpSchemeRe = regexp.MustCompile(`^https?://`)

// BuildPostURL ports ContentBuilder#build_post_url: adds a scheme when
// missing, keeps a non-default port, and prefixes the article path with
// routePrefix (ARTICLE_ROUTE_PREFIX).
func BuildPostURL(siteURL, routePrefix, slug string) string {
	if IsBlank(siteURL) {
		siteURL = "http://localhost:3000"
	}
	siteURL = strings.TrimSuffix(siteURL, "/") // Ruby chomp("/")
	if !httpSchemeRe.MatchString(siteURL) {
		siteURL = "https://" + siteURL
	}

	u, err := url.Parse(siteURL)
	if err != nil || u.Hostname() == "" {
		return siteURL + "/" + slug
	}

	host := u.Hostname()
	if port := u.Port(); port != "" && !isDefaultPort(u.Scheme, port) {
		host = net.JoinHostPort(host, port)
	}

	path := "/" + slug
	if prefix := strings.Trim(routePrefix, "/"); prefix != "" {
		path = "/" + prefix + path
	}
	return (&url.URL{Scheme: u.Scheme, Host: host, Path: path}).String()
}

func isDefaultPort(scheme, port string) bool {
	return scheme == "http" && port == "80" || scheme == "https" && port == "443"
}

// lstrip mirrors Ruby's String#lstrip (strips " \t\n\v\f\r\0").
func lstrip(s string) string {
	return strings.TrimLeft(s, " \t\n\v\f\r\x00")
}
