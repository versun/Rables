package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// ReservedSlugs mirrors Article::RESERVED_SLUGS — slugs that would collide
// with top-level routes.
var ReservedSlugs = []string{
	"admin", "tags", "pages", "users", "session", "setup", "confirm",
	"unsubscribe", "subscriptions", "static", "feed.xml", "sitemap.xml",
	"up", "rails", "twitter",
}

// IsReservedSlug reports whether slug collides with a top-level route.
func IsReservedSlug(slug string) bool {
	for _, r := range ReservedSlugs {
		if slug == r {
			return true
		}
	}
	return false
}

// GenerateSlug ports Article#generate_slug: keeps an existing slug, otherwise
// derives one from the title (parameterized; scripts that parameterize drops,
// e.g. Chinese, fall back to the cleaned-up title with a unique suffix), or
// from the current time when both are blank. Dots are stripped at the end.
// exists reports whether a candidate slug is already taken by another record;
// it may be nil to skip the uniqueness check.
func GenerateSlug(slug, title string, now time.Time, exists func(string) bool) string {
	if IsBlank(slug) {
		if !IsBlank(title) {
			if parameterized := Parameterize(title); parameterized != "" {
				slug = parameterized
			} else {
				slug = uniqueSlugFrom(Squish(title), exists)
			}
		} else {
			slug = Parameterize(now.Format("2006-01-02-15-04"))
		}
	}

	if strings.Contains(slug, ".") {
		slug = strings.ReplaceAll(slug, ".", "")
	}
	return slug
}

// uniqueSlugFrom ports Article#unique_slug_from: appends "-1", "-2", ...
// until the candidate is free.
func uniqueSlugFrom(base string, exists func(string) bool) string {
	candidate := base
	for counter := 1; exists != nil && exists(candidate); counter++ {
		candidate = fmt.Sprintf("%s-%d", base, counter)
	}
	return candidate
}

// transliterator approximates ActiveSupport::Inflector.transliterate for
// Latin diacritics (é→e, ü→u, ...); remaining non-ASCII runes are turned
// into separators by Parameterize, exactly like Rails' "?" replacement.
var transliterator = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)))

// Parameterize ports ActiveSupport's String#parameterize (separator "-"):
// transliterate to ASCII, replace runs of chars outside [a-z0-9\-_] with "-",
// strip leading/trailing separators, collapse duplicates, downcase.
func Parameterize(s string) string {
	if t, _, err := transform.String(transliterator, s); err == nil {
		s = t
	}

	var b strings.Builder
	b.Grow(len(s))
	lastWasSep := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-':
			b.WriteRune(r)
			lastWasSep = false
		case !lastWasSep:
			b.WriteByte('-')
			lastWasSep = true
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.ToLower(out)
}
