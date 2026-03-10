package api

import (
	"crypto/rand"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/paasd/paasd/internal/crypto"
	"github.com/paasd/paasd/internal/middleware"
)

type RegisterRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type RegisterResponse struct {
	TenantID string `json:"tenant_id"`
	APIKey   string `json:"api_key"`
}

type TenantResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type UpdateTenantRequest struct {
	Name *string `json:"name,omitempty"`
}

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

type registrationLimiter struct {
	mu             sync.Mutex
	entries        map[string]*regEntry
	globalCount    int
	globalWindowAt time.Time
}

type regEntry struct {
	count    int
	windowAt time.Time
}

var regLimiter = &registrationLimiter{
	entries: make(map[string]*regEntry),
}

const (
	regMaxPerIPPerHour = 10
	regGlobalPerHour   = 100
	regMaxEntries      = 10000
	regWindow          = 1 * time.Hour
	maxTenants         = 1000
)

func init() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for range ticker.C {
			regLimiter.mu.Lock()
			now := time.Now()
			for k, v := range regLimiter.entries {
				if now.After(v.windowAt) {
					delete(regLimiter.entries, k)
				}
			}
			regLimiter.mu.Unlock()
		}
	}()
}

func (rl *registrationLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	if now.After(rl.globalWindowAt) {
		rl.globalCount = 0
		rl.globalWindowAt = now.Add(regWindow)
	}
	if rl.globalCount >= regGlobalPerHour {
		return false
	}

	if len(rl.entries) >= regMaxEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range rl.entries {
			if oldestKey == "" || v.windowAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.windowAt
			}
		}
		if oldestKey != "" {
			delete(rl.entries, oldestKey)
		}
	}

	entry, exists := rl.entries[ip]
	if !exists || now.After(entry.windowAt) {
		rl.entries[ip] = &regEntry{count: 1, windowAt: now.Add(regWindow)}
		rl.globalCount++
		return true
	}
	if entry.count >= regMaxPerIPPerHour {
		return false
	}
	entry.count++
	rl.globalCount++
	return true
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) handleTenantRegister(w http.ResponseWriter, r *http.Request) {
	// Rate limit ALL registration attempts (including invalid tokens) to prevent
	// brute-force of the bootstrap token. Uses trusted real IP from proxy headers.
	ip := trustedRealIP(r)
	if !regLimiter.allow(ip) {
		w.Header().Set("Retry-After", "3600")
		http.Error(w, `{"error":"rate limit exceeded, try again later"}`, http.StatusTooManyRequests)
		return
	}

	// Bootstrap token gate — open registration (--dev --open-registration) is
	// the only path that skips this. Without that explicit flag, bootstrapToken
	// is always required (main.go fatals if unset outside dev+open-registration).
	if !s.openRegistration {
		provided := r.Header.Get("X-Bootstrap-Token")
		// HMAC-compare to prevent length-leak from ConstantTimeCompare
		if !hmacEqual(provided, s.bootstrapToken) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
	}

	// Enforce max tenant count to prevent DB/disk exhaustion
	var tenantCount int
	if err := s.store.StateDB.QueryRow(`SELECT COUNT(*) FROM tenants`).Scan(&tenantCount); err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if tenantCount >= maxTenants {
		http.Error(w, `{"error":"maximum tenants reached"}`, http.StatusForbidden)
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if len(req.Name) < 2 {
		http.Error(w, `{"error":"name must be at least 2 characters"}`, http.StatusBadRequest)
		return
	}
	if len(req.Name) > 128 {
		http.Error(w, `{"error":"name must be at most 128 characters"}`, http.StatusBadRequest)
		return
	}

	if !emailRegex.MatchString(req.Email) {
		http.Error(w, `{"error":"invalid email format"}`, http.StatusBadRequest)
		return
	}
	if len(req.Email) > 256 {
		http.Error(w, `{"error":"email too long"}`, http.StatusBadRequest)
		return
	}

	tenantID, err := generateID()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	now := time.Now().Unix()

	tx, err := s.store.StateDB.Begin()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', ?, ?)`,
		tenantID, req.Name, req.Email, now, now,
	)
	if err != nil {
		http.Error(w, `{"error":"registration failed"}`, http.StatusConflict)
		return
	}

	_, err = tx.Exec(
		`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`,
		tenantID,
	)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	apiKey, keyID, err := crypto.GenerateAPIKeyWithID()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	keyHash := crypto.HashAPIKey(apiKey, s.masterKey)
	// Use keyID prefix as display hint, not the secret's prefix
	prefix := keyID[:8]

	_, err = tx.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
		 VALUES (?, ?, 'default', ?, ?, ?)`,
		keyID, tenantID, prefix, keyHash, now,
	)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	// Return token in format "keyID.secret" for O(1) lookup
	token := keyID + "." + apiKey

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(RegisterResponse{
		TenantID: tenantID,
		APIKey:   token,
	})
}

func (s *Server) handleTenantGet(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var t TenantResponse
	err := s.store.StateDB.QueryRow(
		`SELECT id, name, email, status, created_at, updated_at FROM tenants WHERE id = ?`,
		tenantID,
	).Scan(&t.ID, &t.Name, &t.Email, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		http.Error(w, `{"error":"tenant not found"}`, http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(t)
}

func (s *Server) handleTenantUpdate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req UpdateTenantRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Name != nil {
		if len(*req.Name) < 2 || len(*req.Name) > 128 {
			http.Error(w, `{"error":"name must be 2-128 characters"}`, http.StatusBadRequest)
			return
		}
		_, err := s.store.StateDB.Exec(
			`UPDATE tenants SET name = ?, updated_at = ? WHERE id = ?`,
			*req.Name, time.Now().Unix(), tenantID,
		)
		if err != nil {
			http.Error(w, `{"error":"failed to update tenant"}`, http.StatusInternalServerError)
			return
		}
	}

	s.handleTenantGet(w, r)
}

func (s *Server) handleTenantDelete(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	now := time.Now().Unix()

	tx, err := s.store.StateDB.Begin()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`UPDATE tenants SET status = 'suspended', updated_at = ? WHERE id = ?`,
		now, tenantID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to delete tenant"}`, http.StatusInternalServerError)
		return
	}

	// Revoke all API keys for the suspended tenant
	_, err = tx.Exec(
		`UPDATE api_keys SET revoked_at = ? WHERE tenant_id = ? AND revoked_at IS NULL`,
		now, tenantID,
	)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// hmacEqual compares two strings in constant time regardless of length.
// Unlike subtle.ConstantTimeCompare, this does not leak the length of either input.
func hmacEqual(a, b string) bool {
	key := []byte("bootstrap-token-compare")
	macA := hmac.New(sha256.New, key)
	macA.Write([]byte(a))
	macB := hmac.New(sha256.New, key)
	macB.Write([]byte(b))
	return hmac.Equal(macA.Sum(nil), macB.Sum(nil))
}
