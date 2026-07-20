package usecase_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	internaltestdb "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase/internal/testdb"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

func openMigratedTestDB(t *testing.T) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "k12.db")
	db, err := internaltestdb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigratedTestDBTemplate_BuiltOnceAndCopiesAreIsolated(t *testing.T) {
	first := openMigratedTestDB(t)
	if _, err := first.Exec(`INSERT INTO agents(name) VALUES('first-only')`); err != nil {
		t.Fatal(err)
	}

	second := openMigratedTestDB(t)
	rows, err := second.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	versions := make([]int, 0, len(migrate.All))
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) != len(migrate.All) {
		t.Fatalf("schema migration count = %d, want %d", len(versions), len(migrate.All))
	}
	for i, migration := range migrate.All {
		if versions[i] != migration.Version {
			t.Fatalf("schema migration[%d] = %d, want %d", i, versions[i], migration.Version)
		}
	}

	var leaked int
	if err := second.QueryRow(`SELECT COUNT(*) FROM agents WHERE name = 'first-only'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("template copy leaked %d rows from another test database", leaked)
	}
	var journalMode string
	if err := second.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "delete" {
		t.Fatalf("journal mode = %q, want delete so the copied database has no WAL/SHM dependency", journalMode)
	}
	var foreignKeys int
	if err := second.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1 on every reopened test database", foreignKeys)
	}
	var integrity string
	if err := second.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("SQLite integrity_check = %q, want ok", integrity)
	}
	var seq int
	var name, path string
	if err := second.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database permissions = %o, want 600", got)
	}
	if dirInfo, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	} else if got := dirInfo.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("database directory permissions = %o, want no group/other access", got)
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("standalone database unexpectedly depends on %s sidecar: %v", suffix, err)
		}
	}
	if builds := internaltestdb.BuildCount(); builds != 1 {
		t.Fatalf("template migrations = %d, want exactly 1", builds)
	}
}
