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

// redirectSkipPrefixes never match path-subject redirect rules (spec section
// 4.12; the Rails middleware skips /admin /assets /rails /up, the Go port serves
// uploaded files from /files and /static so those are skipped too). Rules whose
// subject is host+path ignore the list: a matched subdomain redirects every
// path on it.
var redirectSkipPrefixes = []string{"/admin", "/assets", "/files", "/static", "/up"}

// redirectMatchOn values for the redirects.match_on column.
const (
	redirectMatchPath     = "path"      // regex against the URL path (the Rails behavior)
	redirectMatchHostPath = "host_path" // regex against host+path, e.g. blog.example.com/blog/foo
	redirectMatchSimple   = "simple"    // no regex: plain match_from string, exact or prefix
	// redirectMatchHostLegacy is the pre-migration-0008 value: regex against
	// the Host header alone. It can still arrive via a restored backup (the
	// transfer import bypasses migrations) and is rewritten at load time.
	redirectMatchHostLegacy = "host"
)

// redirectRule is one enabled redirect. Two kinds:
//
//   - simple (from != ""): plain-string compare of the subject, exact or
//     prefix; a prefix match carries the remainder onto the target.
//   - regex (from == ""): pattern matched against the subject. Path rules keep
//     Ruby sub semantics (only the matched span is replaced); host+path rules
//     must match the whole subject — the ^(?:...)$ wrapper is added at load,
//     because splicing unmatched raw Host bytes onto the target would let
//     "^abc\.example\.com" (missing $) turn abc.example.com.evil.example.com
//     into "https://google.com.evil.example.com".
//
// The match subject is the URL path for path rules and the request Host (port
// stripped, lowercased) plus the path for host+path rules.
type redirectRule struct {
	hostPath  bool   // match subject is host+path instead of just the path
	permanent bool   // 301 instead of 302
	from      string // simple mode: match string, host part lowercased at save time
	prefix    bool   // simple mode: prefix match carrying the remainder
	to        string // simple mode target as entered
	// regex mode: compiled pattern and replacement already translated to Go's
	// $1 expansion syntax.
	re          *regexp.Regexp
	replacement string
}

// redirectCacheEntry is the process-wide rule list snapshot: one ordered list,
// the first matching rule wins.
type redirectCacheEntry struct {
	rules     []redirectRule
	fetchedAt time.Time
}

