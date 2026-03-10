package db

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"

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
	return db, nil
}

func (s *Store) runMigrations() error {
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
		data, err := fs.ReadFile(MigrationsFS, "migrations/"+entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}

		var target *sql.DB
		if strings.Contains(entry.Name(), "metering") {
			target = s.MeteringDB
		} else {
			target = s.StateDB
		}

		statements := strings.Split(string(data), ";")
		for _, stmt := range statements {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := target.Exec(stmt); err != nil {
				return fmt.Errorf("exec migration %s: %w", entry.Name(), err)
			}
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
