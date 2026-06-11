package api

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// This file holds the per-IP rate limiter for the public API: a small in-process token
// bucket (no external dependency), one bucket per client IP, lazily refilled on access,
// with a periodic janitor evicting idle IPs so the map stays bounded. Deliberately
// simple: a coarse request cap for a read-only API, not a precise quota system.

const (
	// rateLimitJanitorInterval is how often the janitor sweeps the bucket map for idle
	// IPs: frequent enough to keep the map small under churn, infrequent enough to be cheap.
	rateLimitJanitorInterval = time.Minute

	// rateLimitIdleTTL is how long a bucket may go untouched before eviction. A returning
	// client just gets a fresh full bucket, so eviction is safe; the TTL only bounds
	// memory for one-shot clients.
	rateLimitIdleTTL = 10 * time.Minute
)

// bucket is one client's token bucket: tokens is the current allowance (fractional so
// sub-second refills accrue), last is the last refill, seen the last touch (for idle
// eviction). All access is under the parent rateLimiter's mutex, so it needs no own lock.
type bucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

// rateLimiter is a per-IP token-bucket limiter: rate is the steady tokens-per-second
// refill, burst the bucket capacity. A nil *rateLimiter is a valid, disabled limiter
// whose methods are pass-throughs, so the caller never has to nil-check.
type rateLimiter struct {
	rate  float64 // tokens per second
	burst float64 // bucket capacity

	mu      sync.Mutex
	buckets map[string]*bucket
}

// newRateLimiter builds a limiter allowing rps requests/second per IP with the given
// burst. Returns nil (a disabled pass-through) when rps <= 0. A burst below the rate is
// clamped up to it, so a single request is never rejected on an otherwise-idle bucket.
func newRateLimiter(rps, burst int) *rateLimiter {
	if rps <= 0 {
		return nil
	}
	b := float64(burst)
	if b < float64(rps) {
		b = float64(rps)
	}
	return &rateLimiter{
		rate:    float64(rps),
		burst:   b,
		buckets: make(map[string]*bucket),
	}
}

// allow reports whether a request from ip may proceed, consuming one token when it can.
// It lazily refills from the time since last access (no per-bucket goroutine), caps at
// burst, and updates the idle-eviction timestamp. A nil limiter always allows.
func (rl *rateLimiter) allow(ip string) bool {
	if rl == nil {
		return true
	}
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[ip]
	if !ok {
		// First request from this IP: a full bucket, minus the token it spends now.
		rl.buckets[ip] = &bucket{tokens: rl.burst - 1, last: now, seen: now}
		return true
	}

	// Refill by elapsed time, capped at burst.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rl.rate
		if b.tokens > rl.burst {
			b.tokens = rl.burst
		}
		b.last = now
	}
	b.seen = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// runJanitor periodically evicts buckets untouched for longer than rateLimitIdleTTL so
// the map stays bounded under churn, blocking until ctx is cancelled. A nil limiter
// returns immediately.
func (rl *rateLimiter) runJanitor(ctx context.Context) {
	if rl == nil {
		return
	}
	ticker := time.NewTicker(rateLimitJanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rl.evictIdle(time.Now())
		}
	}
}

// evictIdle removes every bucket last seen before now-rateLimitIdleTTL. Split out
// from runJanitor so it is unit-testable without a real clock/ticker.
func (rl *rateLimiter) evictIdle(now time.Time) {
	cutoff := now.Add(-rateLimitIdleTTL)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for ip, b := range rl.buckets {
		if b.seen.Before(cutoff) {
			delete(rl.buckets, ip)
		}
	}
}

// wrapAPI rate-limits /api/* per client IP; other paths pass straight through. A nil
// limiter returns next unchanged (zero overhead). On exceed it returns 429 with a small
// JSON body (CORS headers are already set by the outer withCORS wrapper).
func (rl *rateLimiter) wrapAPI(next http.Handler) http.Handler {
	if rl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if !rl.allow(clientIP(r)) {
			// Retry-After is advisory; one second matches the per-second refill.
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, ErrorDTO{Error: "rate limit exceeded"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the client IP from RemoteAddr (host:port). It deliberately ignores
// X-Forwarded-For: a client-supplied header could be set to any value, letting a caller
// sidestep the per-IP limit. A portless/malformed RemoteAddr is used verbatim so the
// limiter still keys on something stable.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// logConfig records the effective limiter settings (or that it is disabled) once at
// startup, kept here so the server file does not reach into the unexported fields.
func (rl *rateLimiter) logConfig() {
	if rl == nil {
		log.Info().Msg("api: per-IP rate limiting disabled")
		return
	}
	log.Info().Float64("rps", rl.rate).Float64("burst", rl.burst).Msg("api: per-IP rate limiting enabled")
}
