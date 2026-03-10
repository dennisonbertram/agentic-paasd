// Package builder runs Nixpacks builds with resource limits.
package builder

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// BuildRequest describes a build to execute.
type BuildRequest struct {
	BuildID    string
	ServiceID  string
	TenantID   string
	SourceType string // "git"
	SourceURL  string // git clone URL
	SourceRef  string // branch/tag/sha
	ImageTag   string // e.g. 127.0.0.1:5000/paasd/abc-def:12345678
}

// Builder runs Nixpacks builds with resource limits.
type Builder struct {
	workDir  string // /var/lib/paasd/builds
	nixpacks string // path to nixpacks binary

	// Global concurrency: max 3 concurrent builds
	buildSem chan struct{}

	// Per-tenant concurrency: max 1 concurrent build per tenant
	tenantMu     sync.Mutex
	tenantBuilds map[string]struct{}

	// Active build processes for cancellation (keyed by buildID)
	procMu  sync.Mutex
	procMap map[string]string // buildID -> systemd unit name
}

// NewBuilder creates a builder.
func NewBuilder(workDir, nixpacksPath string) (*Builder, error) {
	if err := os.MkdirAll(workDir, 0700); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	// Verify nixpacks exists
	if _, err := exec.LookPath(nixpacksPath); err != nil {
		return nil, fmt.Errorf("nixpacks binary not found at %s: %w", nixpacksPath, err)
	}
	return &Builder{
		workDir:      workDir,
		nixpacks:     nixpacksPath,
		buildSem:     make(chan struct{}, 3),
		tenantBuilds: make(map[string]struct{}),
		procMap:      make(map[string]string),
	}, nil
}

// Build clones, builds, tags, and pushes an image.
func (b *Builder) Build(ctx context.Context, req BuildRequest, logCb func(string)) error {
	// Per-tenant concurrency check
	if !b.acquireTenant(req.TenantID) {
		return fmt.Errorf("a build is already running for this tenant")
	}
	defer b.releaseTenant(req.TenantID)

	// Global concurrency
	select {
	case b.buildSem <- struct{}{}:
		defer func() { <-b.buildSem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	buildDir := filepath.Join(b.workDir, req.BuildID)
	defer func() {
		if err := os.RemoveAll(buildDir); err != nil {
			log.Printf("builder: failed to cleanup build dir %s: %v", buildDir, err)
		}
	}()

	if err := os.MkdirAll(buildDir, 0755); err != nil {
		return fmt.Errorf("create build dir: %w", err)
	}
	// Make buildDir writable by paasd-builder
	if err := exec.Command("chown", "-R", "paasd-builder:paasd-builder", buildDir).Run(); err != nil {
		return fmt.Errorf("chown build dir: %w", err)
	}

	logCb("[paasd] Starting build " + req.BuildID)

	// Step 1: Clone
	if req.SourceType == "git" {
		if err := b.gitClone(ctx, req, buildDir, logCb); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
	} else {
		return fmt.Errorf("unsupported source type: %s", req.SourceType)
	}

	// Step 2: Nixpacks build
	if err := b.nixpacksBuild(ctx, req, buildDir, logCb); err != nil {
		return fmt.Errorf("nixpacks build: %w", err)
	}

	// Step 3: Push to local registry
	if err := b.pushImage(ctx, req, logCb); err != nil {
		return fmt.Errorf("push image: %w", err)
	}

	logCb("[paasd] Build succeeded: " + req.ImageTag)
	return nil
}

// CancelBuild stops a running build by stopping its systemd scope unit.
func (b *Builder) CancelBuild(buildID string) error {
	b.procMu.Lock()
	unitName, ok := b.procMap[buildID]
	b.procMu.Unlock()
	if !ok {
		return fmt.Errorf("build not found or not running")
	}
	// Stop the systemd scope unit, which kills all processes in it
	cmd := exec.Command("systemctl", "stop", unitName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("stop unit %s: %w", unitName, err)
	}
	return nil
}

func (b *Builder) acquireTenant(tenantID string) bool {
	b.tenantMu.Lock()
	defer b.tenantMu.Unlock()
	if _, ok := b.tenantBuilds[tenantID]; ok {
		return false
	}
	b.tenantBuilds[tenantID] = struct{}{}
	return true
}

func (b *Builder) releaseTenant(tenantID string) {
	b.tenantMu.Lock()
	defer b.tenantMu.Unlock()
	delete(b.tenantBuilds, tenantID)
}

func (b *Builder) trackUnit(buildID, unitName string) {
	b.procMu.Lock()
	b.procMap[buildID] = unitName
	b.procMu.Unlock()
}

func (b *Builder) untrackUnit(buildID string) {
	b.procMu.Lock()
	delete(b.procMap, buildID)
	b.procMu.Unlock()
}

// cleanupUnit stops a systemd scope unit if the context was cancelled.
// Called in defer to ensure no orphaned processes on timeout.
func (b *Builder) cleanupUnit(ctx context.Context, unitName string) {
	if ctx.Err() != nil {
		cmd := exec.Command("systemctl", "stop", unitName)
		if err := cmd.Run(); err != nil {
			log.Printf("builder: failed to stop unit %s on cleanup: %v", unitName, err)
		}
	}
}

// sanitizeURL strips credentials from a URL for logging.
func sanitizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid-url>"
	}
	u.User = nil
	return u.String()
}

