package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/paasd/paasd/internal/crypto"
	"github.com/paasd/paasd/internal/middleware"
)

const maxKeysPerTenant = 20

type CreateKeyRequest struct {
	Name      string `json:"name"`
	ExpiresIn *int64 `json:"expires_in,omitempty"`
}

type CreateKeyResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	APIKey  string `json:"api_key"`
	Prefix  string `json:"prefix"`
	Expires *int64 `json:"expires_at,omitempty"`
}

type KeyInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
	ExpiresAt  *int64 `json:"expires_at,omitempty"`
}

func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req CreateKeyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		req.Name = "unnamed"
	}

	if req.ExpiresIn != nil {
		if *req.ExpiresIn <= 0 {
			writeError(w, http.StatusBadRequest, "expires_in must be positive (seconds)")
			return
		}
		// Cap at 10 years to prevent effectively never-expiring keys
		const maxExpiresIn = 10 * 365 * 24 * 3600 // ~10 years in seconds
		if *req.ExpiresIn > maxExpiresIn {
			writeError(w, http.StatusBadRequest, "expires_in exceeds maximum (10 years)")
			return
		}
	}

	var keyCount int
	err := s.store.StateDB.QueryRow(
		`SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND revoked_at IS NULL`,
		tenantID,
	).Scan(&keyCount)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if keyCount >= maxKeysPerTenant {
		writeError(w, http.StatusForbidden, "maximum API keys reached, revoke unused keys first")
		return
	}

	apiKey, keyID, err := crypto.GenerateAPIKeyWithID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	keyHash := crypto.HashAPIKey(apiKey, s.masterKey)
	// Use keyID prefix as the display hint, not the secret's prefix
	prefix := keyID[:8]
	now := time.Now().Unix()

	var expiresAt *int64
	if req.ExpiresIn != nil {
		exp := now + *req.ExpiresIn
		expiresAt = &exp
	}

	_, err = s.store.StateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		keyID, tenantID, req.Name, prefix, keyHash, now, expiresAt,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create key")
		return
	}

	// Return token in format "keyID.secret" for O(1) lookup
	token := keyID + "." + apiKey

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(CreateKeyResponse{
		ID:      keyID,
		Name:    req.Name,
		APIKey:  token,
		Prefix:  prefix,
		Expires: expiresAt,
	})
}

func (s *Server) handleKeyList(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	rows, err := s.store.StateDB.Query(
		`SELECT id, name, key_prefix, created_at, last_used_at, expires_at
		 FROM api_keys
		 WHERE tenant_id = ? AND revoked_at IS NULL
		 ORDER BY created_at DESC
		 LIMIT 100`,
		tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	defer rows.Close()

	keys := []KeyInfo{}
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt); err != nil {
			continue
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}

	json.NewEncoder(w).Encode(keys)
}

func (s *Server) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	keyID := chi.URLParam(r, "keyID")

	result, err := s.store.StateDB.Exec(
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		time.Now().Unix(), keyID, tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke key")
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
