// Package databases manages Postgres and Redis database provisioning.
package databases

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// Database represents a provisioned database.
type Database struct {
	ID               string `json:"id"`
	TenantID         string `json:"tenant_id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	Status           string `json:"status"`
	ContainerID      string `json:"-"`
	Host             string `json:"host,omitempty"`
	Port             int    `json:"port,omitempty"`
	DBName           string `json:"db_name,omitempty"`
	Username         string `json:"username,omitempty"`
	ConnectionString string `json:"connection_string,omitempty"` // only populated on create + explicit get
	VolumeName       string `json:"-"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
}

// CreateRequest holds parameters for creating a database.
type CreateRequest struct {
	Name string `json:"name"`
	Type string `json:"type"` // "postgres" or "redis"
}

// Manager manages database lifecycle.
type Manager struct {
	db        *sql.DB
	docker    *docker.Client
	masterKey []byte
	mu        sync.Mutex // protects port allocation
}

// NewManager creates a database manager.
func NewManager(db *sql.DB, dockerClient *docker.Client, masterKey []byte) *Manager {
	if dockerClient == nil {
		panic("databases: NewManager requires non-nil docker client")
	}
	mgr := &Manager{
		db:        db,
		docker:    dockerClient,
		masterKey: masterKey,
	}
	mgr.ReconcileStale()
	return mgr
}

