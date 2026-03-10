package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleTenantRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.Email == "" {
		http.Error(w, `{"error":"name and email are required"}`, http.StatusBadRequest)
		return
	}

	tenantID := generateID()
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
		http.Error(w, fmt.Sprintf(`{"error":"failed to create tenant: %s"}`, err.Error()), http.StatusConflict)
		return
	}

	// Create default quotas
	_, err = tx.Exec(
		`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`,
		tenantID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to create quotas"}`, http.StatusInternalServerError)
		return
	}

	// Generate API key
	apiKey, err := crypto.GenerateAPIKey()
	if err != nil {
		http.Error(w, `{"error":"failed to generate api key"}`, http.StatusInternalServerError)
		return
	}

	keyHash, err := crypto.HashPassword(apiKey)
	if err != nil {
		http.Error(w, `{"error":"failed to hash api key"}`, http.StatusInternalServerError)
		return
	}

	keyID := generateID()
	prefix := apiKey[:8]

	_, err = tx.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
		 VALUES (?, ?, 'default', ?, ?, ?)`,
		keyID, tenantID, prefix, keyHash, now,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to create api key"}`, http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"failed to commit"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(RegisterResponse{
		TenantID: tenantID,
		APIKey:   apiKey,
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Name != nil {
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

	_, err := s.store.StateDB.Exec(
		`UPDATE tenants SET status = 'suspended', updated_at = ? WHERE id = ?`,
		time.Now().Unix(), tenantID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to delete tenant"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
