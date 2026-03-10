package db

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	StateDB    *sql.DB
	MeteringDB *sql.DB
}

func Open(dbPath string) (*Store, error) {
	stateDB, err := openDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}

	meteringPath := strings.TrimSuffix(dbPath, ".db") + "-metering.db"
	meteringDB, err := openDB(meteringPath)
	if err != nil {
		stateDB.Close()
		return nil, fmt.Errorf("open metering db: %w", err)
	}

	store := &Store{StateDB: stateDB, MeteringDB: meteringDB}

	if err := store.runMigrations(); err != nil {
		store.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return store, nil
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	// WAL mode allows concurrent readers with a single writer.
	// Allow a small pool so reads don't block on writes.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	return db, nil
}

func (s *Store) runMigrations() error {
	// Ensure migration tracking tables exist on both databases
	for _, db := range []*sql.DB{s.StateDB, s.MeteringDB} {
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
			name TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
			return fmt.Errorf("create schema_migrations table: %w", err)
		}
	}

	entries, err := fs.ReadDir(MigrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		var target *sql.DB
		if strings.Contains(entry.Name(), "metering") {
			target = s.MeteringDB
		} else {
			target = s.StateDB
		}

		// Skip already-applied migrations
		var count int
		if err := target.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, entry.Name()).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", entry.Name(), err)
		}
		if count > 0 {
			log.Printf("migration already applied: %s", entry.Name())
			continue
		}

		data, err := fs.ReadFile(MigrationsFS, "migrations/"+entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}

		tx, err := target.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", entry.Name(), err)
		}

		// Execute the entire migration file as a single exec (SQLite3 driver
		// supports multi-statement execution). This avoids fragile semicolon
		// splitting that breaks on semicolons inside strings or triggers.
		if _, err := tx.Exec(string(data)); err != nil {
			tx.Rollback()
			return fmt.Errorf("exec migration %s: %w", entry.Name(), err)
		}

		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			entry.Name(), time.Now().Unix()); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", entry.Name(), err)
		}

		log.Printf("migration applied: %s", entry.Name())
	}

	return nil
}

func (s *Store) Close() error {
	var errs []error
	if s.StateDB != nil {
		if err := s.StateDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.MeteringDB != nil {
		if err := s.MeteringDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}
