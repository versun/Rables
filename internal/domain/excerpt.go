package domain

// ExcerptLength mirrors Article::EXCERPT_LENGTH.
const ExcerptLength = 200

// BuildExcerpt ports Article#build_excerpt: uses the description when
// present, otherwise the plain-text content; the result is squished and
// truncated at ExcerptLength on a whitespace boundary. Returns "" when the
// source is blank (Rails stores nil).
func BuildExcerpt(description, contentHTML string) string {
	source := description
	if IsBlank(source) {
		source = PlainText(contentHTML)
	}
	text := Squish(source)
	if text == "" {
		return ""
	}
	return truncateWords(text, ExcerptLength)
}

// truncateWords ports ActiveSupport's String#truncate(n, separator: /\s/):
// no-op when len(s) <= n, otherwise cut at the last whitespace within the
// first n-3 characters and append "...".
func truncateWords(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	stop := n - len("...")
	for i := stop; i >= 0; i-- {
		if isWordSeparator(r[i]) {
			stop = i
			break
		}
	}
	return string(r[:stop]) + "..."
}

// isWordSeparator matches Ruby's /\s/: [ \t\r\n\f\v].
func isWordSeparator(r rune) bool {
	switch r {
	case ' ', '\t', '\r', '\n', '\f', '\v':
		return true
	}
	return false
}
