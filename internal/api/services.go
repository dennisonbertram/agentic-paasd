package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/paasd/paasd/internal/middleware"
	"github.com/paasd/paasd/internal/services"
)

// requireSvcManager is a guard that returns 503 if svcManager is nil.
// All service handlers must call this before proceeding.
func (s *Server) requireSvcManager(w http.ResponseWriter) bool {
	if s.svcManager == nil {
		writeError(w, http.StatusServiceUnavailable, "service management is not available (Docker not configured)")
		return false
	}
	return true
}

func (s *Server) handleServiceCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	var req services.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Name) > 128 {
		writeError(w, http.StatusBadRequest, "name must be at most 128 characters")
		return
	}

	// Validate image format and registry allowlist
	if err := services.ValidateImage(req.Image); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate env vars if provided inline
	if len(req.Env) > 0 {
		if err := services.ValidateEnvVars(req.Env); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	svc, err := s.svcManager.Create(r.Context(), tenantID, req)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "service limit reached") {
			writeError(w, http.StatusForbidden, msg)
			return
		}
		if strings.Contains(msg, "invalid") || strings.Contains(msg, "not allowed") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		if strings.Contains(msg, "tenant") {
			writeError(w, http.StatusForbidden, msg)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create service")
		return
	}

	// Deploy asynchronously — return immediately with status "deploying".
	// Use a bounded context (10 min) to prevent goroutine leaks from stuck deploys.
	go func(tid, sid string) {
		deployCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.svcManager.Deploy(deployCtx, tid, sid); err != nil {
			log.Printf("deploy failed for service %s: %v", sid, err)
			return
		}
	}(tenantID, svc.ID)

	svc.Status = "deploying"
	writeJSON(w, http.StatusCreated, svc)
}

func (s *Server) handleServiceList(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	svcs, err := s.svcManager.List(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list services")
		return
	}
	if svcs == nil {
		svcs = []*services.Service{}
	}
	writeJSON(w, http.StatusOK, svcs)
}

func (s *Server) handleServiceGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	svc, err := s.svcManager.Get(r.Context(), tenantID, serviceID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "service not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to get service")
		}
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Delete(r.Context(), tenantID, serviceID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "service not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to delete service: "+err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Start(r.Context(), tenantID, serviceID); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, msg)
		} else if strings.Contains(msg, "tenant") {
			writeError(w, http.StatusForbidden, msg)
		} else if strings.Contains(msg, "no container") {
			writeError(w, http.StatusConflict, msg)
		} else {
			writeError(w, http.StatusBadRequest, msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Stop(r.Context(), tenantID, serviceID); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, msg)
		} else if strings.Contains(msg, "no container") {
			writeError(w, http.StatusConflict, msg)
		} else {
			writeError(w, http.StatusBadRequest, msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleServiceRestart(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Restart(r.Context(), tenantID, serviceID); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, msg)
		} else if strings.Contains(msg, "no container") {
			writeError(w, http.StatusConflict, msg)
		} else {
			writeError(w, http.StatusBadRequest, msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	follow := r.URL.Query().Get("follow") == "true"
	tail := 100
	if t := r.URL.Query().Get("tail"); t != "" {
		if v, err := strconv.Atoi(t); err == nil && v > 0 && v <= 10000 {
			tail = v
		}
	}

	reader, err := s.svcManager.Logs(r.Context(), tenantID, serviceID, follow, tail)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, "service not found")
		} else if strings.Contains(msg, "no container") {
			writeError(w, http.StatusConflict, "service has no container yet")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to get logs")
		}
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if follow {
		w.Header().Set("Transfer-Encoding", "chunked")
	}
	w.WriteHeader(http.StatusOK)

	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				f.Flush()
			}
			if err != nil {
				break
			}
		}
	} else {
		io.Copy(w, reader)
	}
}

func (s *Server) handleServiceEnvGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")
	reveal := r.URL.Query().Get("reveal") == "true"

	vars, err := s.svcManager.GetEnv(r.Context(), tenantID, serviceID, reveal)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "service not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to get env vars")
		}
		return
	}
	writeJSON(w, http.StatusOK, vars)
}

func (s *Server) handleServiceEnvSet(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	var vars map[string]string
	if err := json.NewDecoder(r.Body).Decode(&vars); err != nil {
		writeDecodeError(w, err)
		return
	}

	if len(vars) == 0 {
		writeError(w, http.StatusBadRequest, "no environment variables provided")
		return
	}
	if len(vars) > 100 {
		writeError(w, http.StatusBadRequest, "too many environment variables (max 100)")
		return
	}

	// Validate env var keys and values
	if err := services.ValidateEnvVars(vars); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.svcManager.SetEnv(r.Context(), tenantID, serviceID, vars); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, "service not found")
			return
		}
		if strings.Contains(msg, "invalid") || strings.Contains(msg, "not allowed") || strings.Contains(msg, "env var") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to set env vars")
		return
	}
	log.Printf("AUDIT: tenant=%s set env vars for service=%s keys=%v", tenantID, serviceID, envKeys(vars))
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "note": "restart service for changes to take effect"})
}

func (s *Server) handleServiceEnvDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")
	key := chi.URLParam(r, "key")

	if err := s.svcManager.DeleteEnv(r.Context(), tenantID, serviceID, key); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "env var not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to delete env var")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// envKeys extracts just the keys from a map for audit logging (no values).
func envKeys(vars map[string]string) []string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	return keys
}

func (s *Server) handleServiceReset(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.ResetCircuitBreaker(r.Context(), tenantID, serviceID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "service not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to reset circuit breaker")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "circuit breaker reset"})
}
