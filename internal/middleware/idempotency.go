package middleware

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxIdempotencyEntries       = 500
	maxIdempotencyPerTenant     = 50
	maxIdempotencyKeyLen        = 128
	maxIdempotencyBodyLen       = 8 * 1024 // 8KB max stored response
	idempotencyTTL              = 10 * time.Minute
)

type idempotencyEntry struct {
	statusCode int
	body       []byte
	expiresAt  time.Time
}

type IdempotencyStore struct {
	mu      sync.RWMutex
	entries map[string]*idempotencyEntry
}

func NewIdempotencyStore() *IdempotencyStore {
	s := &IdempotencyStore{
		entries: make(map[string]*idempotencyEntry),
	}
	go s.cleanup()
	return s
}

func (s *IdempotencyStore) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for k, v := range s.entries {
			if now.After(v.expiresAt) {
				delete(s.entries, k)
			}
		}
		s.mu.Unlock()
	}
}

type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       []byte
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.statusCode = code
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	rr.body = append(rr.body, b...)
	return rr.ResponseWriter.Write(b)
}

func (s *IdempotencyStore) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only apply idempotency to POST and PUT
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Never cache auth key creation responses (contain secrets)
		if strings.HasPrefix(r.URL.Path, "/v1/auth/keys") {
			next.ServeHTTP(w, r)
			return
		}

		// Validate key length
		if len(key) > maxIdempotencyKeyLen {
			http.Error(w, `{"error":"idempotency key too long"}`, http.StatusBadRequest)
			return
		}

		// Scope by tenant + method + path + query string.
		// Fail closed: skip caching if tenant ID is missing to prevent cross-tenant collisions.
		tenantID := GetTenantID(r.Context())
		if tenantID == "" {
			next.ServeHTTP(w, r)
			return
		}
		fullKey := tenantID + ":" + r.Method + ":" + r.URL.RequestURI() + ":" + key

		s.mu.RLock()
		entry, exists := s.entries[fullKey]
		s.mu.RUnlock()

		if exists && time.Now().Before(entry.expiresAt) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(entry.statusCode)
			w.Write(entry.body)
			return
		}

		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Only cache successful (2xx) responses within size bounds.
		// Error responses are not cached to prevent "sticky failure" attacks.
		if rec.statusCode >= 200 && rec.statusCode < 300 && len(rec.body) <= maxIdempotencyBodyLen {
			s.mu.Lock()
			// Enforce per-tenant limit to prevent single-tenant memory abuse
			tenantPrefix := tenantID + ":"
			tenantEntryCount := 0
			for k := range s.entries {
				if len(k) > len(tenantPrefix) && k[:len(tenantPrefix)] == tenantPrefix {
					tenantEntryCount++
				}
			}
			if tenantEntryCount < maxIdempotencyPerTenant {
				// Enforce global max entries - evict oldest if at capacity
				if len(s.entries) >= maxIdempotencyEntries {
					var oldestKey string
					var oldestTime time.Time
					for k, v := range s.entries {
						if oldestKey == "" || v.expiresAt.Before(oldestTime) {
							oldestKey = k
							oldestTime = v.expiresAt
						}
					}
					if oldestKey != "" {
						delete(s.entries, oldestKey)
					}
				}
				s.entries[fullKey] = &idempotencyEntry{
					statusCode: rec.statusCode,
					body:       rec.body,
					expiresAt:  time.Now().Add(idempotencyTTL),
				}
			}
			s.mu.Unlock()
		}
	})
}