// ReconcileStale marks databases stuck in "provisioning" as "failed" and
// attempts to clean up their Docker resources. Called on startup.
func (m *Manager) ReconcileStale() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, container_id, volume_name FROM databases WHERE status = 'provisioning'`)
	if err != nil {
		log.Printf("databases: reconcile query failed: %v", err)
		return
	}
	defer rows.Close()

	var stale []struct{ id, containerID, volumeName string }
	for rows.Next() {
		var s struct{ id, containerID, volumeName string }
		if err := rows.Scan(&s.id, &s.containerID, &s.volumeName); err != nil {
			log.Printf("databases: reconcile scan failed: %v", err)
			continue
		}
		stale = append(stale, s)
	}

	for _, s := range stale {
		log.Printf("databases: reconciling stale database %s", s.id)
		if s.containerID != "" {
			_ = m.docker.StopContainer(ctx, s.containerID)
			_ = m.docker.RemoveContainer(ctx, s.containerID)
		}
		if s.volumeName != "" {
			_ = m.docker.RemoveVolume(ctx, s.volumeName)
		}
		m.updateStatus(ctx, s.id, "failed")
	}
	if len(stale) > 0 {
		log.Printf("databases: reconciled %d stale databases", len(stale))
	}
}

// Create provisions a new database.
func (m *Manager) Create(ctx context.Context, tenantID string, req CreateRequest) (*Database, error) {
	if req.Type != "postgres" && req.Type != "redis" {
		return nil, fmt.Errorf("invalid database type %q; must be \"postgres\" or \"redis\"", req.Type)
	}
	if req.Name == "" || len(req.Name) > 128 {
		return nil, fmt.Errorf("name is required (max 128 chars)")
	}

	// Check disk space before provisioning
	if err := diskcheck.CheckAll([]string{"/var/lib/ah", "/var/lib/docker"}, 80, 90); err != nil {
		return nil, fmt.Errorf("disk check: %w", err)
	}

	id, err := generateID()
	if err != nil {
		return nil, err
	}

	// Check quota inside an IMMEDIATE transaction to prevent concurrent creates
	// from both seeing count < 3
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	// Force write lock with a dummy write (SQLite IMMEDIATE)
	_, _ = tx.ExecContext(ctx, `UPDATE databases SET updated_at = updated_at WHERE id = 'lock'`)
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM databases WHERE tenant_id = ? AND status != 'failed'`, tenantID,
	).Scan(&count); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("check quota: %w", err)
	}
	if count >= 3 {
		tx.Rollback()
		return nil, fmt.Errorf("database quota exceeded (max 3)")
	}
	tx.Commit()

	// Generate password
	password, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}

	volumeName := fmt.Sprintf("ah-db-%s", id)
	containerName := fmt.Sprintf("ah-db-%s-%s", tenantID[:8], id[:16])

	// Encrypt password
	passwordEnc, err := crypto.Encrypt([]byte(password), m.masterKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt password: %w", err)
	}

	// Find free port and insert atomically — retry on UNIQUE constraint violation
	var port int
	var dbName, username, connStr string
	now := time.Now().Unix()
	const maxPortRetries = 5
	for attempt := 0; attempt < maxPortRetries; attempt++ {
		port, err = m.findFreePort(req.Type)
		if err != nil {
			return nil, fmt.Errorf("find free port: %w", err)
		}

		switch req.Type {
		case "postgres":
			dbName = "ah"
			username = "ah"
			connStr = fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", username, password, port, dbName)
		case "redis":
			connStr = fmt.Sprintf("redis://:%s@127.0.0.1:%d/0", password, port)
		}

		connStrEnc, encErr := crypto.Encrypt([]byte(connStr), m.masterKey)
		if encErr != nil {
			return nil, fmt.Errorf("encrypt connection string: %w", encErr)
		}

		// Insert DB record — UNIQUE index on port prevents race conditions
		_, err = m.db.ExecContext(ctx,
			`INSERT INTO databases (id, tenant_id, name, type, status, host, port, db_name, username,
			 password_encrypted, connection_string_encrypted, volume_name, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'provisioning', '127.0.0.1', ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, tenantID, req.Name, req.Type, port, dbName, username,
			passwordEnc, connStrEnc, volumeName, now, now,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				continue // retry with different port
			}
			return nil, fmt.Errorf("insert database: %w", err)
		}
		break // success
	}
	if err != nil {
		return nil, fmt.Errorf("insert database after %d retries: %w", maxPortRetries, err)
	}

	// Create Docker volume
	if err := m.docker.CreateVolume(ctx, volumeName); err != nil {
		m.updateStatus(ctx, id, "failed")
		return nil, fmt.Errorf("create volume: %w", err)
	}

	// Run container
	var containerID string
	switch req.Type {
	case "postgres":
		containerID, err = m.runPostgres(ctx, containerName, volumeName, port, dbName, username, password)
	case "redis":
		containerID, err = m.runRedis(ctx, containerName, volumeName, port, password)
	}
	if err != nil {
		m.updateStatus(ctx, id, "failed")
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("run container: %w", err)
	}

	// Update container ID — check rows affected to detect concurrent Delete
	result, updateErr := m.db.ExecContext(ctx,
		`UPDATE databases SET container_id = ?, updated_at = ? WHERE id = ?`,
		containerID, time.Now().Unix(), id,
	)
	if updateErr != nil || result == nil {
		log.Printf("databases: failed to update container_id for %s, cleaning up", id)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("database record deleted during provisioning")
	}
	if rowsAffected, _ := result.RowsAffected(); rowsAffected == 0 {
		log.Printf("databases: record %s was deleted during provisioning, cleaning up", id)
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("database record deleted during provisioning")
	}

	// Wait for health check
	healthy := false
	switch req.Type {
	case "postgres":
		healthy = m.waitForPostgres(port, password, 30*time.Second)
	case "redis":
		healthy = m.waitForRedis(port, password, 30*time.Second)
	}

	if !healthy {
		if m.updateStatus(ctx, id, "failed") {
			_ = m.docker.StopContainer(ctx, containerID)
			_ = m.docker.RemoveContainer(ctx, containerID)
			_ = m.docker.RemoveVolume(ctx, volumeName)
		} else {
			// Row was deleted concurrently — still clean up Docker resources
			_ = m.docker.StopContainer(ctx, containerID)
			_ = m.docker.RemoveContainer(ctx, containerID)
			_ = m.docker.RemoveVolume(ctx, volumeName)
		}
		return nil, fmt.Errorf("database health check failed after 30s")
	}

	if !m.updateStatus(ctx, id, "ready") {
		// Row was deleted during provisioning — clean up
		_ = m.docker.StopContainer(ctx, containerID)
		_ = m.docker.RemoveContainer(ctx, containerID)
		_ = m.docker.RemoveVolume(ctx, volumeName)
		return nil, fmt.Errorf("database record deleted during provisioning")
	}

	return &Database{
		ID:               id,
		TenantID:         tenantID,
		Name:             req.Name,
		Type:             req.Type,
		Status:           "ready",
		Host:             "127.0.0.1",
		Port:             port,
		DBName:           dbName,
		Username:         username,
		ConnectionString: connStr,
		VolumeName:       volumeName,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// Get returns a database by ID, scoped to tenant. Connection string NOT included.
func (m *Manager) Get(ctx context.Context, tenantID, dbID string) (*Database, error) {
	d := &Database{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, name, type, status, container_id, host, port, db_name, username,
		 volume_name, created_at, updated_at
		 FROM databases WHERE id = ? AND tenant_id = ?`,
		dbID, tenantID,
	).Scan(&d.ID, &d.TenantID, &d.Name, &d.Type, &d.Status, &d.ContainerID,
		&d.Host, &d.Port, &d.DBName, &d.Username,
		&d.VolumeName, &d.CreatedAt, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("database not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get database: %w", err)
	}
	return d, nil
}

