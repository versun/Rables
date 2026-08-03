package httpd

import (
	"net"
	"net/http"
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
	return &IPRateLimiter{
		r:          r,
		b:          b,
		lastSweep:  time.Now(),
		sweepEvery: time.Minute,
		maxIdle:    10 * time.Minute,
	}
}

// Allow reports whether the key may proceed right now.
func (l *IPRateLimiter) Allow(key string) bool {
	now := time.Now()
	v, _ := l.visitors.LoadOrStore(key, &visitor{limiter: rate.NewLimiter(l.r, l.b)})
	vis := v.(*visitor)
	vis.lastSeen.Store(now.UnixNano())
	l.sweep(now)
	return vis.limiter.Allow()
}

// sweep drops idle visitors at most once per sweepEvery.
func (l *IPRateLimiter) sweep(now time.Time) {
	l.mu.Lock()
	if now.Sub(l.lastSweep) < l.sweepEvery {
		l.mu.Unlock()
		return
	}
	l.lastSweep = now
	l.mu.Unlock()

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
