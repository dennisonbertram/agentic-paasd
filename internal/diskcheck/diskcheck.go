// Package diskcheck provides disk space checks for deploy/build/create paths.
package diskcheck

import (
	"fmt"
	"log"
	"syscall"
)

// Check verifies disk space at the given path.
// Returns nil if below blockThreshold, error if at or above.
// Logs a warning if above warnThreshold but below blockThreshold.
func Check(path string, warnPct, blockPct float64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("check disk space: %w", err)
	}

	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	if totalBytes == 0 {
		return nil // can't determine, allow
	}

	usedPct := float64(totalBytes-freeBytes) / float64(totalBytes) * 100

	if usedPct >= blockPct {
		return fmt.Errorf("insufficient disk space (%.1f%% used, threshold %.0f%%)", usedPct, blockPct)
	}
	if usedPct >= warnPct {
		log.Printf("WARNING: disk usage %.1f%% exceeds warning threshold %.0f%%", usedPct, warnPct)
	}
	return nil
}
