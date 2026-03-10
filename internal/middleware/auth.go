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
)

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

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
	
				http.Error(w, `{"error":"missing authorization header"}`, http.StatusUnauthorized)
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
	
				http.Error(w, `{"error":"invalid authorization format"}`, http.StatusUnauthorized)
				return
			}
			token := parts[1]

			// Token format: "keyID.secret" for O(1) lookup
			dotIdx := strings.IndexByte(token, '.')
			if dotIdx < 1 || dotIdx >= len(token)-1 {
	
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}
			keyID := token[:dotIdx]
			secret := token[dotIdx+1:]

			if len(keyID) > 64 || len(secret) > 256 {
	
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}

			now := time.Now().Unix()
			var tenantID, keyHash, status string
			err := db.QueryRowContext(r.Context(),
				`SELECT ak.tenant_id, ak.key_hash, t.status
				 FROM api_keys ak
				 JOIN tenants t ON t.id = ak.tenant_id
				 WHERE ak.id = ?
				   AND ak.revoked_at IS NULL
				   AND (ak.expires_at IS NULL OR ak.expires_at > ?)`,
				keyID, now,
			).Scan(&tenantID, &keyHash, &status)
			if err != nil {
	
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}

			if status != "active" {
	
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}

			if !crypto.VerifyAPIKey(keyHash, secret, masterKey) {
	
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
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

