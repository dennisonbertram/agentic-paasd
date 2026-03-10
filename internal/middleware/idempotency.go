package middleware

import (
	"net/http"
	"sync"
	"time"
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
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}

		tenantID := GetTenantID(r.Context())
		fullKey := tenantID + ":" + key

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

		s.mu.Lock()
		s.entries[fullKey] = &idempotencyEntry{
			statusCode: rec.statusCode,
			body:       rec.body,
			expiresAt:  time.Now().Add(24 * time.Hour),
		}
		s.mu.Unlock()
	})
}
