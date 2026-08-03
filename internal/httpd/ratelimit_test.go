package httpd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
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
	v.(*visitor).lastSeen.Store(time.Now().Add(-time.Hour).UnixNano())
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
