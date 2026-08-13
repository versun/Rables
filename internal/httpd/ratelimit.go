package httpd

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// IPRateLimiter keeps one token bucket per key (typically a client IP),
// replacing Rails' rate_limit (plan section 1). Entries idle longer than
// maxIdle are swept lazily so the map does not grow unboundedly.
type IPRateLimiter struct {
	r rate.Limit
	b int

	visitors sync.Map // key string -> *visitor

	mu        sync.Mutex
	lastSweep time.Time

	sweepEvery time.Duration
	maxIdle    time.Duration
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // unix nano
}

// NewIPRateLimiter allows b requests in a burst, refilling at r.
func NewIPRateLimiter(r rate.Limit, b int) *IPRateLimiter {
	// A swept entry comes back with a full burst budget, so the idle TTL
	// must cover the time a whole burst takes to accrue; otherwise a client
	// can burst, wait out the sweep, and burst again at a higher effective
	// rate than the limit intends (e.g. the 5-per-hour subscription limit
	// would allow 5 requests every 10 minutes).
	maxIdle := 10 * time.Minute
	// rate.Limit is tokens per second, so the burst refill time is b/r
	// seconds: time.Second * b / r.
	if refill := time.Duration(float64(time.Second) * float64(b) / float64(r)); refill > maxIdle {
		maxIdle = refill
	}
	return &IPRateLimiter{
		r:          r,
		b:          b,
		lastSweep:  time.Now(),
		sweepEvery: time.Minute,
		maxIdle:    maxIdle,
	}
}

// Allow reports whether the key may proceed right now.
func (l *IPRateLimiter) Allow(key string) bool {
	now := time.Now()
	// lastSeen must be set before the visitor is published: a sweep triggered
	// by another key between LoadOrStore and a later Store would read the zero
	// value (1970), delete the just-created entry as idle, and hand this key a
	// fresh full-burst limiter on its next request.
	nv := &visitor{limiter: rate.NewLimiter(l.r, l.b)}
	nv.lastSeen.Store(now.UnixNano())
	v, _ := l.visitors.LoadOrStore(key, nv)
	vis := v.(*visitor)
	// Stamping lastSeen and confirming the entry is still in the map must be
	// atomic against a sweep's read-and-delete, so both sides take l.mu: a
	// sweep either ran before us (our entry is gone — retry once, the retry
	// re-publishes nv or joins the newer entry, both with a fresh lastSeen no
	// sweep can condemn) or runs after us (it reads the fresh lastSeen and
	// leaves the entry alone). Without this a sweep could read the pre-Store
	// lastSeen, delete the entry we are about to use, and lose the token
	// spend — the next request would get a fresh full-burst limiter.
	l.mu.Lock()
	vis.lastSeen.Store(now.UnixNano())
	cur, ok := l.visitors.Load(key)
	l.mu.Unlock()
	if !ok || cur != vis {
		return l.Allow(key)
	}
	l.sweep(now)
	return vis.limiter.Allow()
}

// sweep drops idle visitors at most once per sweepEvery. It holds l.mu for
// the whole pass so that a concurrent Allow cannot refresh an entry's
// lastSeen between the staleness read and the Delete.
func (l *IPRateLimiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) < l.sweepEvery {
		return
	}
	l.lastSweep = now

	cutoff := now.Add(-l.maxIdle).UnixNano()
	l.visitors.Range(func(k, v any) bool {
		if v.(*visitor).lastSeen.Load() < cutoff {
			l.visitors.Delete(k)
		}
		return true
	})
}

// RateLimit returns middleware that rejects over-limit requests with 429.
// keyFunc extracts the bucket key; nil defaults to ClientIP.
func RateLimit(l *IPRateLimiter, keyFunc func(*http.Request) string) func(http.Handler) http.Handler {
	if keyFunc == nil {
		keyFunc = ClientIP
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(keyFunc(r)) {
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP is the default rate-limit key: the request's remote IP.
func ClientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// rateLimitKey is the Server-scoped bucket key for rate limits. When the
// deployment sits behind a reverse proxy (Cfg.TrustXForwardedFor) every
// request shares the proxy's RemoteAddr, so an X-Forwarded-For hop keys the
// bucket instead. Proxies append the peer they see to any client-supplied
// header, so the rightmost non-empty hop is the one the trusted proxy vouches
// for; the leftmost hops come from the client and are trivially forgeable,
// which would hand an attacker a fresh bucket per request. Some proxies add
// their hop as a separate header line instead of appending (Header.Get reads
// only the first line), so all lines are joined before splitting. Without
// the header (or the trust flag) it falls back to the remote IP.
func (s *Server) rateLimitKey(r *http.Request) string {
	if s.Cfg.TrustXForwardedFor {
		hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			if ip := strings.TrimSpace(hops[i]); ip != "" {
				return ip
			}
		}
	}
	return ClientIP(r)
}
