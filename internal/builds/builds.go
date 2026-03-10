// Package builds coordinates build lifecycle between DB and builder.
package builds

import (
	"context"
	cryptoRand "crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/paasd/paasd/internal/builder"
)

// Build represents a build record.
type Build struct {
	ID           string `json:"id"`
	ServiceID    string `json:"service_id"`
	TenantID     string `json:"tenant_id"`
	Status       string `json:"status"`
	SourceType   string `json:"source_type"`
	SourceURL    string `json:"source_url,omitempty"`
	SourceRef    string `json:"source_ref"`
	Image        string `json:"image,omitempty"`
	NixpacksPlan string `json:"nixpacks_plan,omitempty"`
	Log          string `json:"-"`
	StartedAt    *int64 `json:"started_at,omitempty"`
	FinishedAt   *int64 `json:"finished_at,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// StartBuildRequest holds parameters for starting a build.
type StartBuildRequest struct {
	SourceType string `json:"source_type"`
	SourceURL  string `json:"source_url"`
	SourceRef  string `json:"source_ref"`
}

// DeployFunc is called when a build succeeds to deploy the resulting image.
type DeployFunc func(ctx context.Context, tenantID, serviceID, imageTag string) error

// Manager coordinates build lifecycle.
type Manager struct {
	db       *sql.DB
	builder  *builder.Builder
	deployFn DeployFunc
	logMu    sync.Mutex
	logSubs  map[string][]chan string // buildID -> subscribers
}

// NewManager creates a build manager.
func NewManager(db *sql.DB, b *builder.Builder, deployFn DeployFunc) *Manager {
	return &Manager{
		db:      db,
		builder: b,
		deployFn: deployFn,
		logSubs: make(map[string][]chan string),
	}
}

// ImageTag generates a deterministic image tag for local registry.
func ImageTag(tenantID, serviceID, buildID string) string {
	tPrefix := tenantID
	if len(tPrefix) > 8 {
		tPrefix = tPrefix[:8]
	}
	sPrefix := serviceID
	if len(sPrefix) > 8 {
		sPrefix = sPrefix[:8]
	}
	bPrefix := buildID
	if len(bPrefix) > 8 {
		bPrefix = bPrefix[:8]
	}
	return fmt.Sprintf("127.0.0.1:5000/paasd/%s-%s:%s", tPrefix, sPrefix, bPrefix)
}

// StartBuild creates a build record and starts the build asynchronously.
func (m *Manager) StartBuild(ctx context.Context, tenantID, serviceID string, req StartBuildRequest) (*Build, error) {
	if req.SourceType != "git" {
		return nil, fmt.Errorf("unsupported source_type: %s (only 'git' is supported)", req.SourceType)
	}
	if req.SourceURL == "" {
		return nil, fmt.Errorf("source_url is required for git builds")
	}
	if err := validateGitURL(req.SourceURL); err != nil {
		return nil, err
	}

	ref := req.SourceRef
	if ref == "" {
		ref = "main"
	}
	if len(ref) > 256 {
		return nil, fmt.Errorf("source_ref too long (max 256)")
	}

	// Verify service exists and belongs to tenant
	var svcExists int
	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM services WHERE id = ? AND tenant_id = ?`,
		serviceID, tenantID,
	).Scan(&svcExists)
	if err != nil || svcExists == 0 {
		return nil, fmt.Errorf("service not found")
	}

	buildID, err := generateID()
	if err != nil {
		return nil, err
	}

	imageTag := ImageTag(tenantID, serviceID, buildID)
	now := time.Now().Unix()

	_, err = m.db.ExecContext(ctx,
		`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, created_at)
		 VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?)`,
		buildID, serviceID, tenantID, req.SourceType, req.SourceURL, ref, imageTag, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert build: %w", err)
	}

	build := &Build{
		ID:         buildID,
		ServiceID:  serviceID,
		TenantID:   tenantID,
		Status:     "pending",
		SourceType: req.SourceType,
		SourceURL:  req.SourceURL,
		SourceRef:  ref,
		Image:      imageTag,
		CreatedAt:  now,
	}

	go m.runBuild(buildID, tenantID, serviceID, builder.BuildRequest{
		BuildID:    buildID,
		ServiceID:  serviceID,
		TenantID:   tenantID,
		SourceType: req.SourceType,
		SourceURL:  req.SourceURL,
		SourceRef:  ref,
		ImageTag:   imageTag,
	})

	return build, nil
}

func (m *Manager) runBuild(buildID, tenantID, serviceID string, req builder.BuildRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	now := time.Now().Unix()
	m.db.ExecContext(ctx,
		`UPDATE builds SET status = 'running', started_at = ? WHERE id = ?`,
		now, buildID,
	)

	logCb := func(line string) {
		m.appendLog(ctx, buildID, line)
		m.notifyLogSubs(buildID, line)
	}

	err := m.builder.Build(ctx, req, logCb)

	finishedAt := time.Now().Unix()
	if err != nil {
		logCb("[paasd] BUILD FAILED: " + err.Error())
		log.Printf("build %s failed: %v", buildID, err)
		m.db.ExecContext(ctx,
			`UPDATE builds SET status = 'failed', finished_at = ? WHERE id = ?`,
			finishedAt, buildID,
		)
		m.closeLogSubs(buildID)
		return
	}

	m.db.ExecContext(ctx,
		`UPDATE builds SET status = 'succeeded', finished_at = ? WHERE id = ?`,
		finishedAt, buildID,
	)
	m.closeLogSubs(buildID)

	// Deploy the built image
	logCb("[paasd] Deploying built image...")
	if m.deployFn != nil {
		if deployErr := m.deployFn(ctx, tenantID, serviceID, req.ImageTag); deployErr != nil {
			log.Printf("build %s succeeded but deploy failed: %v", buildID, deployErr)
			logCb("[paasd] Deploy failed: " + deployErr.Error())
		} else {
			logCb("[paasd] Deploy succeeded")
		}
	}
}

