package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxIdempotencyEntries   = 500
	maxIdempotencyPerTenant = 50
	maxIdempotencyKeyLen    = 128
	maxIdempotencyBodyLen   = 8 * 1024 // 8KB max stored response
	idempotencyTTL          = 10 * time.Minute
)

type idempotencyEntry struct {
	statusCode  int
	contentType string // preserved from original response
	body        []byte
	bodyHash    string // SHA-256 hex of request body for mismatch detection
	expiresAt   time.Time
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
	statusCode    int
	wroteHeader   bool
	body          []byte
	overflow      bool // true if body exceeded maxIdempotencyBodyLen
}

func (rr *responseRecorder) WriteHeader(code int) {
	if !rr.wroteHeader {
		rr.statusCode = code
		rr.wroteHeader = true
	}
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	// Implicit 200 if WriteHeader was never called (per net/http spec)
	if !rr.wroteHeader {
		rr.statusCode = http.StatusOK
		rr.wroteHeader = true
	}
	// Cap in-memory buffering to prevent memory exhaustion from large responses.
	// Once overflow, stop buffering but continue writing to the client.
	if !rr.overflow {
		if len(rr.body)+len(b) <= maxIdempotencyBodyLen {
			rr.body = append(rr.body, b...)
		} else {
			rr.overflow = true
			rr.body = nil // release buffered bytes
		}
	}
	return rr.ResponseWriter.Write(b)
}

// hashRequestBody reads the request body, computes SHA-256, and replaces
// r.Body with a new reader so downstream handlers can still read it.
func hashRequestBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "empty", nil
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	r.Body.Close()
	// Replace body so downstream handlers can read it
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	h := sha256.Sum256(bodyBytes)
	return hex.EncodeToString(h[:]), nil
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
			writeJSONError(w, http.StatusBadRequest, "idempotency key too long")
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

		// Hash request body for payload mismatch detection.
		// Body is already capped by maxBodySize middleware (1MB).
		reqBodyHash, err := hashRequestBody(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "failed to read request body")
			return
		}

		s.mu.RLock()
		entry, exists := s.entries[fullKey]
		s.mu.RUnlock()

		if exists && time.Now().Before(entry.expiresAt) {
			// Detect payload mismatch: same idempotency key but different body
			if entry.bodyHash != reqBodyHash {
				writeJSONError(w, http.StatusConflict, "idempotency key reused with different request body")
				return
			}
			w.Header().Set("Idempotency-Replayed", "true")
			// For responses with body, set content headers
			if len(entry.body) > 0 {
				if entry.contentType != "" {
					w.Header().Set("Content-Type", entry.contentType)
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.Header().Set("X-Content-Type-Options", "nosniff")
			}
			w.WriteHeader(entry.statusCode)
			if len(entry.body) > 0 {
				w.Write(entry.body)
			}
			return
		}

		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		// Default to 200 if handler never called WriteHeader (per net/http spec)
		if !rec.wroteHeader {
			rec.statusCode = http.StatusOK
		}

		// Only cache successful (2xx) responses within size bounds.
		// Error responses are not cached to prevent "sticky failure" attacks.
		if rec.statusCode >= 200 && rec.statusCode < 300 && !rec.overflow && len(rec.body) <= maxIdempotencyBodyLen {
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
					statusCode:  rec.statusCode,
					contentType: rec.Header().Get("Content-Type"),
					body:        rec.body,
					bodyHash:    reqBodyHash,
					expiresAt:   time.Now().Add(idempotencyTTL),
				}
			}
			s.mu.Unlock()
		}
	})
}
