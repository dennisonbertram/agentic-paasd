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

	"github.com/paasd/paasd/internal/crypto"
	"github.com/paasd/paasd/internal/docker"
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
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
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

	_, err = m.db.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, 'created', ?, ?, '', ?, ?)`,
		id, tenantID, req.Name, req.Image, port, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert service: %w", err)
	}

	if len(req.Env) > 0 {
		if err := m.setEnvVars(ctx, id, req.Env); err != nil {
			return nil, fmt.Errorf("set env vars: %w", err)
		}
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
	if err := m.checkTenantActive(ctx, tenantID); err != nil {
		m.updateStatus(ctx, serviceID, "failed")
		return err
	}
	// Try to enter the deploy queue; reject immediately if full (backpressure)
	select {
	case m.deployQueue <- struct{}{}:
		defer func() { <-m.deployQueue }()
	default:
		m.updateStatus(ctx, serviceID, "failed")
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

	m.updateStatus(ctx, serviceID, "deploying")

	// Ensure per-tenant network exists for isolation
	_, err = m.docker.EnsureNetwork(ctx, docker.TenantNetworkName(tenantID))
	if err != nil {
		m.updateStatus(ctx, serviceID, "failed")
		return fmt.Errorf("ensure tenant network: %w", err)
	}

	if err := m.docker.PullImage(ctx, svc.Image); err != nil {
		m.updateStatus(ctx, serviceID, "failed")
		return fmt.Errorf("pull image: %w", err)
	}

	envVars, err := m.getEnvVars(ctx, serviceID)
	if err != nil {
		m.updateStatus(ctx, serviceID, "failed")
		return fmt.Errorf("load env vars: %w", err)
	}

	port := svc.Port
	if port <= 0 {
		port = 8000
	}
	if p, ok := envVars["PORT"]; ok {
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			port = 8000
		}
	}

	// Load resource limits from tenant quotas
	limits := m.getResourceLimits(ctx, tenantID)

	containerID, err := m.docker.RunContainer(ctx, tenantID, serviceID, svc.Image, port, envVars, nil, limits)
	if err != nil {
		m.updateStatus(ctx, serviceID, "failed")
		return fmt.Errorf("run container: %w", err)
	}

	now := time.Now().Unix()
	_, err = m.db.ExecContext(ctx,
		`UPDATE services SET status = 'running', container_id = ?, updated_at = ? WHERE id = ?`,
		containerID, now, serviceID,
	)
	if err != nil {
		return fmt.Errorf("update service: %w", err)
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
		_ = m.docker.StopContainer(ctx, svc.ContainerID)
		_ = m.docker.RemoveContainer(ctx, svc.ContainerID)
	}

	_, err = m.db.ExecContext(ctx, `DELETE FROM services WHERE id = ? AND tenant_id = ?`, serviceID, tenantID)
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	return nil
}

// StopAllForTenant stops and removes all containers belonging to a tenant.
func (m *Manager) StopAllForTenant(ctx context.Context, tenantID string) {
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
			_ = m.docker.StopContainer(ctx, containerID.String)
			_ = m.docker.RemoveContainer(ctx, containerID.String)
		}
		m.updateStatus(ctx, svcID, "stopped")
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
		`SELECT id, tenant_id, name, status, image, port, container_id, created_at, updated_at
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
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &s.Status, &s.Image, &s.Port, &containerID, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		if containerID.Valid {
			s.ContainerID = containerID.String
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
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, status, image, port, container_id, created_at, updated_at
		 FROM services WHERE id = ? AND tenant_id = ?`,
		serviceID, tenantID,
	).Scan(&s.ID, &s.TenantID, &s.Name, &s.Status, &s.Image, &s.Port, &containerID, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("service not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get service: %w", err)
	}
	if containerID.Valid {
		s.ContainerID = containerID.String
	}
	return s, nil
}

func (m *Manager) updateStatus(ctx context.Context, serviceID, status string) {
	m.db.ExecContext(ctx,
		`UPDATE services SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), serviceID,
	)
}

func (m *Manager) setEnvVars(ctx context.Context, serviceID string, vars map[string]string) error {
	now := time.Now().Unix()
	for k, v := range vars {
		encrypted, err := crypto.Encrypt([]byte(v), m.masterKey)
		if err != nil {
			return fmt.Errorf("encrypt env var %s: %w", k, err)
		}
		_, err = m.db.ExecContext(ctx,
			`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(service_id, key) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = excluded.updated_at`,
			serviceID, k, encrypted, now, now,
		)
		if err != nil {
			return fmt.Errorf("upsert env var %s: %w", k, err)
		}
	}
	return nil
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
