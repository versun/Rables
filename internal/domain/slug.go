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

// AdminArticleBatchSlugs and AdminPageBatchSlugs mirror the batch collection
// routes under /admin/posts and /admin/pages. chi matches static segments
// before {id}/{slug}, so a record slugged like a batch action still opens its
// edit form (GET falls through to {id}/edit), but the update — mapped to
// POST /admin/posts/{id} because HTML forms cannot PATCH — is swallowed by
// the batch handler and silently lost. They are separate from ReservedSlugs:
// those collide with top-level public routes, these only inside the admin
// namespaces, so pages (public under /pages/) reserve only the three actions
// the pages admin registers.
var (
	AdminArticleBatchSlugs = []string{
		"batch_destroy", "batch_publish", "batch_unpublish",
		"batch_add_tags", "batch_crosspost", "batch_newsletter",
	}
	AdminPageBatchSlugs = []string{
		"batch_destroy", "batch_publish", "batch_unpublish",
	}
)

// GenerateSlug ports Article#generate_slug: keeps an existing slug, otherwise
// derives one from the title (parameterized; scripts that parameterize drops,
// e.g. Chinese, fall back to the cleaned-up title with a unique suffix), or
// from the current time when both are blank. URL-unsafe characters are
// stripped at the end (CleanSlug), for handwritten and derived slugs alike.
// exists reports whether a candidate slug is already taken by another record;
// it may be nil to skip the uniqueness check.
func GenerateSlug(slug, title string, now time.Time, exists func(string) bool) string {
	if IsBlank(slug) {
		if !IsBlank(title) {
			if parameterized := Parameterize(title); parameterized != "" {
				slug = parameterized
			} else {
				slug = uniqueSlugFrom(CleanSlug(Squish(title)), exists)
			}
		} else {
			slug = Parameterize(now.Format("2006-01-02-15-04"))
		}
	}

	return CleanSlug(slug)
}

// slugUnsafeChars are the characters stripped from slugs: "." would collide
// with route suffixes (/tags/x.rss), the rest are URL path delimiters that
// html/template leaves unescaped in URL path position, so a slug containing
// them (e.g. the Chinese fallback for "你好？世界") breaks the public page and
// the admin links that embed it.
const slugUnsafeChars = "./?#%\\"

// CleanSlug strips URL-unsafe and control characters from a slug.
func CleanSlug(slug string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(slugUnsafeChars, r) {
			return -1
		}
		return r
	}, slug)
}

// IsValidSlug reports whether slug is free of URL-unsafe and control
// characters, i.e. CleanSlug would not change it.
func IsValidSlug(slug string) bool {
	return CleanSlug(slug) == slug
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

// Parameterize ports ActiveSupport's String#parameterize (separator "-"):
// transliterate to ASCII (NFD + dropping Mn marks approximates
// ActiveSupport::Inflector.transliterate for Latin diacritics, é→e, ü→u, ...;
// remaining non-ASCII runes become separators, exactly like Rails' "?"
// replacement), replace runs of chars outside [a-z0-9\-_] with "-", strip
// leading/trailing separators, collapse duplicates, downcase.
func Parameterize(s string) string {
	// The chain is rebuilt per call: transform.Chain buffers per-call state
	// on the struct, so a shared Transformer would race across goroutines.
	if t, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn))), s); err == nil {
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
