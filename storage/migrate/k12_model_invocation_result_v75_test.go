package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ModelInvocationResultV75IsRegisteredAndBackfillsLegacyRows(t *testing.T) {
	var registered *Migration
	for index := range All {
		if All[index].Version == 75 {
			registered = &All[index]
			break
		}
	}
	if registered == nil {
		t.Fatal("migration v75 is not registered in migrate.All")
	}
	if registered.AtomicFunc == nil || registered.Func != nil || registered.SQL != "" {
		t.Fatalf("migration v75 must be one additive AtomicFunc: %+v", *registered)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.Exec(`
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			applied_at INTEGER NOT NULL
		);
		CREATE TABLE k12_model_invocations (
			invocation_id TEXT PRIMARY KEY,
			result_digest TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO k12_model_invocations(invocation_id,result_digest)
		VALUES('legacy-success','sha256:legacy')`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(ctx, db, *registered); err != nil {
		t.Fatalf("apply V75 to legacy invocation ledger: %v", err)
	}
	has, err := columnExists(ctx, db, "k12_model_invocations", "result_json")
	if err != nil || !has {
		t.Fatalf("V75 result_json column: has=%v err=%v", has, err)
	}
	var resultJSON string
	if err := db.QueryRow(`
		SELECT result_json FROM k12_model_invocations
		WHERE invocation_id='legacy-success'`).Scan(&resultJSON); err != nil {
		t.Fatal(err)
	}
	if resultJSON != "" {
		t.Fatalf("legacy invocation fabricated result payload %q", resultJSON)
	}
	if _, err := db.Exec(`
		UPDATE k12_model_invocations SET result_json='{broken'
		WHERE invocation_id='legacy-success'`); err == nil {
		t.Fatal("V75 accepted invalid non-empty result_json")
	}
	var versionCount int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM schema_migrations WHERE version=75`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 {
		t.Fatalf("V75 migration ledger count=%d, want 1", versionCount)
	}
}
