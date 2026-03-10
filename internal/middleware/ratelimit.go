package middleware

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const maxRateLimitEntries = 10000

type rateLimitEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	entries map[string]*rateLimitEntry
	rate    rate.Limit
	burst   int
}

func NewRateLimiter(rps float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		entries: make(map[string]*rateLimitEntry),
		rate:    rate.Limit(rps),
		burst:   burst,
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-1 * time.Hour)
		for k, v := range rl.entries {
			if v.lastSeen.Before(cutoff) {
				delete(rl.entries, k)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) getLimiter(key string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, exists := rl.entries[key]
	if exists {
		entry.lastSeen = time.Now()
		return entry.limiter
	}

	// Evict oldest if at capacity
	if len(rl.entries) >= maxRateLimitEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range rl.entries {
			if oldestKey == "" || v.lastSeen.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.lastSeen
			}
		}
		if oldestKey != "" {
			delete(rl.entries, oldestKey)
		}
	}

	limiter := rate.NewLimiter(rl.rate, rl.burst)
	rl.entries[key] = &rateLimitEntry{
		limiter:  limiter,
		lastSeen: time.Now(),
	}
	return limiter
}

func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := GetTenantID(r.Context())
		if tenantID == "" {
			// Fail closed: reject if tenant ID is missing (should never happen
			// in authenticated routes, but prevents bypass if context is lost)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}

		limiter := rl.getLimiter(tenantID)
		if !limiter.Allow() {
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// GlobalRateLimiter enforces a single rate limit across all tenants to prevent
// multi-tenant aggregate abuse (e.g., attacker creating many tenants to multiply throughput).
type GlobalRateLimiter struct {
	limiter *rate.Limiter
}

func NewGlobalRateLimiter(rps float64, burst int) *GlobalRateLimiter {
	return &GlobalRateLimiter{
		limiter: rate.NewLimiter(rate.Limit(rps), burst),
	}
}

func (gl *GlobalRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gl.limiter.Allow() {
			writeJSONError(w, http.StatusTooManyRequests, "global rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}
