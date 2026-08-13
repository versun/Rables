package httpd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"rables/internal/config"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestRateLimitSixthRequest429(t *testing.T) {
	// Comment submission budget: 5 requests per 3 minutes per IP.
	limiter := NewIPRateLimiter(rate.Every(3*time.Minute/5), 5)
	h := RateLimit(limiter, ClientIP)(okHandler())

	for i := 1; i <= 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/comments", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/comments", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th request: status = %d, want 429", rec.Code)
	}
}

func TestRateLimitKeysAreIndependent(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Every(time.Hour), 1)
	h := RateLimit(limiter, ClientIP)(okHandler())

	for _, remoteAddr := range []string{"192.0.2.1:1234", "192.0.2.2:1234"} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s first request: status = %d, want 200", remoteAddr, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "192.0.2.1:9999" // same IP, new port: same bucket
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request from 192.0.2.1: status = %d, want 429", rec.Code)
	}
}

func TestRateLimitLazySweep(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Every(time.Hour), 1)
	limiter.Allow("stale")

	v, _ := limiter.visitors.Load("stale")
	v.(*visitor).lastSeen.Store(time.Now().Add(-2 * limiter.maxIdle).UnixNano())
	limiter.mu.Lock()
	limiter.lastSweep = time.Now().Add(-2 * limiter.sweepEvery)
	limiter.mu.Unlock()

	limiter.Allow("fresh") // triggers the sweep
	if _, ok := limiter.visitors.Load("stale"); ok {
		t.Error("idle visitor was not swept")
	}
	if _, ok := limiter.visitors.Load("fresh"); !ok {
		t.Error("active visitor was swept")
	}
}

// TestRateLimitFreshVisitorSurvivesSweep guards the LoadOrStore race in
// Allow: a visitor used to be published with a zero lastSeen and only then
// stamped, so a sweep triggered by another key in between read 1970, deleted
// the entry as idle, and the key's next request got a fresh full-burst
// limiter. lastSeen is now set before publication, so a just-created entry
// always survives a concurrent sweep and the second request stays rejected.
// (With the fix this can never fail; on the old code it failed whenever a
// sweep landed in the publish window.)
func TestRateLimitFreshVisitorSurvivesSweep(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Every(time.Hour), 1)
	limiter.sweepEvery = 0 // every Allow sweeps, maximizing overlap

	const workers = 8
	const keysPerWorker = 250
	errs := make(chan string, 2*workers*keysPerWorker)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < keysPerWorker; i++ {
				key := fmt.Sprintf("192.0.2.%d-%d", w, i)
				if !limiter.Allow(key) {
					errs <- key + ": first request rejected"
				}
				if limiter.Allow(key) {
					errs <- key + ": second request allowed (limiter state lost)"
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestRateLimitInFlightVisitorSurvivesSweep guards the second LoadOrStore
// race in Allow: for a key already in the map, a sweep triggered by another
// key can run between LoadOrStore and the lastSeen Store, read the stale
// lastSeen, and delete the very entry the request is about to spend a token
// on. That spend is lost and the next request gets a fresh full-burst
// limiter (~2x burst). Allow now re-checks the map after stamping lastSeen
// and retries once when its entry is gone, so the second request stays
// rejected. (With the fix this can never fail; on the old code it failed
// whenever a sweep landed in the stamp window.)
func TestRateLimitInFlightVisitorSurvivesSweep(t *testing.T) {
	limiter := NewIPRateLimiter(rate.Every(time.Hour), 1)
	limiter.sweepEvery = 0 // every Allow sweeps, maximizing overlap

	done := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := fmt.Sprintf("sweeper-%d", w)
			for {
				select {
				case <-done:
					return
				default:
					limiter.Allow(key) // triggers a sweep
				}
			}
		}(w)
	}

	const key = "hot"
	for round := 0; round < 2000; round++ {
		// Seed the key as long-idle so a concurrent sweep can delete the
		// entry while the first Allow below is in flight.
		vis := &visitor{limiter: rate.NewLimiter(limiter.r, limiter.b)}
		vis.lastSeen.Store(time.Now().Add(-2 * limiter.maxIdle).UnixNano())
		limiter.visitors.Store(key, vis)

		if !limiter.Allow(key) {
			t.Errorf("round %d: first request rejected", round)
		}
		if limiter.Allow(key) {
			t.Errorf("round %d: second request allowed (token spend lost to a sweep)", round)
		}
	}
	close(done)
	wg.Wait()
}

