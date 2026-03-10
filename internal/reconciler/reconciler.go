// Package reconciler syncs DB state with actual Docker state.
package reconciler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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
// Recovers from panics to keep the loop alive.
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
			r.safeReconcile(ctx)
		}
	}
}

func (r *Reconciler) safeReconcile(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("reconciler: PANIC recovered: %v", rec)
		}
	}()
	if err := r.reconcileOnce(ctx); err != nil {
		log.Printf("reconciler: error: %v", err)
	}
}

func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	// Get all paasd containers from Docker using the tenant label,
	// which is set on BOTH service and database containers.
	containerIDs, err := r.docker.ListContainersByLabel(ctx, "paasd.tenant", "")
	if err != nil {
		return fmt.Errorf("list paasd containers: %w", err)
	}

	containerSet := make(map[string]bool, len(containerIDs))
	for _, id := range containerIDs {
		containerSet[id] = true
	}

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
			// Double-check with a direct inspect to avoid transient list misses
			_, inspectErr := r.docker.InspectContainer(ctx, s.containerID)
			if inspectErr == nil {
				// Container actually exists, skip
				continue
			}

			now := time.Now().Unix()
			// Atomic circuit breaker: reset crash_count if last crash is outside
			// the 10min window, otherwise increment. Open circuit at 5 crashes.
			_, err := r.db.ExecContext(ctx, `
				UPDATE services SET
					status = 'crashed',
					crash_count = CASE
						WHEN last_crashed_at IS NULL OR (? - last_crashed_at) >= 600 THEN 1
						ELSE crash_count + 1
					END,
					circuit_open = CASE
						WHEN last_crashed_at IS NOT NULL AND (? - last_crashed_at) < 600 AND crash_count + 1 >= 5 THEN 1
						ELSE circuit_open
					END,
					last_crashed_at = ?,
					last_error = 'container not found by reconciler',
					updated_at = ?
				WHERE id = ?`,
				now, now, now, now, s.id)
			if err != nil {
				log.Printf("reconciler: failed to mark service %s as crashed: %v", s.id, err)
			} else {
				// Check if circuit was opened
				var circuitOpen int
				r.db.QueryRowContext(ctx, `SELECT circuit_open FROM services WHERE id = ?`, s.id).Scan(&circuitOpen)
				if circuitOpen == 1 {
					log.Printf("reconciler: service %s circuit breaker OPEN", s.id)
				}
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
				// Double-check with inspect
				_, inspectErr := r.docker.InspectContainer(ctx, d.containerID)
				if inspectErr == nil {
					continue
				}
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