// sanitizeLogLine strips control characters from build output (preserving tabs).
func sanitizeLogLine(line string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1 // drop
		}
		return r
	}, line)
}

func (b *Builder) gitClone(ctx context.Context, req BuildRequest, buildDir string, logCb func(string)) error {
	ref := req.SourceRef
	if ref == "" {
		ref = "main"
	}

	logCb(fmt.Sprintf("[paasd] Cloning %s (ref: %s)", sanitizeURL(req.SourceURL), ref))

	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Run git clone as paasd-builder user via systemd-run for isolation
	unitName := fmt.Sprintf("paasd-clone-%s.scope", req.BuildID[:16])
	cmd := exec.CommandContext(cloneCtx,
		"systemd-run", "--scope", "--quiet",
		"--unit="+unitName,
		"-p", "MemoryMax=512M",
		"/usr/sbin/runuser", "-u", "paasd-builder", "--",
		"git", "clone", "--depth=1", "--branch", ref,
		"--config", "http.followRedirects=false",
		req.SourceURL, buildDir,
	)
	cmd.Dir = b.workDir
	cmd.Env = []string{
		"HOME=/tmp",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"GIT_TERMINAL_PROMPT=0",
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git clone: %w", err)
	}
	b.trackUnit(req.BuildID, unitName)
	defer b.untrackUnit(req.BuildID)

	go streamLines(stdout, logCb)
	go streamLines(stderr, logCb)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	logCb("[paasd] Clone complete")
	return nil
}

func (b *Builder) nixpacksBuild(ctx context.Context, req BuildRequest, buildDir string, logCb func(string)) error {
	logCb("[paasd] Running nixpacks build...")

	buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// Run nixpacks via systemd-run with resource limits and sandboxing.
	// Nixpacks needs Docker socket access to build images (it runs docker build internally).
	// We run as paasd-builder (member of docker group) instead of root.
	unitName := fmt.Sprintf("paasd-build-%s.scope", req.BuildID[:16])
	cmd := exec.CommandContext(buildCtx,
		"systemd-run", "--scope", "--quiet",
		"--unit="+unitName,
		"-p", "MemoryMax=2G",
		"-p", "CPUQuota=200%",
		"/usr/sbin/runuser", "-u", "paasd-builder", "--",
		b.nixpacks, "build", buildDir, "--name", req.ImageTag,
	)

	cmd.Env = []string{
		"HOME=/tmp",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"NIXPACKS_NO_CACHE=1",
		"DOCKER_CONFIG=/tmp/.docker",
	}
	// Create new process group for killable tree

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start nixpacks: %w", err)
	}
	b.trackUnit(req.BuildID, unitName)
	defer b.untrackUnit(req.BuildID)
	defer b.cleanupUnit(buildCtx, unitName)

	go streamLines(stdout, logCb)
	go streamLines(stderr, logCb)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("nixpacks build failed: %w", err)
	}

	logCb("[paasd] Nixpacks build complete")
	return nil
}

func (b *Builder) pushImage(ctx context.Context, req BuildRequest, logCb func(string)) error {
	logCb("[paasd] Pushing image to local registry...")

	pushCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	unitName := fmt.Sprintf("paasd-push-%s.scope", req.BuildID[:16])
	cmd := exec.CommandContext(pushCtx,
		"systemd-run", "--scope", "--quiet",
		"--unit="+unitName,
		"docker", "push", req.ImageTag,
	)
	cmd.Env = []string{
		"HOME=/tmp",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"DOCKER_HOST=unix:///var/run/docker.sock",
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start docker push: %w", err)
	}
	b.trackUnit(req.BuildID, unitName)
	defer b.untrackUnit(req.BuildID)
	defer b.cleanupUnit(pushCtx, unitName)

	go streamLines(stdout, logCb)
	go streamLines(stderr, logCb)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("docker push failed: %w", err)
	}

	logCb("[paasd] Push complete")
	return nil
}

func streamLines(r io.Reader, logCb func(string)) {
	scanner := bufio.NewScanner(r)
	// Allow larger lines for build output
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := sanitizeLogLine(scanner.Text())
		logCb(line)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("builder: stream scan error: %v", err)
	}
}