// redirectMiddleware applies enabled redirect rules in position order, first
// match wins, mirroring app/middleware/redirect_middleware.rb: only GET/HEAD
// requests are eligible, permanent rules answer 301 and the rest 302. The Go
// port adds host+path rules and the simple no-regex mode (subdomain-redirect
// extensions). The integrator wires it between stripTrailingSlash and
// setupRedirect.
func (s *Server) redirectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		rules := s.redirectRules(r.Context())
		if len(rules) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		path := r.URL.Path
		skipPath := redirectSkippablePath(path)
		// The host and the host+path subject are resolved lazily: most sites
		// only carry path rules. The site base URL for relative host-rule
		// targets is likewise resolved on first use, so a misconfigured site
		// URL warns once rather than per rule.
		var host, hostPath string
		var hostPathResolved bool
		resolveHostPath := func() {
			if !hostPathResolved {
				host = redirectRequestHost(r)
				hostPath = host + path
				hostPathResolved = true
			}
		}
		var base string
		var baseOK, baseResolved bool
		for _, rule := range rules {
			var subject string
			if rule.hostPath {
				resolveHostPath()
				subject = hostPath
			} else {
				if skipPath {
					continue
				}
				subject = path
			}
			target, ok := rule.applyTo(subject)
			if !ok {
				continue
			}
			// A relative target ("/tags/blog") of a host+path rule belongs to
			// the canonical site, not to the subdomain being redirected away
			// from, so complete it with the configured site URL. Path-rule
			// targets stay relative and resolve against the request's own host.
			// The request's query string is never appended: the target is
			// exactly what the rule yields.
			if rule.hostPath && strings.HasPrefix(target, "/") {
				if !baseResolved {
					base, baseOK = s.redirectSiteBaseURL(r.Context())
					baseResolved = true
				}
				if !baseOK {
					continue
				}
				target = base + target
			}
			// A path-rule target keeps the request path's unmatched prefix, so
			// a crafted path like //evil.example/old would leak into Location
			// as a protocol-relative URL pointing off-site. No legitimate rule
			// yields a protocol-relative target (simple targets are validated,
			// host-rule targets are completed with the site base), so treat it
			// as non-matching.
			if strings.HasPrefix(target, "//") {
				continue
			}
			// A rule mapping the request onto itself would bounce the browser
			// through an endless redirect loop; treat it as non-matching. The
			// guard compares absolute targets against the request host, which
			// path rules have not needed up to this point.
			resolveHostPath()
			if redirectTargetsSelf(target, host, path) {
				continue
			}
			s.writeRedirect(w, rule, subject, target)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// redirectSkippablePath reports whether path falls under a skip prefix. The
// match is segment-aware: an article slug like /updates-2024 must not be
// mistaken for the /up prefix.
func redirectSkippablePath(path string) bool {
	for _, prefix := range redirectSkipPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// writeRedirect answers a matched rule: 301 for permanent rules, 302 for the
// rest, with the match subject (path or host+path) logged as "from".
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

// redirectTargetsSelf reports whether a matched rule's final target sends the
// request back to itself: the same path for a relative target, the same host
// and path for an absolute one. Scheme and query are ignored — the middleware
// matches on neither, so a rule differing only in those loops just the same.
// Only the exact self-target is caught; loops spanning several rules (A→B→A)
// remain the admin's responsibility.
func redirectTargetsSelf(target, reqHost, reqPath string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	targetPath := u.Path
	if targetPath == "" {
		targetPath = "/"
	}
	if u.Host == "" {
		// A relative target carries no host; compare paths only, so a rule
		// differing from the request just in query or fragment ("/old?x=1")
		// counts as self too — the middleware matches on the path alone and
		// such a rule would loop the browser exactly like "/old" would.
		return targetPath == reqPath
	}
	return normalizeRedirectHost(u.Host) == reqHost && targetPath == reqPath
}

// redirectRequestHost normalizes the request Host header: any port stripped,
// brackets around an IPv6 literal removed, lowercased because DNS is
// case-insensitive, and without a trailing root dot (abc.example.com. is the
// same name as abc.example.com).
func redirectRequestHost(r *http.Request) string {
	return normalizeRedirectHost(r.Host)
}

// normalizeRedirectHost applies redirectRequestHost's normalization to any
// host[:port] string.
func normalizeRedirectHost(host string) string {
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

// redirectRules returns the enabled rules in evaluation order (position, then
// id), cached process-wide for redirectCacheTTL. A database error fails open
// (no redirects) and is not cached, so the next request retries.
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
	var entry redirectCacheEntry
	for _, row := range rows {
		rule := redirectRule{permanent: row.Permanent == 1}
		if row.MatchOn == redirectMatchSimple {
			// match_on is authoritative; the host part is normalized at load
			// like at save time, so a transfer-imported row (which bypasses
			// the admin form) matches what the middleware sees. A blank
			// match_from is skipped: falling through to the regex branch
			// would compile an empty pattern that matches everything.
			from := normalizeMatchFrom(row.MatchFrom)
			if from == "" {
				s.Log.Warn("skip redirect rule with blank match_from", "id", row.ID)
				continue
			}
			rule.from = from
			rule.prefix = row.MatchPrefix == 1
			rule.to = row.Replacement
			rule.hostPath = !strings.HasPrefix(from, "/")
		} else {
			// Mirrors Redirect#match? rescuing RegexpError: a rule that does
			// not compile under RE2 (e.g. Ruby-only syntax) never matches. A
			// blank pattern is skipped for the same reason as a blank
			// match_from: it would compile into a match-everything rule.
			pattern := row.Regex
			if strings.TrimSpace(pattern) == "" {
				s.Log.Warn("skip redirect rule with blank regex", "id", row.ID)
				continue
			}
			rule.hostPath = row.MatchOn == redirectMatchHostPath
			if row.MatchOn == redirectMatchHostLegacy {
				// Apply the migration's rewrite, then treat it as host+path.
				pattern = rewriteHostOnlyPattern(pattern)
				rule.hostPath = true
			}
			if rule.hostPath {
				// A pattern without any "/" can only intend the host itself;
				// against a host+path subject (which always carries at least
				// "/") it could never match. Give it the same whole-host
				// rewrite, so "^blog\.example\.com$" matches every path on the
				// host. Path-aware patterns contain a "/" and are used as-is,
				// so "^blog\.example\.com/?$" still pins the root.
				if !strings.Contains(pattern, "/") {
					pattern = rewriteHostOnlyPattern(pattern)
				}
				pattern = "^(?:" + pattern + ")$"
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				s.Log.Warn("skip uncompilable redirect rule", "id", row.ID, "error", err)
				continue
			}
			rule.re = re
			rule.replacement = rubyToGoExpansion(row.Replacement)
		}
		entry.rules = append(entry.rules, rule)
	}
	entry.fetchedAt = time.Now()
	s.Ext.Store(redirectCacheKey, entry)
	return entry.rules
}

// applyTo mirrors Redirect#apply_to. ok is false when the subject (path or
// host+path) does not match.
func (rule redirectRule) applyTo(subject string) (string, bool) {
	if rule.from != "" {
		return rule.applySimpleTo(subject)
	}
	// Regex mode, Ruby sub semantics: the matched span is replaced by the
	// expanded replacement, keeping any unmatched prefix/suffix (host+path
	// patterns are pre-anchored, so nothing unmatched survives there).
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

// applySimpleTo is the no-regex matcher. An exact rule ignores one trailing
// slash on either side (blog.example.com and blog.example.com/ are the same
// root). A prefix rule carries the remainder onto the target; a from without a
// trailing slash only continues on a path-segment boundary, so /blog never
// swallows /blogroll.
func (rule redirectRule) applySimpleTo(subject string) (string, bool) {
	from := rule.from
	if !rule.prefix {
		if trimOneTrailingSlash(subject) != trimOneTrailingSlash(from) {
			return "", false
		}
		return rule.to, true
	}
	var rest string
	if strings.HasSuffix(from, "/") {
		if !strings.HasPrefix(subject, from) {
			return "", false
		}
		rest = subject[len(from):]
	} else {
		switch {
		case subject == from:
		case strings.HasPrefix(subject, from+"/"):
			rest = subject[len(from):]
		default:
			return "", false
		}
	}
	return joinRedirectTarget(rule.to, rest), true
}

// trimOneTrailingSlash drops a single trailing "/", keeping a lone "/".
func trimOneTrailingSlash(s string) string {
	if len(s) > 1 {
		return strings.TrimSuffix(s, "/")
	}
	return s
}

// joinRedirectTarget appends a carried remainder to the target with exactly
// one slash between them.
func joinRedirectTarget(to, rest string) string {
	if rest == "" {
		return to
	}
	toSlash := strings.HasSuffix(to, "/")
	restSlash := strings.HasPrefix(rest, "/")
	switch {
	case toSlash && restSlash:
		return to + rest[1:]
	case !toSlash && !restSlash:
		return to + "/" + rest
	default:
		return to + rest
	}
}

// rewriteHostOnlyPattern turns a host-only pattern into host+path form, like
// migration 0008: wrapped in a non-capturing group (top-level alternations and
// capture-group numbering intact), one trailing '$' anchor dropped (a literal
// \$ is kept), and an optional path group appended.
func rewriteHostOnlyPattern(pattern string) string {
	if !strings.HasSuffix(pattern, `\$`) {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	return "(?:" + pattern + ")(?:/.*)?$"
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