func (m *Manager) appendLog(ctx context.Context, buildID, line string) {
	_, err := m.db.ExecContext(ctx,
		`UPDATE builds SET log = log || ? || char(10) WHERE id = ?`,
		line, buildID,
	)
	if err != nil {
		log.Printf("builds: failed to append log for %s: %v", buildID, err)
	}
}

func (m *Manager) notifyLogSubs(buildID, line string) {
	m.logMu.Lock()
	subs := m.logSubs[buildID]
	m.logMu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- line:
		default:
		}
	}
}

func (m *Manager) closeLogSubs(buildID string) {
	m.logMu.Lock()
	subs := m.logSubs[buildID]
	delete(m.logSubs, buildID)
	m.logMu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
}

func (m *Manager) subscribeLogs(buildID string) chan string {
	ch := make(chan string, 100)
	m.logMu.Lock()
	m.logSubs[buildID] = append(m.logSubs[buildID], ch)
	m.logMu.Unlock()
	return ch
}

// GetBuild returns a build by ID, scoped to tenant.
func (m *Manager) GetBuild(ctx context.Context, tenantID, buildID string) (*Build, error) {
	b := &Build{}
	err := m.db.QueryRowContext(ctx,
		`SELECT id, service_id, tenant_id, status, source_type, source_url, source_ref, image, log, started_at, finished_at, created_at
		 FROM builds WHERE id = ? AND tenant_id = ?`,
		buildID, tenantID,
	).Scan(&b.ID, &b.ServiceID, &b.TenantID, &b.Status, &b.SourceType, &b.SourceURL, &b.SourceRef, &b.Image, &b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("build not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get build: %w", err)
	}
	return b, nil
}

// ListBuilds returns builds for a service, scoped to tenant.
func (m *Manager) ListBuilds(ctx context.Context, tenantID, serviceID string) ([]*Build, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, service_id, tenant_id, status, source_type, source_url, source_ref, image, started_at, finished_at, created_at
		 FROM builds WHERE tenant_id = ? AND service_id = ? ORDER BY created_at DESC LIMIT 50`,
		tenantID, serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list builds: %w", err)
	}
	defer rows.Close()

	var result []*Build
	for rows.Next() {
		b := &Build{}
		if err := rows.Scan(&b.ID, &b.ServiceID, &b.TenantID, &b.Status, &b.SourceType, &b.SourceURL, &b.SourceRef, &b.Image, &b.StartedAt, &b.FinishedAt, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan build: %w", err)
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

// GetBuildLogs returns the full build log.
func (m *Manager) GetBuildLogs(ctx context.Context, tenantID, buildID string) (string, error) {
	var logText string
	err := m.db.QueryRowContext(ctx,
		`SELECT log FROM builds WHERE id = ? AND tenant_id = ?`,
		buildID, tenantID,
	).Scan(&logText)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("build not found")
	}
	if err != nil {
		return "", fmt.Errorf("get build logs: %w", err)
	}
	return logText, nil
}

// StreamBuildLogs writes the existing log then streams new lines.
func (m *Manager) StreamBuildLogs(ctx context.Context, tenantID, buildID string, w io.Writer) error {
	build, err := m.GetBuild(ctx, tenantID, buildID)
	if err != nil {
		return err
	}

	if build.Log != "" {
		if _, err := io.WriteString(w, build.Log); err != nil {
			return err
		}
	}

	if build.Status == "succeeded" || build.Status == "failed" {
		return nil
	}

	ch := m.subscribeLogs(buildID)
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return nil
			}
			if _, err := io.WriteString(w, line+"\n"); err != nil {
				return err
			}
			if f, ok := w.(interface{ Flush() }); ok {
				f.Flush()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// CancelBuild cancels a running build.
func (m *Manager) CancelBuild(ctx context.Context, tenantID, buildID string) error {
	build, err := m.GetBuild(ctx, tenantID, buildID)
	if err != nil {
		return err
	}
	if build.Status != "pending" && build.Status != "running" {
		return fmt.Errorf("build is not in progress (status: %s)", build.Status)
	}

	if err := m.builder.CancelBuild(buildID); err != nil {
		log.Printf("builds: cancel build %s: %v", buildID, err)
	}

	now := time.Now().Unix()
	m.db.ExecContext(ctx,
		`UPDATE builds SET status = 'failed', finished_at = ? WHERE id = ?`,
		now, buildID,
	)
	m.appendLog(ctx, buildID, "[paasd] Build cancelled by user")
	m.closeLogSubs(buildID)
	return nil
}

func validateGitURL(rawURL string) error {
	if len(rawURL) > 2048 {
		return fmt.Errorf("source_url too long (max 2048)")
	}
	if !strings.HasPrefix(rawURL, "https://") {
		return fmt.Errorf("only HTTPS git URLs are allowed")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid git URL: %w", err)
	}

	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "192.168.") ||
		strings.HasPrefix(host, "172.") {
		return fmt.Errorf("private/localhost URLs are not allowed")
	}

	if u.User != nil {
		return fmt.Errorf("credentials in URL are not allowed")
	}

	return nil
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
