// Package services coordinates DB records with Docker containers.
package services

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// Service represents a deployed service.
type Service struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Image       string `json:"image"`
	ContainerID string `json:"container_id,omitempty"`
	Port        int    `json:"port"`
	URL         string `json:"url,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	CrashCount   int    `json:"crash_count"`
	CircuitOpen  bool   `json:"circuit_open"`
	LastCrashedAt int64 `json:"last_crashed_at,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// CreateRequest holds parameters for creating a new service.
type CreateRequest struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Port  int               `json:"port"`
	Env   map[string]string `json:"env"`
}

// maxConcurrentDeploys limits simultaneous deploy operations globally.
const maxConcurrentDeploys = 5

// maxQueuedDeploys limits how many deploys can be waiting for a slot.
// If the queue is full, new deploy requests are rejected with backpressure.
const maxQueuedDeploys = 20

// imageAllowPattern restricts images to Docker Hub library (official) and
// standard namespace/repo:tag format. Blocks registry prefixes (e.g., evil.com/img).
var imageAllowPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)?(?::[a-zA-Z0-9._-]+)?$`)

// envKeyPattern validates environment variable key names.
var envKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`)

// maxEnvValueLen is the maximum length of an environment variable value.
const maxEnvValueLen = 32768 // 32KB

// deniedEnvKeys are environment variable names that cannot be set by tenants.
var deniedEnvKeys = map[string]bool{
	"LD_PRELOAD":     true,
	"LD_LIBRARY_PATH": true,
	"PATH":           true,
}

// isNotFoundError returns true if the error indicates a container definitively
// does not exist (Docker 404). Transient errors return false.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "No such container") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "404")
}

// Manager coordinates service lifecycle between DB and Docker.
type Manager struct {
	db          *sql.DB
	docker      *docker.Client
	masterKey   []byte
	deploySem   chan struct{} // bounded deploy concurrency
	deployQueue chan struct{} // bounded queue for waiting deploys
}

// NewManager creates a service manager.
func NewManager(db *sql.DB, docker *docker.Client, masterKey []byte) *Manager {
	if docker == nil {
		panic("ah: NewManager requires a non-nil Docker client")
	}
	return &Manager{
		db:          db,
		docker:      docker,
		masterKey:   masterKey,
		deploySem:   make(chan struct{}, maxConcurrentDeploys),
		deployQueue: make(chan struct{}, maxQueuedDeploys),
	}
}

// checkTenantActive verifies the tenant is not suspended/deleted.
// Returns an error if tenant is not in active state.
func (m *Manager) checkTenantActive(ctx context.Context, tenantID string) error {
	var status string
	err := m.db.QueryRowContext(ctx,
		`SELECT status FROM tenants WHERE id = ?`, tenantID,
	).Scan(&status)
	if err != nil {
		return fmt.Errorf("tenant not found")
	}
	if status != "active" {
		return fmt.Errorf("tenant is %s", status)
	}
	return nil
}

// ValidateImage checks that an image reference is allowed.
func ValidateImage(img string) error {
	if img == "" {
		return fmt.Errorf("image is required")
	}
	if len(img) > 256 {
		return fmt.Errorf("image reference too long")
	}
	if slashIdx := strings.IndexByte(img, '/'); slashIdx > 0 {
		prefix := img[:slashIdx]
		if strings.ContainsAny(prefix, ".:") {
			return fmt.Errorf("custom registries not allowed; use Docker Hub images only")
		}
	}
	if !imageAllowPattern.MatchString(img) {
		return fmt.Errorf("invalid image format")
	}
	return nil
}

