package api

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
)

type HealthResponse struct {
	Status  string      `json:"status"`
	Docker  DockerInfo  `json:"docker"`
	GVisor  GVisorInfo  `json:"gvisor"`
	Disk    DiskInfo    `json:"disk"`
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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{Status: "ok"}

	// Check Docker
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output(); err == nil {
		resp.Docker = DockerInfo{Available: true, Version: strings.TrimSpace(string(out))}
	}

	// Check gVisor
	if out, err := exec.Command("runsc", "--version").Output(); err == nil {
		lines := strings.Split(string(out), "\n")
		version := ""
		for _, line := range lines {
			if strings.HasPrefix(line, "runsc version") {
				version = strings.TrimPrefix(line, "runsc version ")
				break
			}
		}
		resp.GVisor = GVisorInfo{Available: true, Version: version}
	}

	// Check disk
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

	// Check DB
	if err := s.store.StateDB.Ping(); err != nil {
		resp.Status = "degraded"
	}

	json.NewEncoder(w).Encode(resp)
}

func round2(f float64) float64 {
	return float64(int(f*100)) / 100
}
