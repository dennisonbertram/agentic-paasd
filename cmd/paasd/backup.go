package main

import (
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func runBackup(dbPath string) {
	backupDir := filepath.Join(filepath.Dir(dbPath), "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		log.Fatalf("create backup dir: %v", err)
	}

	ts := time.Now().Format("20060102-150405")

	// Backup state DB using VACUUM INTO for WAL-safe consistent snapshot
	if err := backupSQLite(dbPath, filepath.Join(backupDir, fmt.Sprintf("paasd-%s.db.gz", ts))); err != nil {
		log.Fatalf("backup state db: %v", err)
	}

	// Backup metering DB if it exists
	meteringPath := dbPath[:len(dbPath)-3] + "-metering.db"
	if _, err := os.Stat(meteringPath); err == nil {
		if err := backupSQLite(meteringPath, filepath.Join(backupDir, fmt.Sprintf("paasd-metering-%s.db.gz", ts))); err != nil {
			log.Printf("WARNING: backup metering db: %v", err)
		}
	}

	log.Printf("backup complete: %s", backupDir)
}

// backupSQLite creates a consistent backup using SQLite's VACUUM INTO,
// then gzips the result with atomic temp-file rename.
func backupSQLite(srcDB, dstGz string) error {
	// Open the source DB in read-only mode
	db, err := sql.Open("sqlite3", srcDB+"?mode=ro&_journal_mode=WAL")
	if err != nil {
		return fmt.Errorf("open %s: %w", srcDB, err)
	}
	defer db.Close()

	// Use a temp file for the VACUUM INTO target
	tmpDB := dstGz + ".vacuumtmp"
	defer os.Remove(tmpDB) // clean up intermediate file

	// VACUUM INTO creates a consistent, standalone copy of the database
	_, err = db.Exec(fmt.Sprintf("VACUUM INTO '%s'", tmpDB))
	if err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}

	// Now gzip the consistent copy with atomic write
	if err := gzipFileAtomic(tmpDB, dstGz); err != nil {
		return err
	}

	return nil
}

// gzipFileAtomic compresses src to dst using a temp+rename pattern.
func gzipFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	tmpDst := dst + ".tmp"
	out, err := os.OpenFile(tmpDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create temp %s: %w", tmpDst, err)
	}

	gz := gzip.NewWriter(out)

	_, copyErr := io.Copy(gz, in)

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

	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmpDst)
		return fmt.Errorf("sync: %w", err)
	}

	if err := out.Close(); err != nil {
		os.Remove(tmpDst)
		return fmt.Errorf("close: %w", err)
	}

	if err := os.Rename(tmpDst, dst); err != nil {
		os.Remove(tmpDst)
		return fmt.Errorf("rename: %w", err)
	}

	log.Printf("backed up %s → %s (%.1f KB)", filepath.Base(src), filepath.Base(dst), float64(info.Size())/1024)
	return nil
}
