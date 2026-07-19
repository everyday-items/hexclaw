package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ProblemAttemptsV19Migration(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := Run(context.Background(), db, All); err != nil {
		t.Fatalf("migrate through V19: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 19 {
		t.Fatalf("latest migration=%d, want at least 19", version)
	}
	var v19 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=19`).Scan(&v19); err != nil {
		t.Fatal(err)
	}
	if v19 != 1 {
		t.Fatalf("V19 migration record missing: count=%d", v19)
	}
	for _, table := range []string{"k12_problems", "k12_attempts"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing %s after V19", table)
		}
	}
}
