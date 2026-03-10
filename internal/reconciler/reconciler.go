// Package reconciler syncs DB state with actual Docker state.
package reconciler

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"

	"github.com/paasd/paasd/internal/docker"
)

// Reconciler periodically checks DB state against Docker state.
type Reconciler struct {
	db       *sql.DB
	docker   *docker.Client
	interval time.Duration
}

// New creates a reconciler with the given interval.
func New(db *sql.DB, dockerClient *docker.Client, interval time.Duration) *Reconciler {
	return &Reconciler{
		db:       db,
		docker:   dockerClient,
		interval: interval,
	}
}

// Run starts the reconciliation loop. Blocks until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	log.Printf("reconciler: starting (interval=%s)", r.interval)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("reconciler: stopped")
			return
		case <-ticker.C:
			if err := r.reconcileOnce(ctx); err != nil {
				log.Printf("reconciler: error: %v", err)
			}
		}
	}
}

func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	// Get all paasd containers from Docker
	containers, err := r.docker.ListContainersByLabel(ctx, "paasd.tenant", "")
	if err != nil {
		return err
	}

	// Build a set of running container IDs for fast lookup
	// We need full container info, not just IDs filtered by value
	allContainers, err := r.listAllPaasdContainers(ctx)
	if err != nil {
		return err
	}
	containerSet := make(map[string]bool, len(allContainers))
	for _, id := range allContainers {
		containerSet[id] = true
	}
	_ = containers // unused, we use allContainers instead

	var checked, fixed int

	// 1. Check services marked "running" — if container gone, mark "crashed"
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, container_id, tenant_id FROM services WHERE status = 'running' AND container_id != ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type svcRecord struct{ id, containerID, tenantID string }
	var runningServices []svcRecord
	for rows.Next() {
		var s svcRecord
		if err := rows.Scan(&s.id, &s.containerID, &s.tenantID); err != nil {
			continue
		}
		runningServices = append(runningServices, s)
	}
	rows.Close()

	for _, s := range runningServices {
		checked++
		if !containerSet[s.containerID] {
			now := time.Now().Unix()
			// Increment crash count and check circuit breaker
			var crashCount int
			var lastCrashedAt sql.NullInt64
			r.db.QueryRowContext(ctx,
				`SELECT crash_count, last_crashed_at FROM services WHERE id = ?`, s.id,
			).Scan(&crashCount, &lastCrashedAt)

			crashCount++
			circuitOpen := 0
			// If 5+ crashes within 10 minutes, open circuit breaker
			if crashCount >= 5 && lastCrashedAt.Valid && (now-lastCrashedAt.Int64) < 600 {
				circuitOpen = 1
				log.Printf("reconciler: service %s circuit breaker OPEN (crash_count=%d)", s.id, crashCount)
			}

			_, err := r.db.ExecContext(ctx,
				`UPDATE services SET status = 'crashed', crash_count = ?, circuit_open = ?,
				 last_crashed_at = ?, last_error = 'container not found by reconciler',
				 updated_at = ? WHERE id = ?`,
				crashCount, circuitOpen, now, now, s.id)
			if err != nil {
				log.Printf("reconciler: failed to mark service %s as crashed: %v", s.id, err)
			} else {
				log.Printf("reconciler: service %s marked crashed (container %s not found)", s.id, s.containerID[:12])
				fixed++
			}
		}
	}

	// 2. Check services stuck in "deploying" for > 10min
	tenMinAgo := time.Now().Add(-10 * time.Minute).Unix()
	result, err := r.db.ExecContext(ctx,
		`UPDATE services SET status = 'failed', last_error = 'deploy timed out (reconciler)',
		 updated_at = ? WHERE status = 'deploying' AND updated_at < ?`,
		time.Now().Unix(), tenMinAgo)
	if err != nil {
		log.Printf("reconciler: failed to mark stale deploys: %v", err)
	} else if n, _ := result.RowsAffected(); n > 0 {
		log.Printf("reconciler: marked %d stale deploys as failed", n)
		fixed += int(n)
	}

	// 3. Check databases marked "ready" — if container gone, mark "unavailable"
	dbRows, err := r.db.QueryContext(ctx,
		`SELECT id, container_id FROM databases WHERE status = 'ready' AND container_id != ''`)
	if err != nil {
		log.Printf("reconciler: failed to query databases: %v", err)
	} else {
		defer dbRows.Close()
		type dbRecord struct{ id, containerID string }
		var readyDBs []dbRecord
		for dbRows.Next() {
			var d dbRecord
			if err := dbRows.Scan(&d.id, &d.containerID); err != nil {
				continue
			}
			readyDBs = append(readyDBs, d)
		}
		dbRows.Close()

		for _, d := range readyDBs {
			checked++
			if !containerSet[d.containerID] {
				_, err := r.db.ExecContext(ctx,
					`UPDATE databases SET status = 'unavailable', updated_at = ? WHERE id = ?`,
					time.Now().Unix(), d.id)
				if err != nil {
					log.Printf("reconciler: failed to mark database %s as unavailable: %v", d.id, err)
				} else {
					log.Printf("reconciler: database %s marked unavailable (container not found)", d.id)
					fixed++
				}
			}
		}
	}

	if checked > 0 || fixed > 0 {
		log.Printf("reconciler: checked=%d fixed=%d", checked, fixed)
	}

	return nil
}

// listAllPaasdContainers returns IDs of all containers with any paasd label.
func (r *Reconciler) listAllPaasdContainers(ctx context.Context) ([]string, error) {
	// Use docker CLI to list containers with paasd labels
	// The docker SDK ListContainersByLabel requires an exact value match,
	// so we list all containers and filter by label prefix
	ids, err := r.docker.ListContainersByLabel(ctx, "paasd.managed", "true")
	if err != nil && !strings.Contains(err.Error(), "not found") {
		// Try alternate label
		ids2, err2 := r.docker.ListContainersByLabel(ctx, "paasd.tenant", "")
		if err2 != nil {
			return nil, err
		}
		ids = ids2
	}

	// Also get database containers
	dbIDs, _ := r.docker.ListContainersByLabel(ctx, "paasd.type", "database")

	// Merge into set
	idSet := make(map[string]bool)
	for _, id := range ids {
		idSet[id] = true
	}
	for _, id := range dbIDs {
		idSet[id] = true
	}

	result := make([]string, 0, len(idSet))
	for id := range idSet {
		result = append(result, id)
	}
	return result, nil
}
