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

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close()

	gz := gzip.NewWriter(out)
	defer gz.Close()

	n, err := io.Copy(gz, in)
	if err != nil {
		return fmt.Errorf("compress: %w", err)
	}

	log.Printf("backed up %s → %s (%.1f KB)", filepath.Base(src), filepath.Base(dst), float64(n)/1024)
	return nil
}
