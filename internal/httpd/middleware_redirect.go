package httpd

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// redirectCacheTTL mirrors the 5-minute expiry on the Rails middleware's
// "redirect_middleware/enabled_redirects" cache entry.
const redirectCacheTTL = 5 * time.Minute

const redirectCacheKey = "httpd.redirect_rules"

// redirectSkipPrefixes never match path redirect rules (spec section 4.12; the
// Rails middleware skips /admin /assets /rails /up, the Go port serves
// uploaded files from /files and /static so those are skipped too). Host rules
// ignore the list: a matched subdomain redirects every path on it.
var redirectSkipPrefixes = []string{"/admin", "/assets", "/files", "/static", "/up"}

// redirectMatchOn values for the redirects.match_on column: what a rule's
// regex runs against.
const (
	redirectMatchPath = "path" // URL path (the Rails behavior)
	redirectMatchHost = "host" // request Host header, port stripped, lowercased
)

// redirectRule is one enabled redirect with its compiled pattern. replacement
// is already translated to Go's $1 expansion syntax.
type redirectRule struct {
	re          *regexp.Regexp
	replacement string
	permanent   bool
}

// redirectCacheEntry is the process-wide rule list snapshot. Host rules always
// run before path rules, regardless of creation order.
type redirectCacheEntry struct {
	hostRules []redirectRule
	pathRules []redirectRule
	fetchedAt time.Time
}

