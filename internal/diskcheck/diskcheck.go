// Package diskcheck provides disk space checks for deploy/build/create paths.
package diskcheck

import (
	"fmt"
	"log"
	"os"
	"syscall"
)

// Check verifies disk space at the given path.
// Returns nil if below blockThreshold, error if at or above.
// Logs a warning if above warnThreshold but below blockThreshold.
func Check(path string, warnPct, blockPct float64) error {
	if err := checkPath(path, warnPct, blockPct); err != nil {
		return err
	}
	return nil
}

// CheckAll verifies disk space at multiple paths, returning the first error.
// This should be used when operations affect multiple filesystems (e.g., both
// the ah data dir and Docker's storage root).
func CheckAll(paths []string, warnPct, blockPct float64) error {
	for _, path := range paths {
		// Skip paths that don't exist (e.g., Docker might use a different root)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := checkPath(path, warnPct, blockPct); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func checkPath(path string, warnPct, blockPct float64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("check disk space at %s: %w", path, err)
	}

	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	if totalBytes == 0 {
		return nil // can't determine, allow
	}

	usedPct := float64(totalBytes-freeBytes) / float64(totalBytes) * 100

	if usedPct >= blockPct {
		return fmt.Errorf("insufficient disk space at %s (%.1f%% used, threshold %.0f%%)", path, usedPct, blockPct)
	}
	if usedPct >= warnPct {
		log.Printf("WARNING: disk usage at %s: %.1f%% exceeds warning threshold %.0f%%", path, usedPct, warnPct)
	}
	return nil
}
