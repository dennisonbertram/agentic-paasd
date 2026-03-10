package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/paasd/paasd/internal/crypto"
)

type contextKey string

const TenantIDKey contextKey = "tenant_id"

const (
	lastUsedInterval = 5 * time.Minute
	lastUsedMaxKeys  = 10000
	authCacheTTL     = 30 * time.Second
	authCacheMaxKeys = 5000
)

// authCacheEntry caches a validated key's DB result to reduce SQLite load.
// The HMAC verification still runs on every request (fast, in-memory).
type authCacheEntry struct {
	tenantID  string
	keyHash   string
	status    string
	expiresAt *int64 // key expiration time (nil = no expiry)
	cachedAt  time.Time
}

type authCache struct {
	mu      sync.RWMutex
	entries map[string]*authCacheEntry
}

func newAuthCache() *authCache {
	c := &authCache{
		entries: make(map[string]*authCacheEntry),
	}
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for range ticker.C {
			c.mu.Lock()
			now := time.Now()
			for k, v := range c.entries {
				if now.Sub(v.cachedAt) > authCacheTTL {
					delete(c.entries, k)
				}
			}
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *authCache) get(keyID string) (*authCacheEntry, bool) {
	c.mu.RLock()
	entry, exists := c.entries[keyID]
	c.mu.RUnlock()
	if !exists || time.Since(entry.cachedAt) > authCacheTTL {
		return nil, false
	}
	return entry, true
}

func (c *authCache) set(keyID string, entry *authCacheEntry) {
	c.mu.Lock()
	// Evict oldest if at capacity
	if len(c.entries) >= authCacheMaxKeys {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range c.entries {
			if oldestKey == "" || v.cachedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.cachedAt
			}
		}
		if oldestKey != "" {
			delete(c.entries, oldestKey)
		}
	}
	c.entries[keyID] = entry
	c.mu.Unlock()
}

// lastUsedTracker samples last_used_at updates with bounded map.
type lastUsedTracker struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	db       *sql.DB
}

func newLastUsedTracker(db *sql.DB) *lastUsedTracker {
	t := &lastUsedTracker{
		lastSeen: make(map[string]time.Time),
		db:       db,
	}
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		for range ticker.C {
			t.mu.Lock()
			cutoff := time.Now().Add(-24 * time.Hour)
			for k, v := range t.lastSeen {
				if v.Before(cutoff) {
					delete(t.lastSeen, k)
				}
			}
			t.mu.Unlock()
		}
	}()
	return t
}

func (t *lastUsedTracker) maybeUpdate(keyID string) {
	t.mu.Lock()
	last, exists := t.lastSeen[keyID]
	now := time.Now()
	if exists && now.Sub(last) < lastUsedInterval {
		t.mu.Unlock()
		return
	}
	if !exists && len(t.lastSeen) >= lastUsedMaxKeys {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range t.lastSeen {
			if oldestKey == "" || v.Before(oldestTime) {
				oldestKey = k
				oldestTime = v
			}
		}
		if oldestKey != "" {
			delete(t.lastSeen, oldestKey)
		}
	}
	t.lastSeen[keyID] = now
	t.mu.Unlock()

	t.db.Exec("UPDATE api_keys SET last_used_at = ? WHERE id = ?", now.Unix(), keyID)
}

func Auth(db *sql.DB, masterKey []byte) func(http.Handler) http.Handler {
	tracker := newLastUsedTracker(db)
	cache := newAuthCache()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				writeJSONError(w, http.StatusUnauthorized, "missing authorization header")
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
				writeJSONError(w, http.StatusUnauthorized, "invalid authorization format, expected: Bearer <token>")
				return
			}
			token := parts[1]

			// Token format: "keyID.secret" for O(1) lookup
			dotIdx := strings.IndexByte(token, '.')
			if dotIdx < 1 || dotIdx >= len(token)-1 {
				writeJSONError(w, http.StatusUnauthorized, "invalid api key")
				return
			}
			keyID := token[:dotIdx]
			secret := token[dotIdx+1:]

			if len(keyID) > 64 || len(secret) > 256 {
				writeJSONError(w, http.StatusUnauthorized, "invalid api key")
				return
			}

			var tenantID, keyHash, status string

			// Check auth cache first to reduce DB load
			if cached, ok := cache.get(keyID); ok {
				// Verify expiry locally even on cache hit
				if cached.expiresAt != nil && time.Now().Unix() > *cached.expiresAt {
					writeJSONError(w, http.StatusUnauthorized, "invalid api key")
					return
				}
				tenantID = cached.tenantID
				keyHash = cached.keyHash
				status = cached.status
			} else {
				now := time.Now().Unix()
				var expiresAt *int64
				err := db.QueryRowContext(r.Context(),
					`SELECT ak.tenant_id, ak.key_hash, t.status, ak.expires_at
					 FROM api_keys ak
					 JOIN tenants t ON t.id = ak.tenant_id
					 WHERE ak.id = ?
					   AND ak.revoked_at IS NULL
					   AND (ak.expires_at IS NULL OR ak.expires_at > ?)`,
					keyID, now,
				).Scan(&tenantID, &keyHash, &status, &expiresAt)
				if err != nil {
					writeJSONError(w, http.StatusUnauthorized, "invalid api key")
					return
				}

				cache.set(keyID, &authCacheEntry{
					tenantID:  tenantID,
					keyHash:   keyHash,
					status:    status,
					expiresAt: expiresAt,
					cachedAt:  time.Now(),
				})
			}

			if status != "active" {
				writeJSONError(w, http.StatusUnauthorized, "invalid api key")
				return
			}

			if !crypto.VerifyAPIKey(keyHash, secret, masterKey) {
				writeJSONError(w, http.StatusUnauthorized, "invalid api key")
				return
			}

			tracker.maybeUpdate(keyID)

			ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func GetTenantID(ctx context.Context) string {
	v, _ := ctx.Value(TenantIDKey).(string)
	return v
}