// A swept visitor comes back with a full burst budget, so the idle TTL must
// cover the time a whole burst takes to accrue — otherwise a client can
// burst, wait out the sweep, and burst again above the intended rate. Slow
// limiters get maxIdle = burst refill time; fast ones keep the 10m floor.
func TestRateLimitMaxIdleCoversBurstRefill(t *testing.T) {
	// The subscription budget: 5 requests per hour (rate.Every(12*time.Minute),
	// burst 5) refills a full burst in 1h, well past the 10m default.
	slow := NewIPRateLimiter(rate.Every(12*time.Minute), 5)
	if slow.maxIdle != time.Hour {
		t.Errorf("slow limiter maxIdle = %s, want 1h (full burst refill)", slow.maxIdle)
	}
	// The comment budget (5 per 3 minutes) refills in 3m: the floor stands.
	fast := NewIPRateLimiter(rate.Every(3*time.Minute/commentCreateBurst), commentCreateBurst)
	if fast.maxIdle != 10*time.Minute {
		t.Errorf("fast limiter maxIdle = %s, want the 10m floor", fast.maxIdle)
	}
}

func TestRateLimitKeyXForwardedFor(t *testing.T) {
	req := func(xff string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	trusted := &Server{Cfg: config.Config{TrustXForwardedFor: true}}
	untrusted := &Server{Cfg: config.Config{}}

	tests := []struct {
		name string
		s    *Server
		req  *http.Request
		want string
	}{
		{name: "untrusted ignores the header", s: untrusted, req: req("203.0.113.7"), want: "192.0.2.1"},
		{name: "trusted takes the rightmost hop", s: trusted, req: req("203.0.113.7, 10.0.0.1"), want: "10.0.0.1"},
		{name: "trusted trims whitespace", s: trusted, req: req("  203.0.113.7 , 10.0.0.1 "), want: "10.0.0.1"},
		{name: "trusted skips empty hops", s: trusted, req: req("203.0.113.7, , "), want: "203.0.113.7"},
		{name: "trusted without header falls back", s: trusted, req: req(""), want: "192.0.2.1"},
		{name: "trusted with blank header falls back", s: trusted, req: req("  "), want: "192.0.2.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.rateLimitKey(tt.req); got != tt.want {
				t.Errorf("rateLimitKey = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("trusted reads all header lines", func(t *testing.T) {
		// Some proxies add their hop as a separate X-Forwarded-For header line
		// instead of appending to the client's; a forged first line must not
		// win over the rightmost hop.
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Add("X-Forwarded-For", "203.0.113.7")
		r.Header.Add("X-Forwarded-For", "10.0.0.1")
		if got := trusted.rateLimitKey(r); got != "10.0.0.1" {
			t.Errorf("rateLimitKey = %q, want %q", got, "10.0.0.1")
		}
	})
}

func TestRateLimitXFFKeysAreIndependent(t *testing.T) {
	s := &Server{Cfg: config.Config{TrustXForwardedFor: true}}
	limiter := NewIPRateLimiter(rate.Every(time.Hour), 1)
	h := RateLimit(limiter, s.rateLimitKey)(okHandler())

	forwarded := func(xff string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Forwarded-For", xff)
		return req
	}
	for _, xff := range []string{"203.0.113.1", "203.0.113.2"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, forwarded(xff))
		if rec.Code != http.StatusOK {
			t.Errorf("%s first request: status = %d, want 200", xff, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, forwarded("203.0.113.1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request from 203.0.113.1: status = %d, want 429", rec.Code)
	}

	// The leftmost hops are client-supplied: rotating them must not open a
	// fresh bucket, only the proxy-appended rightmost hop keys the limiter.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, forwarded("198.51.100.9, 203.0.113.1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("forged leftmost hop from 203.0.113.1: status = %d, want 429", rec.Code)
	}
}
