// Package gc cleans up orphaned Docker resources.
package gc

import (
	"context"
	"database/sql"
	"log"
	"os"
	"runtime/debug"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// GC cleans up orphaned containers, volumes, images, and build dirs.
type GC struct {
	db       *sql.DB
	docker   *docker.Client
	interval time.Duration
}

// minResourceAge is the minimum time a resource must exist before GC considers
// deleting it. This prevents races with provisioning/deploy operations.
const minResourceAge = 10 * time.Minute

// New creates a garbage collector with the given interval.
func New(db *sql.DB, dockerClient *docker.Client, interval time.Duration) *GC {
	return &GC{
		db:       db,
		docker:   dockerClient,
		interval: interval,
	}
}

// Run starts the GC loop. Blocks until ctx is cancelled.
// Recovers from panics to keep the loop alive.
func (g *GC) Run(ctx context.Context) {
	log.Printf("gc: starting (interval=%s)", g.interval)
	// Delay first run by 2 minutes to let startup settle
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}

	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()

	// Run once immediately after delay
	g.safeCollect(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Printf("gc: stopped")
			return
		case <-ticker.C:
			g.safeCollect(ctx)
		}
	}
}

func (g *GC) safeCollect(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("gc: PANIC recovered: %v\n%s", rec, string(debug.Stack()))
		}
	}()
	// Per-tick timeout prevents a hung Docker call from stalling GC forever.
	tickCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := g.collectOnce(tickCtx); err != nil {
		log.Printf("gc: error: %v", err)
	}
}

func (g *GC) collectOnce(ctx context.Context) error {
	var removed int

	// 1. Orphaned containers: ah-labeled containers not in DB
	orphanedContainers, err := g.findOrphanedContainers(ctx)
	if err != nil {
		log.Printf("gc: orphaned container check failed: %v", err)
	} else {
		for _, id := range orphanedContainers {
			log.Printf("gc: removing orphaned container %s", id[:12])
			_ = g.docker.StopContainer(ctx, id)
			if err := g.docker.RemoveContainer(ctx, id); err != nil {
				log.Printf("gc: failed to remove container %s: %v", id[:12], err)
			} else {
				removed++
			}
		}
	}

	// 2. Orphaned volumes: ah-db-* volumes not in DB
	orphanedVolumes, err := g.findOrphanedVolumes(ctx)
	if err != nil {
		log.Printf("gc: orphaned volume check failed: %v", err)
	} else {
		for _, name := range orphanedVolumes {
			log.Printf("gc: removing orphaned volume %s", name)
			if err := g.docker.RemoveVolumeSafe(ctx, name); err != nil {
				log.Printf("gc: failed to remove volume %s: %v", name, err)
			} else {
				removed++
			}
		}
	}

	// 3. Old build work dirs (older than 1 hour)
	buildDirsCleaned := g.cleanOldBuildDirs("/var/lib/ah/builds", 1*time.Hour)
	removed += buildDirsCleaned

	// 4. Dangling images — prune images not referenced by any container
	pruned, pruneErr := g.docker.PruneDanglingImages(ctx)
	if pruneErr != nil {
		log.Printf("gc: image prune failed: %v", pruneErr)
	} else if pruned > 0 {
		log.Printf("gc: pruned %d dangling images", pruned)
		removed += pruned
	}

	if removed > 0 {
		log.Printf("gc: removed %d orphaned resources", removed)
	}

	return nil
}

func (g *GC) findOrphanedContainers(ctx context.Context) ([]string, error) {
	// Get all ah service containers
	svcContainers, err := g.docker.ListContainersByLabel(ctx, "ah.service", "")
	if err != nil {
		return nil, err
	}
	// Get all ah database containers
	dbContainers, err := g.docker.ListContainersByLabel(ctx, "ah.type", "database")
	if err != nil {
		return nil, err
	}

	var orphaned []string
	cutoff := time.Now().Add(-minResourceAge)

	// Check service containers
	for _, id := range svcContainers {
		// Skip containers younger than minResourceAge to avoid deploy races
		info, inspectErr := g.docker.InspectContainer(ctx, id)
		if inspectErr != nil {
			continue // can't inspect, skip (don't delete what we can't verify)
		}
		if info.CreatedAt.After(cutoff) {
			continue // too new, skip
		}

		var count int
		err := g.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM services WHERE container_id = ?`, id).Scan(&count)
		if err != nil {
			// DB error — do NOT treat as orphaned. Log and skip.
			log.Printf("gc: DB error checking container %s, skipping: %v", id[:12], err)
			continue
		}
		if count == 0 {
			orphaned = append(orphaned, id)
		}
	}

	// Check database containers
	for _, id := range dbContainers {
		info, inspectErr := g.docker.InspectContainer(ctx, id)
		if inspectErr != nil {
			continue
		}
		if info.CreatedAt.After(cutoff) {
			continue
		}

		var count int
		err := g.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM databases WHERE container_id = ?`, id).Scan(&count)
		if err != nil {
			log.Printf("gc: DB error checking database container %s, skipping: %v", id[:12], err)
			continue
		}
		if count == 0 {
			orphaned = append(orphaned, id)
		}
	}

	return orphaned, nil
}

func (g *GC) findOrphanedVolumes(ctx context.Context) ([]string, error) {
	volumes, err := g.docker.ListVolumes(ctx, "ah-db-")
	if err != nil {
		return nil, err
	}

	var orphaned []string
	for _, name := range volumes {
		// Strict prefix match — Docker name filter can be substring-based
		if !strings.HasPrefix(name, "ah-db-") {
			continue
		}

		var count int
		err := g.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM databases WHERE volume_name = ?`, name).Scan(&count)
		if err != nil {
			// DB error — do NOT treat as orphaned. Log and skip.
			log.Printf("gc: DB error checking volume %s, skipping: %v", name, err)
			continue
		}
		if count == 0 {
			orphaned = append(orphaned, name)
		}
	}

	return orphaned, nil
}

func (g *GC) cleanOldBuildDirs(basePath string, maxAge time.Duration) int {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return 0
	}

	cutoff := time.Now().Add(-maxAge)
	var removed int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		// Security: skip symlinks to prevent traversal attacks
		if info.Mode()&os.ModeSymlink != 0 {
			log.Printf("gc: skipping symlink in build dir: %s", entry.Name())
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(basePath, entry.Name())
			// Verify resolved path is still under basePath
			resolved, evalErr := filepath.EvalSymlinks(path)
			if evalErr != nil {
				log.Printf("gc: cannot resolve path %s, skipping: %v", path, evalErr)
				continue
			}
			absBase, _ := filepath.Abs(basePath)
			if !strings.HasPrefix(resolved, absBase+string(os.PathSeparator)) && resolved != absBase {
				log.Printf("gc: SECURITY: path %s resolves outside base %s, skipping", path, basePath)
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				log.Printf("gc: failed to remove build dir %s: %v", path, err)
			} else {
				log.Printf("gc: removed old build dir %s", entry.Name())
				removed++
			}
		}
	}
	return removed
}