// redirectMiddleware applies enabled redirect rules, mirroring
// app/middleware/redirect_middleware.rb: only GET/HEAD requests are eligible,
// the first matching rule wins, permanent rules answer 301 and the rest 302.
// The Go port adds host rules (match_on='host', a subdomain-redirect
// extension): they run before every path rule, match against the request Host
// with any port stripped, and ignore redirectSkipPrefixes. The integrator
// wires it between accessLog and setupRedirect.
func (s *Server) redirectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		hostRules, pathRules := s.redirectRules(r.Context())
		if len(hostRules) > 0 {
			host := redirectRequestHost(r)
			// base is resolved lazily on the first relative target, once per
			// request, so a misconfigured site URL warns once rather than per
			// rule.
			var base string
			var baseOK, baseResolved bool
			for _, rule := range hostRules {
				target, ok := rule.applyTo(host)
				if !ok {
					continue
				}
				// A relative target ("/tags/\1") belongs to the canonical site,
				// not to the subdomain being redirected away from, so complete it
				// with the configured site URL. The request's own path and query
				// are never appended: the target is exactly what the rule yields.
				if strings.HasPrefix(target, "/") {
					if !baseResolved {
						base, baseOK = s.redirectSiteBaseURL(r.Context())
						baseResolved = true
					}
					if !baseOK {
						continue
					}
					target = base + target
				}
				s.writeRedirect(w, rule, host, target)
				return
			}
		}
		path := r.URL.Path
		for _, prefix := range redirectSkipPrefixes {
			// Segment-aware match: an article slug like /updates-2024 must
			// not be mistaken for the /up prefix.
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				next.ServeHTTP(w, r)
				return
			}
		}
		for _, rule := range pathRules {
			target, ok := rule.applyTo(path)
			if !ok {
				continue
			}
			s.writeRedirect(w, rule, path, target)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeRedirect answers a matched rule: 301 for permanent rules, 302 for the
// rest, with the match subject (path or host) logged as "from".
func (s *Server) writeRedirect(w http.ResponseWriter, rule redirectRule, from, target string) {
	status := http.StatusFound
	if rule.permanent {
		status = http.StatusMovedPermanently
	}
	s.Log.Info("redirect applied", "from", from, "to", target, "status", status)
	w.Header().Set("Location", target)
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(status)
}

// redirectRequestHost is the match subject for host rules: the request Host
// header with any port stripped, brackets around an IPv6 literal removed,
// lowercased because DNS is case-insensitive, and without a trailing root dot
// (abc.example.com. is the same name as abc.example.com).
func redirectRequestHost(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	host = strings.ToLower(host)
	return strings.TrimSuffix(host, ".")
}

// redirectSiteBaseURL is "scheme://host" of the configured site URL, used to
// complete relative host-rule targets. ok is false when no usable site URL is
// configured; the caller then treats the rule as non-matching (fails open)
// rather than emitting a relative Location the browser would resolve against
// the very subdomain the rule redirects away from.
func (s *Server) redirectSiteBaseURL(ctx context.Context) (string, bool) {
	st, err := s.Settings().Get(ctx)
	if err != nil {
		s.Log.Error("load settings for host redirect", "error", err)
		return "", false
	}
	u, err := url.Parse(strings.TrimSpace(st.Url.String))
	if !st.Url.Valid || err != nil || u.Scheme == "" || u.Host == "" {
		s.Log.Warn("host redirect with relative target needs an absolute site URL in settings")
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// InvalidateRedirectCache drops the cached rule list so the next request
// re-reads the redirects table; admin writes call it like the Rails model's
// after_save/after_destroy cache sweep.
func (s *Server) InvalidateRedirectCache() {
	s.Ext.Delete(redirectCacheKey)
}

// redirectRules returns the enabled rules split by match target, cached
// process-wide for redirectCacheTTL. A database error fails open (no
// redirects) and is not cached, so the next request retries.
func (s *Server) redirectRules(ctx context.Context) (host, path []redirectRule) {
	if v, ok := s.Ext.Load(redirectCacheKey); ok {
		if entry := v.(redirectCacheEntry); time.Since(entry.fetchedAt) < redirectCacheTTL {
			return entry.hostRules, entry.pathRules
		}
	}
	rows, err := s.Q.ListEnabledRedirects(ctx)
	if err != nil {
		s.Log.Error("load redirect rules", "error", err)
		return nil, nil
	}
	var entry redirectCacheEntry
	for _, row := range rows {
		pattern := row.Regex
		if row.MatchOn == redirectMatchHost {
			// Host rules must match the whole host: applyTo keeps unmatched
			// prefix/suffix (Ruby sub semantics), which is harmless for paths
			// but for hosts would splice raw Host bytes onto the target —
			// "^abc\.example\.com" (missing $) would turn the Host
			// abc.example.com.evil.example.com into the target
			// "https://google.com.evil.example.com". The non-capturing
			// wrapper preserves the admin's capture-group numbering and makes
			// ^/$ anchors optional.
			pattern = "^(?:" + pattern + ")$"
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			// Mirrors Redirect#match? rescuing RegexpError: a rule that does
			// not compile under RE2 (e.g. Ruby-only syntax) never matches.
			s.Log.Warn("skip uncompilable redirect rule", "id", row.ID, "error", err)
			continue
		}
		rule := redirectRule{
			re:          re,
			replacement: rubyToGoExpansion(row.Replacement),
			permanent:   row.Permanent == 1,
		}
		if row.MatchOn == redirectMatchHost {
			entry.hostRules = append(entry.hostRules, rule)
		} else {
			entry.pathRules = append(entry.pathRules, rule)
		}
	}
	entry.fetchedAt = time.Now()
	s.Ext.Store(redirectCacheKey, entry)
	return entry.hostRules, entry.pathRules
}

// applyTo mirrors Redirect#apply_to (sub with the first match): the matched
// span is replaced by the expanded replacement. ok is false when the subject
// (path or host) does not match.
func (rule redirectRule) applyTo(subject string) (string, bool) {
	loc := rule.re.FindStringSubmatchIndex(subject)
	if loc == nil {
		return "", false
	}
	var b []byte
	b = append(b, subject[:loc[0]]...)
	b = rule.re.ExpandString(b, rule.replacement, subject, loc)
	b = append(b, subject[loc[1]:]...)
	return string(b), true
}

// rubyToGoExpansion rewrites the Ruby sub-replacement syntax admins enter
// (\1-\9 and \0/\& backreferences, \k<name>/\k'name' named backreferences,
// \\ literal backslash) into Go ExpandString form (${1}/${0}/${name}, with
// literal dollars escaped). Unrecognized backslash forms stay literal.
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
		case c == '\\' && i+1 < len(repl) && repl[i+1] == '&':
			b.WriteString("${0}")
			i++
		case c == '\\' && i+2 < len(repl) && repl[i+1] == 'k' && (repl[i+2] == '<' || repl[i+2] == '\''):
			close := byte('>')
			if repl[i+2] == '\'' {
				close = '\''
			}
			if end := strings.IndexByte(repl[i+3:], close); end > 0 {
				b.WriteString("${")
				b.WriteString(repl[i+3 : i+3+end])
				b.WriteByte('}')
				i += 3 + end
			} else {
				b.WriteByte(c)
			}
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
