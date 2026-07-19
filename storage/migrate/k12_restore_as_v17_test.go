package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12RestoreAsV17CreatesDurableImmutableEvidenceTables(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"k12_restore_archives", "k12_restore_snapshots", "k12_restore_migrations", "k12_restore_journal",
		"k12_restore_asset_migrations",
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing: n=%d err=%v", table, n, err)
		}
	}
	for _, trigger := range []string{
		"k12_restore_archives_no_update", "k12_restore_archives_no_delete",
		"k12_restore_snapshots_no_update", "k12_restore_snapshots_no_delete",
		"k12_restore_journal_no_update", "k12_restore_journal_no_delete",
		"k12_restore_asset_migrations_no_update", "k12_restore_asset_migrations_no_delete",
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?`, trigger).Scan(&n); err != nil || n != 1 {
			t.Fatalf("trigger %s missing: n=%d err=%v", trigger, n, err)
		}
	}
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 17 {
		t.Fatalf("migration version=%d want >=17", version)
	}
}