// List returns all databases for a tenant. Connection strings NOT included.
func (m *Manager) List(ctx context.Context, tenantID string) ([]*Database, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, tenant_id, name, type, status, host, port, db_name, username, created_at, updated_at
		 FROM databases WHERE tenant_id = ? ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()

	var result []*Database
	for rows.Next() {
		d := &Database{}
		if err := rows.Scan(&d.ID, &d.TenantID, &d.Name, &d.Type, &d.Status,
			&d.Host, &d.Port, &d.DBName, &d.Username, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan database: %w", err)
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// GetConnectionString returns the decrypted connection string.
func (m *Manager) GetConnectionString(ctx context.Context, tenantID, dbID string) (string, error) {
	var connStrEnc string
	err := m.db.QueryRowContext(ctx,
		`SELECT connection_string_encrypted FROM databases WHERE id = ? AND tenant_id = ?`,
		dbID, tenantID,
	).Scan(&connStrEnc)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("database not found")
	}
	if err != nil {
		return "", fmt.Errorf("get connection string: %w", err)
	}
	plaintext, err := crypto.Decrypt(connStrEnc, m.masterKey)
	if err != nil {
		return "", fmt.Errorf("decrypt connection string: %w", err)
	}
	return string(plaintext), nil
}

// Delete destroys a database: stops container, removes volume, deletes record.
// Only deletes the DB record after Docker cleanup succeeds to avoid orphaning resources.
func (m *Manager) Delete(ctx context.Context, tenantID, dbID string) error {
	d, err := m.Get(ctx, tenantID, dbID)
	if err != nil {
		return err
	}

	// Stop and remove container
	if d.ContainerID != "" {
		if err := m.docker.StopContainer(ctx, d.ContainerID); err != nil {
			log.Printf("databases: stop container %s: %v", d.ContainerID, err)
			// Container may not exist (already stopped/removed) — continue
		}
		if err := m.docker.RemoveContainer(ctx, d.ContainerID); err != nil {
			// If container removal fails (not just "not found"), keep the record
			if !strings.Contains(err.Error(), "No such container") &&
				!strings.Contains(err.Error(), "not found") {
				log.Printf("databases: remove container %s failed, keeping record: %v", d.ContainerID, err)
				return fmt.Errorf("failed to remove database container")
			}
		}
	}

	// Remove volume
	if d.VolumeName != "" {
		if err := m.docker.RemoveVolume(ctx, d.VolumeName); err != nil {
			if !strings.Contains(err.Error(), "no such volume") &&
				!strings.Contains(err.Error(), "not found") {
				log.Printf("databases: remove volume %s failed, keeping record: %v", d.VolumeName, err)
				return fmt.Errorf("failed to remove database volume")
			}
		}
	}

	// Delete record only after Docker cleanup succeeded
	_, err = m.db.ExecContext(ctx, `DELETE FROM databases WHERE id = ? AND tenant_id = ?`, dbID, tenantID)
	if err != nil {
		return fmt.Errorf("delete database record: %w", err)
	}

	return nil
}

func (m *Manager) updateStatus(ctx context.Context, id, status string) bool {
	// Use a fresh context to ensure status updates succeed even if the
	// request context is cancelled or timed out.
	freshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := m.db.ExecContext(freshCtx,
		`UPDATE databases SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id,
	)
	if err != nil {
		log.Printf("databases: failed to update status for %s: %v", id, err)
		return false
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		log.Printf("databases: record %s was deleted, status update skipped", id)
		return false
	}
	return true
}

func (m *Manager) findFreePort(dbType string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var minPort, maxPort int
	switch dbType {
	case "postgres":
		minPort, maxPort = 5432, 6000
	case "redis":
		minPort, maxPort = 6379, 7000
	}

	// Check which ports are already allocated in DB
	rows, err := m.db.Query(`SELECT port FROM databases WHERE port IS NOT NULL AND status NOT IN ('failed')`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	usedPorts := make(map[int]bool)
	for rows.Next() {
		var p int
		rows.Scan(&p)
		usedPorts[p] = true
	}

	// Find first free port
	for port := minPort; port <= maxPort; port++ {
		if usedPorts[port] {
			continue
		}
		// Also check if port is actually available on the host
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free ports available in range %d-%d", minPort, maxPort)
}

func (m *Manager) runPostgres(ctx context.Context, name, volume string, port int, dbName, user, password string) (string, error) {
	return m.docker.RunDatabase(ctx, docker.RunDatabaseConfig{
		Name:          name,
		Image:         "postgres:16-alpine",
		HostPort:      port,
		ContainerPort: 5432,
		Env: map[string]string{
			"POSTGRES_DB":       dbName,
			"POSTGRES_USER":     user,
			"POSTGRES_PASSWORD": password,
		},
		VolumeName: volume,
		MountPath:  "/var/lib/postgresql/data",
	})
}

func (m *Manager) runRedis(ctx context.Context, name, volume string, port int, password string) (string, error) {
	return m.docker.RunDatabase(ctx, docker.RunDatabaseConfig{
		Name:          name,
		Image:         "redis:7-alpine",
		HostPort:      port,
		ContainerPort: 6379,
		Env:           map[string]string{},
		Cmd:           []string{"redis-server", "--requirepass", password},
		VolumeName:    volume,
		MountPath:     "/data",
	})
}

func (m *Manager) waitForPostgres(port int, password string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(1 * time.Second)
	}
	return false
}

func (m *Manager) waitForRedis(port int, password string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(1 * time.Second)
	}
	return false
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("random hex: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
