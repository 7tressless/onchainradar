package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Hermetic tests for the per-IP token-bucket rate limiter: burst then throttle,
// refill over time, per-IP isolation, the disabled (nil) pass-through, idle
// eviction, and the /api/* path scoping of the middleware. No DB, no real clock
// dependency beyond short sleeps for the refill case.

func TestRateLimiterBurstThenThrottle(t *testing.T) {
	// rate 5/s, burst 10: the first 10 requests pass, the 11th (same instant) is
	// throttled.
	rl := newRateLimiter(5, 10)
	allowed := 0
	for i := 0; i < 10; i++ {
		if rl.allow("1.2.3.4") {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("burst allowed = %d, want 10", allowed)
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("11th request within burst window should be throttled")
	}
}

func TestRateLimiterRefills(t *testing.T) {
	// rate 20/s, burst 20 (one token per 50ms). Drain the full burst, confirm the
	// next call is throttled, then after a 70ms wait a token (20 * 0.07 = 1.4) has
	// accrued and the next call passes. Draining the burst first sidesteps the
	// burst>=rate clamp while still exercising the time-based refill.
	rl := newRateLimiter(20, 20)
	for i := 0; i < 20; i++ {
		if !rl.allow("ip") {
			t.Fatalf("burst request %d should pass", i)
		}
	}
	if rl.allow("ip") {
		t.Fatal("request past the drained burst should be throttled")
	}
	time.Sleep(70 * time.Millisecond)
	if !rl.allow("ip") {
		t.Fatal("a token should have refilled after the wait")
	}
}

func TestRateLimiterPerIPIsolation(t *testing.T) {
	// burst 1: one IP exhausting its bucket must not affect another IP.
	rl := newRateLimiter(1, 1)
	if !rl.allow("a") {
		t.Fatal("ip a first request should pass")
	}
	if rl.allow("a") {
		t.Fatal("ip a second request should be throttled")
	}
	if !rl.allow("b") {
		t.Fatal("ip b must have its own independent bucket")
	}
}

func TestRateLimiterNilIsDisabled(t *testing.T) {
	var rl *rateLimiter // disabled
	for i := 0; i < 1000; i++ {
		if !rl.allow("x") {
			t.Fatal("nil limiter must always allow")
		}
	}
	// wrapAPI on a nil limiter must return the handler unchanged (pass-through).
	called := false
	h := rl.wrapAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if !called {
		t.Fatal("nil-limiter wrapAPI must invoke the inner handler")
	}
}

func TestRateLimiterEvictsIdle(t *testing.T) {
	rl := newRateLimiter(1, 1)
	rl.allow("stale")
	rl.allow("fresh")
	if got := len(rl.buckets); got != 2 {
		t.Fatalf("bucket count = %d, want 2", got)
	}
	// Backdate "stale" beyond the idle TTL, then run eviction at "now".
	rl.mu.Lock()
	rl.buckets["stale"].seen = time.Now().Add(-rateLimitIdleTTL - time.Minute)
	rl.mu.Unlock()

	rl.evictIdle(time.Now())

	rl.mu.Lock()
	_, staleExists := rl.buckets["stale"]
	_, freshExists := rl.buckets["fresh"]
	n := len(rl.buckets)
	rl.mu.Unlock()
	if staleExists {
		t.Fatal("stale bucket should have been evicted")
	}
	if !freshExists {
		t.Fatal("fresh bucket should have survived eviction")
	}
	if n != 1 {
		t.Fatalf("bucket count after eviction = %d, want 1", n)
	}
}

func TestRateLimiterMiddleware429AndScope(t *testing.T) {
	// burst 1, rate 1: the middleware throttles a second /api/ request from the same
	// RemoteAddr with 429, but never throttles a non-/api/ path.
	rl := newRateLimiter(1, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := rl.wrapAPI(inner)

	call := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "9.9.9.9:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call("/api/stats"); got != http.StatusOK {
		t.Fatalf("first /api request = %d, want 200", got)
	}
	if got := call("/api/stats"); got != http.StatusTooManyRequests {
		t.Fatalf("second /api request = %d, want 429", got)
	}
	// A non-/api path must bypass the limiter entirely, even after the bucket for
	// this IP is exhausted.
	if got := call("/"); got != http.StatusOK {
		t.Fatalf("non-/api request = %d, want 200 (limiter must not scope it)", got)
	}
}

func TestClientIPFromRemoteAddr(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4:5678":      "1.2.3.4",
		"[2001:db8::1]:443": "2001:db8::1",
		"no-port-here":      "no-port-here", // malformed: used verbatim
		"5.6.7.8":           "5.6.7.8",      // bare IP (no port): used verbatim
	}
	for remote, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		r.RemoteAddr = remote
		if got := clientIP(r); got != want {
			t.Errorf("clientIP(%q) = %q, want %q", remote, got, want)
		}
	}
}
