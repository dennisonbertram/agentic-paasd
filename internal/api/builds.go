package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/paasd/paasd/internal/builds"
	"github.com/paasd/paasd/internal/middleware"
)

func (s *Server) requireBuildManager(w http.ResponseWriter) bool {
	if s.buildManager == nil {
		writeError(w, http.StatusServiceUnavailable, "build system is not available")
		return false
	}
	return true
}

func (s *Server) handleBuildCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	var req builds.StartBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	build, err := s.buildManager.StartBuild(r.Context(), tenantID, serviceID, req)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, msg)
			return
		}
		if strings.Contains(msg, "not allowed") || strings.Contains(msg, "unsupported") ||
			strings.Contains(msg, "required") || strings.Contains(msg, "too long") ||
			strings.Contains(msg, "invalid") || strings.Contains(msg, "credentials") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		if strings.Contains(msg, "already running") {
			writeError(w, http.StatusConflict, msg)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to start build")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"build_id": build.ID,
		"status":   build.Status,
		"image":    build.Image,
	})
}

func (s *Server) handleBuildList(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	result, err := s.buildManager.ListBuilds(r.Context(), tenantID, serviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list builds")
		return
	}
	if result == nil {
		result = []*builds.Build{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleBuildGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	buildID := chi.URLParam(r, "buildID")

	build, err := s.buildManager.GetBuild(r.Context(), tenantID, buildID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "build not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to get build")
		}
		return
	}
	writeJSON(w, http.StatusOK, build)
}

func (s *Server) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	buildID := chi.URLParam(r, "buildID")

	follow := r.URL.Query().Get("follow") == "true"

	if follow {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		if err := s.buildManager.StreamBuildLogs(r.Context(), tenantID, buildID, w); err != nil {
			// Can't write error after headers sent
			return
		}
		return
	}

	logs, err := s.buildManager.GetBuildLogs(r.Context(), tenantID, buildID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "build not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to get build logs")
		}
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(logs))
}

func (s *Server) handleBuildCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	buildID := chi.URLParam(r, "buildID")

	if err := s.buildManager.CancelBuild(r.Context(), tenantID, buildID); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, "build not found")
		} else if strings.Contains(msg, "not in progress") {
			writeError(w, http.StatusConflict, msg)
		} else {
			writeError(w, http.StatusInternalServerError, "failed to cancel build")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}