// ValidateEnvVars checks env var keys and values for format and safety.
func ValidateEnvVars(vars map[string]string) error {
	for k, v := range vars {
		if !envKeyPattern.MatchString(k) {
			return fmt.Errorf("invalid env var key %q: must match [A-Za-z_][A-Za-z0-9_]{0,127}", k)
		}
		if deniedEnvKeys[strings.ToUpper(k)] {
			return fmt.Errorf("env var %q is not allowed", k)
		}
		if len(v) > maxEnvValueLen {
			return fmt.Errorf("env var %q value too long (max %d bytes)", k, maxEnvValueLen)
		}
		if strings.ContainsAny(v, "\x00") {
			return fmt.Errorf("env var %q value contains null bytes", k)
		}
	}
	return nil
}

// Create inserts a new service record (status=created, not yet running).
// Enforces tenant quota (max_services).
func (m *Manager) Create(ctx context.Context, tenantID string, req CreateRequest) (*Service, error) {
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		return nil, err
	}
	if err := ValidateImage(req.Image); err != nil {
		return nil, err
	}

	// Validate env vars if provided
	if len(req.Env) > 0 {
		if err := ValidateEnvVars(req.Env); err != nil {
			return nil, err
		}
	}

	// Enforce tenant quota
	var maxServices int
	err := m.db.QueryRowContext(ctx,
		`SELECT max_services FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxServices)
	if err != nil {
		return nil, fmt.Errorf("check quota: %w", err)
	}

	var currentCount int
	err = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM services WHERE tenant_id = ?`, tenantID,
	).Scan(&currentCount)
	if err != nil {
		return nil, fmt.Errorf("count services: %w", err)
	}
	if currentCount >= maxServices {
		return nil, fmt.Errorf("service limit reached (max %d)", maxServices)
	}

	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	port := req.Port
	if port <= 0 {
		port = 8000
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port must be between 1 and 65535")
	}

	// Use a transaction so service insert + env vars are atomic.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, 'created', ?, ?, '', ?, ?)`,
		id, tenantID, req.Name, req.Image, port, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert service: %w", err)
	}

	if len(req.Env) > 0 {
		for k, v := range req.Env {
			encrypted, encErr := crypto.Encrypt([]byte(v), m.masterKey)
			if encErr != nil {
				return nil, fmt.Errorf("encrypt env var %s: %w", k, encErr)
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(service_id, key) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = excluded.updated_at`,
				id, k, encrypted, now, now,
			)
			if err != nil {
				return nil, fmt.Errorf("insert env var %s: %w", k, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit service create: %w", err)
	}

	return &Service{
		ID:        id,
		TenantID:  tenantID,
		Name:      req.Name,
		Status:    "created",
		Image:     req.Image,
		Port:      port,
		URL:       fmt.Sprintf("http://%s.localhost", id),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Deploy pulls the image, reads env vars, creates and starts the container.
// Uses a bounded semaphore with a bounded queue for backpressure.
func (m *Manager) Deploy(ctx context.Context, tenantID, serviceID string) error {
	if m.docker == nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", "Docker client not configured")
		return fmt.Errorf("docker client not configured")
	}
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		m.updateStatusWithError(ctx, serviceID, "failed", err.Error())
		return err
	}
	// Try to enter the deploy queue; reject immediately if full (backpressure)
	select {
	case m.deployQueue <- struct{}{}:
		defer func() { <-m.deployQueue }()
	default:
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", "deploy queue full; try again later")
		return fmt.Errorf("deploy queue full; try again later")
	}

	// Acquire deploy slot (bounded concurrency)
	select {
	case m.deploySem <- struct{}{}:
		defer func() { <-m.deploySem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	if svc.CircuitOpen {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", "circuit breaker is open")
		return fmt.Errorf("circuit breaker is open: service has crashed too many times; use POST /reset to clear")
	}

	// Check disk space before deploy
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", err.Error())
		return fmt.Errorf("disk check: %w", err)
	}

	m.updateStatusScoped(ctx, tenantID, serviceID, "deploying")

	// Ensure per-tenant network exists for isolation
	_, err = m.docker.EnsureNetwork(ctx, docker.TenantNetworkName(tenantID))
	if err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("network setup failed: %v", err))
		return fmt.Errorf("ensure tenant network: %w", err)
	}

	if err := m.docker.PullImage(ctx, svc.Image); err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("image pull failed: %v", err))
		return fmt.Errorf("pull image: %w", err)
	}

	// Re-verify service still exists after the slow image pull.
	// The user may have deleted the service while we were pulling.
	svc, err = m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return fmt.Errorf("service deleted during deploy")
	}

	// Remove existing container before creating new one to prevent
	// "name already in use" errors on redeploy.
	if svc.ContainerID != "" {
		log.Printf("services: removing existing container %s before redeploy of %s", svc.ContainerID[:12], serviceID)
		_ = m.docker.StopContainer(ctx, svc.ContainerID)
		_ = m.docker.RemoveContainer(ctx, svc.ContainerID)
	}

	envVars, err := m.getEnvVars(ctx, serviceID)
	if err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("env vars load failed: %v", err))
		return fmt.Errorf("load env vars: %w", err)
	}

	port := svc.Port
	if port <= 0 {
		port = 8000
	}
	if p, ok := envVars["PORT"]; ok {
		var parsed int
		if _, err := fmt.Sscanf(p, "%d", &parsed); err == nil && parsed >= 1 && parsed <= 65535 {
			port = parsed
		}
	}

	// Load resource limits from tenant quotas
	limits := m.getResourceLimits(ctx, tenantID)

	containerID, err := m.docker.RunContainer(ctx, tenantID, serviceID, svc.Image, port, envVars, nil, limits)
	if err != nil {
		m.updateStatusWithErrorScoped(ctx, tenantID, serviceID, "failed", fmt.Sprintf("container start failed: %v", err))
		return fmt.Errorf("run container: %w", err)
	}

	now := time.Now().Unix()
	res, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = 'running', container_id = ?, last_error = '', updated_at = ? WHERE id = ? AND tenant_id = ?`,
		containerID, now, serviceID, tenantID,
	)
	if err != nil {
		// Container is running but DB update failed; try to clean up
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		return fmt.Errorf("update service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Service was deleted while we were deploying; clean up the container
		log.Printf("WARNING: service %s was deleted during deploy; removing orphan container %s", serviceID, containerID)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		return fmt.Errorf("service deleted during deploy")
	}
	return nil
}

// getResourceLimits reads per-service resource limits from tenant quotas.
func (m *Manager) getResourceLimits(ctx context.Context, tenantID string) *docker.ResourceLimits {
	var maxMemMB int
	var maxCPUCores float64
	err := m.db.QueryRowContext(ctx,
		`SELECT max_memory_mb, max_cpu_cores FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&maxMemMB, &maxCPUCores)
	if err != nil {
		log.Printf("services: failed to load resource limits for tenant %s: %v (using defaults)", tenantID, err)
		return nil // use defaults
	}
	limits := &docker.ResourceLimits{}
	if maxMemMB > 0 {
		limits.MemoryMB = int64(maxMemMB)
	}
	if maxCPUCores > 0 {
		limits.CPUCores = maxCPUCores
	}
	return limits
}

// Stop stops a running service container.
func (m *Manager) Stop(ctx context.Context, tenantID, serviceID string) error {
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}
	if svc.ContainerID == "" {
		return fmt.Errorf("service has no container")
	}

	if err := m.docker.StopContainer(ctx, svc.ContainerID); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	m.updateStatus(ctx, serviceID, "stopped")
	return nil
}

// Start starts a stopped service container.
func (m *Manager) Start(ctx context.Context, tenantID, serviceID string) error {
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		return err
	}
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}
	if svc.CircuitOpen {
		return fmt.Errorf("circuit breaker is open: service has crashed too many times; use POST /reset to clear")
	}
	if svc.ContainerID == "" {
		return fmt.Errorf("service has no container")
	}

	if err := m.docker.StartContainer(ctx, svc.ContainerID); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	m.updateStatus(ctx, serviceID, "running")
	return nil
}

// Restart stops and starts a service container.
func (m *Manager) Restart(ctx context.Context, tenantID, serviceID string) error {
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}
	if svc.CircuitOpen {
		return fmt.Errorf("circuit breaker is open: service has crashed too many times; use POST /reset to clear")
	}
	if svc.ContainerID == "" {
		return fmt.Errorf("service has no container")
	}

	if err := m.docker.StopContainer(ctx, svc.ContainerID); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	if err := m.docker.StartContainer(ctx, svc.ContainerID); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	m.updateStatus(ctx, serviceID, "running")
	return nil
}

// Delete stops and removes the container, then deletes the DB record.
func (m *Manager) Delete(ctx context.Context, tenantID, serviceID string) error {
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	if svc.ContainerID != "" {
		if stopErr := m.docker.StopContainer(ctx, svc.ContainerID); stopErr != nil {
			log.Printf("WARNING: failed to stop container %s for service %s: %v", svc.ContainerID, serviceID, stopErr)
		}
		if rmErr := m.docker.RemoveContainer(ctx, svc.ContainerID); rmErr != nil {
			log.Printf("WARNING: failed to remove container %s for service %s: %v (orphan container may remain)", svc.ContainerID, serviceID, rmErr)
		}
	}
	// Also try cleanup by deterministic container name to catch split-brain orphans
	// where a container exists but DB doesn't have its ID.
	expectedName := fmt.Sprintf("ah-%s-%s", tenantID, serviceID)
	if cleanupErr := m.docker.StopAndRemoveByName(ctx, expectedName); cleanupErr != nil {
		// Not an error — container may not exist by this name
		log.Printf("services: cleanup by name %s: %v", expectedName, cleanupErr)
	}

	_, err = m.db.ExecContext(ctx, `DELETE FROM services WHERE id = ? AND tenant_id = ?`, serviceID, tenantID)
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	return nil
}

// StopAllForTenant stops and removes all containers belonging to a tenant.
// Also cleans up any split-brain orphan containers by label.
func (m *Manager) StopAllForTenant(ctx context.Context, tenantID string) {
	// First, clean up any containers with this tenant's label (catches split-brain orphans)
	if labelContainers, err := m.docker.ListContainersByLabel(ctx, "ah.tenant", tenantID); err == nil {
		for _, cid := range labelContainers {
			_ = m.docker.StopContainer(ctx, cid)
			_ = m.docker.RemoveContainer(ctx, cid)
		}
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id FROM services WHERE tenant_id = ?`, tenantID,
	)
	if err != nil {
		log.Printf("services: failed to list services for tenant %s: %v", tenantID, err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var svcID string
		var containerID sql.NullString
		if err := rows.Scan(&svcID, &containerID); err != nil {
			continue
		}
		if containerID.Valid && containerID.String != "" {
			stopOk := true
			if stopErr := m.docker.StopContainer(ctx, containerID.String); stopErr != nil {
				log.Printf("WARNING: failed to stop container %s for tenant %s: %v", containerID.String, tenantID, stopErr)
				stopOk = false
			}
			if rmErr := m.docker.RemoveContainer(ctx, containerID.String); rmErr != nil {
				log.Printf("WARNING: failed to remove container %s for tenant %s: %v (orphan may remain)", containerID.String, tenantID, rmErr)
				stopOk = false
			}
			if stopOk {
				m.updateStatus(ctx, svcID, "stopped")
			} else {
				m.updateStatusWithError(ctx, svcID, "failed", "container cleanup failed during tenant suspension")
			}
		} else {
			m.updateStatus(ctx, svcID, "stopped")
		}
	}
}

// Logs returns a reader for the service container logs.
func (m *Manager) Logs(ctx context.Context, tenantID, serviceID string, follow bool, tail int) (io.ReadCloser, error) {
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return nil, err
	}
	if svc.ContainerID == "" {
		return nil, fmt.Errorf("service has no container")
	}
	return m.docker.LogsContainer(ctx, svc.ContainerID, follow, tail)
}

// List returns all services for a tenant.
func (m *Manager) List(ctx context.Context, tenantID string) ([]*Service, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, name, status, image, port, container_id, last_error, crash_count, circuit_open, last_crashed_at, created_at, updated_at
		 FROM services WHERE tenant_id = ? ORDER BY created_at DESC LIMIT 100`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()

	var svcs []*Service
	for rows.Next() {
		s := &Service{}
		var containerID sql.NullString
		var lastCrashedAt sql.NullInt64
		var circuitOpen int
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &s.Status, &s.Image, &s.Port, &containerID, &s.LastError, &s.CrashCount, &circuitOpen, &lastCrashedAt, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		if containerID.Valid {
			s.ContainerID = containerID.String
		}
		s.CircuitOpen = circuitOpen != 0
		if lastCrashedAt.Valid {
			s.LastCrashedAt = lastCrashedAt.Int64
		}
		s.URL = fmt.Sprintf("http://%s.localhost", s.ID)
		svcs = append(svcs, s)
	}
	return svcs, rows.Err()
}

// Get returns a single service by ID, scoped to tenant.
func (m *Manager) Get(ctx context.Context, tenantID, serviceID string) (*Service, error) {
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return nil, err
	}

	if svc.ContainerID != "" {
		info, err := m.docker.InspectContainer(ctx, svc.ContainerID)
		if err == nil {
			svc.Status = info.Status
		}
	}
	svc.URL = fmt.Sprintf("http://%s.localhost", svc.ID)
	return svc, nil
}

// SetEnv sets or updates environment variables for a service.
func (m *Manager) SetEnv(ctx context.Context, tenantID, serviceID string, vars map[string]string) error {
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}
	if err := ValidateEnvVars(vars); err != nil {
		return err
	}
	return m.setEnvVars(ctx, serviceID, vars)
}

// GetEnv returns env var keys for a service. If reveal is true, returns decrypted values.
// Audit logs reveal operations for security monitoring.
func (m *Manager) GetEnv(ctx context.Context, tenantID, serviceID string, reveal bool) (map[string]string, error) {
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return nil, err
	}
	if reveal {
		log.Printf("AUDIT: tenant=%s revealed env vars for service=%s", tenantID, serviceID)
		return m.getEnvVars(ctx, serviceID)
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT key FROM service_env WHERE service_id = ? ORDER BY key`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list env keys: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		result[key] = "********"
	}
	return result, rows.Err()
}

// DeleteEnv removes a single environment variable.
func (m *Manager) DeleteEnv(ctx context.Context, tenantID, serviceID, key string) error {
	if _, err := m.getOwned(ctx, tenantID, serviceID); err != nil {
		return err
	}
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM service_env WHERE service_id = ? AND key = ?`,
		serviceID, key,
	)
	if err != nil {
		return fmt.Errorf("delete env var: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("env var not found")
	}
	log.Printf("AUDIT: tenant=%s deleted env var %q for service=%s", tenantID, key, serviceID)
	return nil
}

// getOwned loads a service and verifies tenant ownership.
func (m *Manager) getOwned(ctx context.Context, tenantID, serviceID string) (*Service, error) {
	s := &Service{}
	var containerID sql.NullString
	var circuitOpenInt int
	var lastCrashedAtNull sql.NullInt64
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, status, image, port, container_id, last_error, crash_count, circuit_open, last_crashed_at, created_at, updated_at
		 FROM services WHERE id = ? AND tenant_id = ?`,
		serviceID, tenantID,
	).Scan(&s.ID, &s.TenantID, &s.Name, &s.Status, &s.Image, &s.Port, &containerID, &s.LastError, &s.CrashCount, &circuitOpenInt, &lastCrashedAtNull, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("service not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get service: %w", err)
	}
	if containerID.Valid {
		s.ContainerID = containerID.String
	}
	s.CircuitOpen = circuitOpenInt != 0
	if lastCrashedAtNull.Valid {
		s.LastCrashedAt = lastCrashedAtNull.Int64
	}
	return s, nil
}

