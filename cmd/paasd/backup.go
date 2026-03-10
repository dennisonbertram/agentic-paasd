package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

func runBackup(dbPath string) {
	backupDir := filepath.Join(filepath.Dir(dbPath), "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		log.Fatalf("create backup dir: %v", err)
	}

	ts := time.Now().Format("20060102-150405")

	// Backup state DB
	if err := backupFile(dbPath, filepath.Join(backupDir, fmt.Sprintf("paasd-%s.db.gz", ts))); err != nil {
		log.Fatalf("backup state db: %v", err)
	}

	// Backup metering DB if it exists
	meteringPath := dbPath[:len(dbPath)-3] + "-metering.db"
	if _, err := os.Stat(meteringPath); err == nil {
		if err := backupFile(meteringPath, filepath.Join(backupDir, fmt.Sprintf("paasd-metering-%s.db.gz", ts))); err != nil {
			log.Printf("WARNING: backup metering db: %v", err)
		}
	}

	log.Printf("backup complete: %s", backupDir)
}

func backupFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	// Write to temp file, then atomic rename to final path
	tmpDst := dst + ".tmp"
	out, err := os.Create(tmpDst)
	if err != nil {
		return fmt.Errorf("create temp %s: %w", tmpDst, err)
	}

	gz := gzip.NewWriter(out)

	n, copyErr := io.Copy(gz, in)

	// Close gzip writer first (flushes + writes footer)
	if err := gz.Close(); err != nil {
		out.Close()
		os.Remove(tmpDst)
		if copyErr != nil {
			return fmt.Errorf("compress: %w", copyErr)
		}
		return fmt.Errorf("finalize gzip: %w", err)
	}

	if copyErr != nil {
		out.Close()
		os.Remove(tmpDst)
		return fmt.Errorf("compress: %w", copyErr)
	}

	// Sync to disk for durability
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmpDst)
		return fmt.Errorf("sync: %w", err)
	}

	if err := out.Close(); err != nil {
		os.Remove(tmpDst)
		return fmt.Errorf("close: %w", err)
	}

	// Atomic rename — no partial file at final path
	if err := os.Rename(tmpDst, dst); err != nil {
		os.Remove(tmpDst)
		return fmt.Errorf("rename: %w", err)
	}

	log.Printf("backed up %s → %s (%.1f KB)", filepath.Base(src), filepath.Base(dst), float64(n)/1024)
	return nil
}
