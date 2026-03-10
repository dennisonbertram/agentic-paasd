package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/paasd/paasd/internal/middleware"
	"github.com/paasd/paasd/internal/services"
)

func (s *Server) handleServiceCreate(w http.ResponseWriter, r *http.Request) {
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
	if req.Image == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	if len(req.Image) > 512 {
		writeError(w, http.StatusBadRequest, "image must be at most 512 characters")
		return
	}

	svc, err := s.svcManager.Create(r.Context(), tenantID, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create service")
		return
	}

	// Deploy asynchronously — return immediately with status "deploying".
	// Use context.Background() because r.Context() is canceled after the response.
	go func(tid, sid string) {
		if err := s.svcManager.Deploy(context.Background(), tid, sid); err != nil {
			// Status already set to "failed" by Deploy on error
			return
		}
	}(tenantID, svc.ID)

	svc.Status = "deploying"
	writeJSON(w, http.StatusCreated, svc)
}

func (s *Server) handleServiceList(w http.ResponseWriter, r *http.Request) {
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
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	svc, err := s.svcManager.Get(r.Context(), tenantID, serviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Delete(r.Context(), tenantID, serviceID); err != nil {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Start(r.Context(), tenantID, serviceID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Stop(r.Context(), tenantID, serviceID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleServiceRestart(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Restart(r.Context(), tenantID, serviceID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusNotFound, "service not found or no container")
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
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")
	reveal := r.URL.Query().Get("reveal") == "true"

	vars, err := s.svcManager.GetEnv(r.Context(), tenantID, serviceID, reveal)
	if err != nil {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, vars)
}

func (s *Server) handleServiceEnvSet(w http.ResponseWriter, r *http.Request) {
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

	if err := s.svcManager.SetEnv(r.Context(), tenantID, serviceID, vars); err != nil {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "note": "restart service for changes to take effect"})
}

func (s *Server) handleServiceEnvDelete(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")
	key := chi.URLParam(r, "key")

	if err := s.svcManager.DeleteEnv(r.Context(), tenantID, serviceID, key); err != nil {
		writeError(w, http.StatusNotFound, "env var not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