func (m *Manager) updateStatus(ctx context.Context, serviceID, status string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), serviceID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update status for service %s to %s: %v", serviceID, status, err)
	}
}

func (m *Manager) updateStatusScoped(ctx context.Context, tenantID, serviceID, status string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
		status, time.Now().Unix(), serviceID, tenantID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update status for service %s to %s: %v", serviceID, status, err)
	}
}

func (m *Manager) updateStatusWithError(ctx context.Context, serviceID, status, lastError string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, lastError, time.Now().Unix(), serviceID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update status/error for service %s to %s: %v", serviceID, status, err)
	}
}

func (m *Manager) updateStatusWithErrorScoped(ctx context.Context, tenantID, serviceID, status, lastError string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE services SET status = ?, last_error = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`,
		status, lastError, time.Now().Unix(), serviceID, tenantID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update status/error for service %s to %s: %v", serviceID, status, err)
	}
}

// ResetCircuitBreaker resets the circuit breaker for a service, allowing it to restart.
func (m *Manager) ResetCircuitBreaker(ctx context.Context, tenantID, serviceID string) error {
	svc, err := m.getOwned(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	// If container exists, stop it before resetting.
	// Treat "not found" as success (container already gone is a valid terminal state).
	if svc.ContainerID != "" {
		if stopErr := m.docker.StopContainer(ctx, svc.ContainerID); stopErr != nil {
			if isNotFoundError(stopErr) {
				// Container is already gone — safe to proceed
				log.Printf("services: container %s already removed during reset", svc.ContainerID[:12])
			} else {
				// Stop failed for non-404 reason — check container state
				info, inspectErr := m.docker.InspectContainer(ctx, svc.ContainerID)
				if inspectErr != nil {
					if isNotFoundError(inspectErr) {
						// Container gone between stop and inspect — safe
						log.Printf("services: container %s disappeared during reset", svc.ContainerID[:12])
					} else {
						// Can't verify container state — fail closed
						return fmt.Errorf("cannot verify container state after stop failure: %v (stop error: %v)", inspectErr, stopErr)
					}
				} else if info.Status == "running" {
					return fmt.Errorf("failed to stop container before reset: %w", stopErr)
				}
				// Container exists but not running — safe to proceed
			}
		}
		// Also try to remove the container to clean up
		_ = m.docker.RemoveContainer(ctx, svc.ContainerID)
	}

	_, err = m.db.ExecContext(ctx,
		`UPDATE services SET crash_count = 0, circuit_open = 0, crash_window_start = NULL, status = 'stopped',
		 last_error = '', updated_at = ? WHERE id = ? AND tenant_id = ?`,
		time.Now().Unix(), serviceID, tenantID)
	if err != nil {
		return fmt.Errorf("reset circuit breaker: %w", err)
	}
	log.Printf("services: circuit breaker reset for %s", serviceID)
	return nil
}

func (m *Manager) setEnvVars(ctx context.Context, serviceID string, vars map[string]string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	for k, v := range vars {
		encrypted, err := crypto.Encrypt([]byte(v), m.masterKey)
		if err != nil {
			return fmt.Errorf("encrypt env var %s: %w", k, err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(service_id, key) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = excluded.updated_at`,
			serviceID, k, encrypted, now, now,
		)
		if err != nil {
			return fmt.Errorf("upsert env var %s: %w", k, err)
		}
	}
	return tx.Commit()
}

func (m *Manager) getEnvVars(ctx context.Context, serviceID string) (map[string]string, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT key, value_encrypted FROM service_env WHERE service_id = ?`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("query env vars: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var key, encrypted string
		if err := rows.Scan(&key, &encrypted); err != nil {
			return nil, err
		}
		plaintext, err := crypto.Decrypt(encrypted, m.masterKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt env var %s: %w", key, err)
		}
		result[key] = string(plaintext)
	}
	return result, rows.Err()
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
