// Package testdb provides immutable migrated SQLite templates for K12 use-case tests.
// It lives under internal and is only imported by _test.go helpers; production code
// never links it into the HexClaw binary.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

var (
	templateOnce   sync.Once
	templateBytes  []byte
	templateErr    error
	templateBuilds atomic.Uint32
)

// Open writes a fresh standalone copy of the current migrated schema to path.
func Open(path string) (*sql.DB, error) {
	template, err := snapshot()
	if err != nil {
		return nil, fmt.Errorf("build migrated SQLite test template: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("restrict migrated SQLite test directory: %w", err)
	}
	if err := os.WriteFile(path, template, 0o600); err != nil {
		return nil, fmt.Errorf("copy migrated SQLite test template: %w", err)
	}
	// The immutable template stays in DELETE mode so it can be copied without
	// sidecars. Every opened copy still uses the production foreign-key contract;
	// DSN-level pragma also covers a replacement physical connection.
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open migrated SQLite test copy: %w", err)
	}
	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("verify migrated SQLite test foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		_ = db.Close()
		return nil, fmt.Errorf("migrated SQLite test foreign_keys=%d, want 1", foreignKeys)
	}
	return db, nil
}

// BuildCount reports how many times this process performed the full migration chain.
func BuildCount() uint32 {
	return templateBuilds.Load()
}

func snapshot() ([]byte, error) {
	templateOnce.Do(func() {
		templateBuilds.Add(1)
		templateBytes, templateErr = buildSnapshot()
	})
	return templateBytes, templateErr
}

func buildSnapshot() ([]byte, error) {
	dir, err := os.MkdirTemp("", "hexclaw-k12-migrated-testdb-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "template.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode=DELETE`).Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, err
	}
	if journalMode != "delete" {
		_ = db.Close()
		return nil, fmt.Errorf("journal mode %q cannot produce a standalone template", journalMode)
	}
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Closing all handles before reading guarantees the snapshot has no WAL/SHM dependency.
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(dbPath)
}
