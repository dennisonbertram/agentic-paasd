package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/paasd/paasd/internal/crypto"
	"github.com/paasd/paasd/internal/middleware"
)

type CreateKeyRequest struct {
	Name      string `json:"name"`
	ExpiresIn *int64 `json:"expires_in,omitempty"` // seconds
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		req.Name = "unnamed"
	}

	apiKey, err := crypto.GenerateAPIKey()
	if err != nil {
		http.Error(w, `{"error":"failed to generate key"}`, http.StatusInternalServerError)
		return
	}

	keyHash, err := crypto.HashPassword(apiKey)
	if err != nil {
		http.Error(w, `{"error":"failed to hash key"}`, http.StatusInternalServerError)
		return
	}

	keyID := generateID()
	prefix := apiKey[:8]
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
		http.Error(w, `{"error":"failed to create key"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(CreateKeyResponse{
		ID:      keyID,
		Name:    req.Name,
		APIKey:  apiKey,
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
		 ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to list keys"}`, http.StatusInternalServerError)
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
		http.Error(w, `{"error":"failed to revoke key"}`, http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, `{"error":"key not found"}`, http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
