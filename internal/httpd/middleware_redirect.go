package httpd

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// redirectCacheTTL mirrors the 5-minute expiry on the Rails middleware's
// "redirect_middleware/enabled_redirects" cache entry.
const redirectCacheTTL = 5 * time.Minute

const redirectCacheKey = "httpd.redirect_rules"

// redirectSkipPrefixes never match redirect rules (spec section 4.12; the
// Rails middleware skips /admin /assets /rails /up, the Go port serves
// uploaded files from /files and /static so those are skipped too).
var redirectSkipPrefixes = []string{"/admin", "/assets", "/files", "/static", "/up"}

// redirectRule is one enabled redirect with its compiled pattern. replacement
// is already translated to Go's $1 expansion syntax.
type redirectRule struct {
	re          *regexp.Regexp
	replacement string
	permanent   bool
}

// redirectCacheEntry is the process-wide rule list snapshot.
type redirectCacheEntry struct {
	rules     []redirectRule
	fetchedAt time.Time
}

// redirectMiddleware applies enabled redirect rules, mirroring
// app/middleware/redirect_middleware.rb: only GET/HEAD requests are eligible,
// the first matching rule wins, permanent rules answer 301 and the rest 302.
// The integrator wires it between accessLog and setupRedirect.
func (s *Server) redirectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		for _, prefix := range redirectSkipPrefixes {
			if strings.HasPrefix(path, prefix) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		for _, rule := range s.redirectRules(r.Context()) {
			target, ok := rule.applyTo(path)
			if !ok {
				continue
			}
			status := http.StatusFound
			if rule.permanent {
				status = http.StatusMovedPermanently
			}
			s.Log.Info("redirect applied", "from", path, "to", target, "status", status)
			w.Header().Set("Location", target)
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// InvalidateRedirectCache drops the cached rule list so the next request
// re-reads the redirects table; admin writes call it like the Rails model's
// after_save/after_destroy cache sweep.
func (s *Server) InvalidateRedirectCache() {
	s.Ext.Delete(redirectCacheKey)
}

// redirectRules returns the enabled rules, cached process-wide for
// redirectCacheTTL. A database error fails open (no redirects) and is not
// cached, so the next request retries.
func (s *Server) redirectRules(ctx context.Context) []redirectRule {
	if v, ok := s.Ext.Load(redirectCacheKey); ok {
		if entry := v.(redirectCacheEntry); time.Since(entry.fetchedAt) < redirectCacheTTL {
			return entry.rules
		}
	}
	rows, err := s.Q.ListEnabledRedirects(ctx)
	if err != nil {
		s.Log.Error("load redirect rules", "error", err)
		return nil
	}
	rules := make([]redirectRule, 0, len(rows))
	for _, row := range rows {
		re, err := regexp.Compile(row.Regex)
		if err != nil {
			// Mirrors Redirect#match? rescuing RegexpError: a rule that does
			// not compile under RE2 (e.g. Ruby-only syntax) never matches.
			s.Log.Warn("skip uncompilable redirect rule", "id", row.ID, "error", err)
			continue
		}
		rules = append(rules, redirectRule{
			re:          re,
			replacement: rubyToGoExpansion(row.Replacement),
			permanent:   row.Permanent == 1,
		})
	}
	s.Ext.Store(redirectCacheKey, redirectCacheEntry{rules: rules, fetchedAt: time.Now()})
	return rules
}

// applyTo mirrors Redirect#apply_to (path.sub with the first match): the
// matched span is replaced by the expanded replacement. ok is false when the
// path does not match.
func (rule redirectRule) applyTo(path string) (string, bool) {
	loc := rule.re.FindStringSubmatchIndex(path)
	if loc == nil {
		return "", false
	}
	var b []byte
	b = append(b, path[:loc[0]]...)
	b = rule.re.ExpandString(b, rule.replacement, path, loc)
	b = append(b, path[loc[1]:]...)
	return string(b), true
}

// rubyToGoExpansion rewrites the Ruby sub-replacement syntax admins enter
// (\1 backreferences, \\ literal backslash) into Go ExpandString form
// (${1}, with literal dollars escaped).
func rubyToGoExpansion(repl string) string {
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		switch {
		case c == '\\' && i+1 < len(repl) && repl[i+1] >= '0' && repl[i+1] <= '9':
			b.WriteString("${")
			b.WriteByte(repl[i+1])
			b.WriteByte('}')
			i++
		case c == '\\' && i+1 < len(repl) && repl[i+1] == '\\':
			b.WriteByte('\\')
			i++
		case c == '$':
			b.WriteString("$$")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
