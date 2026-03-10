package api

import (
	"context"
	"math"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/paasd/paasd/internal/middleware"
)

type HealthResponse struct {
	Status string `json:"status"`
}

type DetailedHealthResponse struct {
	Status string     `json:"status"`
	Docker DockerInfo `json:"docker"`
	GVisor GVisorInfo `json:"gvisor"`
	Disk   DiskInfo   `json:"disk"`
}

type DockerInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

type GVisorInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

type DiskInfo struct {
	TotalGB     float64 `json:"total_gb"`
	FreeGB      float64 `json:"free_gb"`
	UsedPercent float64 `json:"used_percent"`
}

var (
	detailedHealthCache     DetailedHealthResponse
	detailedHealthCacheValid bool
	detailedHealthCacheMu   sync.RWMutex
	detailedHealthCacheTime time.Time
	detailedHealthCacheTTL  = 30 * time.Second
)

// handleHealth returns minimal constant-time status for public (unauthenticated) requests.
// No DB calls — avoids DoS via unauthenticated health check flooding.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
}

// handleHealthDetailed returns full system info (authenticated only).
func (s *Server) handleHealthDetailed(w http.ResponseWriter, r *http.Request) {
	_ = middleware.GetTenantID(r.Context()) // auth enforced by middleware

	detailedHealthCacheMu.RLock()
	if detailedHealthCacheValid && time.Since(detailedHealthCacheTime) < detailedHealthCacheTTL {
		resp := detailedHealthCache // copy by value under lock
		detailedHealthCacheMu.RUnlock()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	detailedHealthCacheMu.RUnlock()

	resp := s.buildDetailedHealth()

	detailedHealthCacheMu.Lock()
	detailedHealthCache = resp // store by value
	detailedHealthCacheValid = true
	detailedHealthCacheTime = time.Now()
	detailedHealthCacheMu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) buildDetailedHealth() DetailedHealthResponse {
	resp := DetailedHealthResponse{Status: "ok"}

	// Check Docker with 5s timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output(); err == nil {
		resp.Docker = DockerInfo{Available: true, Version: strings.TrimSpace(string(out))}
	}

	// Check gVisor with 5s timeout
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if out, err := exec.CommandContext(ctx2, "runsc", "--version").Output(); err == nil {
		lines := strings.Split(string(out), "\n")
		version := ""
		for _, line := range lines {
			if strings.HasPrefix(line, "runsc version") {
				version = strings.TrimPrefix(line, "runsc version ")
				break
			}
		}
		if version == "" {
			// Fallback: use first non-empty line if standard prefix not found
			version = strings.TrimSpace(string(out))
		}
		resp.GVisor = GVisorInfo{Available: version != "", Version: version}
	}

	// Check disk (no exec, safe syscall)
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		totalBytes := stat.Blocks * uint64(stat.Bsize)
		freeBytes := stat.Bavail * uint64(stat.Bsize)
		totalGB := float64(totalBytes) / (1024 * 1024 * 1024)
		freeGB := float64(freeBytes) / (1024 * 1024 * 1024)
		usedPercent := 0.0
		if totalGB > 0 {
			usedPercent = ((totalGB - freeGB) / totalGB) * 100
		}
		resp.Disk = DiskInfo{
			TotalGB:     round2(totalGB),
			FreeGB:      round2(freeGB),
			UsedPercent: round2(usedPercent),
		}
	}

	if err := s.store.StateDB.Ping(); err != nil {
		resp.Status = "degraded"
	}

	return resp
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
