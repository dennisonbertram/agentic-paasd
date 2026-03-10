package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/paasd/paasd/internal/databases"
	"github.com/paasd/paasd/internal/middleware"
)

// requireDBManager is a guard that returns 503 if dbManager is nil.
func (s *Server) requireDBManager(w http.ResponseWriter) bool {
	if s.dbManager == nil {
		writeError(w, http.StatusServiceUnavailable, "database management is not available")
		return false
	}
	return true
}

func (s *Server) handleDatabaseCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	var req databases.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := s.dbManager.Create(ctx, tenantID, req)
	if err != nil {
		log.Printf("database create error: %v", err)
		if isUserError(err) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create database")
		return
	}

	writeJSON(w, http.StatusCreated, db)
}

func (s *Server) handleDatabaseList(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	dbs, err := s.dbManager.List(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list databases")
		return
	}

	writeJSON(w, http.StatusOK, dbs)
}

func (s *Server) handleDatabaseGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	dbID := chi.URLParam(r, "dbID")

	db, err := s.dbManager.Get(r.Context(), tenantID, dbID)
	if err != nil {
		if err.Error() == "database not found" {
			writeError(w, http.StatusNotFound, "database not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get database")
		return
	}

	writeJSON(w, http.StatusOK, db)
}

func (s *Server) handleDatabaseConnectionString(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	dbID := chi.URLParam(r, "dbID")

	connStr, err := s.dbManager.GetConnectionString(r.Context(), tenantID, dbID)
	if err != nil {
		if err.Error() == "database not found" {
			writeError(w, http.StatusNotFound, "database not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get connection string")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"connection_string": connStr})
}

func (s *Server) handleDatabaseDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	dbID := chi.URLParam(r, "dbID")

	if err := s.dbManager.Delete(r.Context(), tenantID, dbID); err != nil {
		if err.Error() == "database not found" {
			writeError(w, http.StatusNotFound, "database not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete database")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// isUserError checks if an error is a user-facing validation error.
func isUserError(err error) bool {
	msg := err.Error()
	switch {
	case len(msg) > 7 && msg[:7] == "invalid":
		return true
	case msg == "name is required (max 128 chars)":
		return true
	case msg == "database quota exceeded (max 3)":
		return true
	}
	return false
}
