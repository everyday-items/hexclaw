package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ModelInvocationsFreshDDLIncludesRequestPolicySnapshot(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.ExecContext(context.Background(), K12ModelInvocationsV15DDL); err != nil {
		t.Fatalf("create fresh model invocation ledger: %v", err)
	}
	has, err := columnExists(
		context.Background(),
		db,
		"k12_model_invocations",
		"request_policy_snapshot_json",
	)
	if err != nil || !has {
		t.Fatalf("fresh ledger request_policy_snapshot_json: has=%v err=%v", has, err)
	}
}

func TestK12ModelInvocationRequestPolicyV64UpgradesLegacyLedger(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
CREATE TABLE k12_model_invocations (
    invocation_id TEXT PRIMARY KEY
);
INSERT INTO k12_model_invocations(invocation_id) VALUES ('inv-legacy');
`); err != nil {
		t.Fatalf("create legacy model invocation ledger: %v", err)
	}

	var migration *Migration
	for index := range All {
		if All[index].Version == 64 {
			migration = &All[index]
			break
		}
	}
	if migration == nil {
		t.Fatal("migration v64 is not registered in migrate.All")
	}
	if migration.Func == nil {
		t.Fatal("migration v64 must probe the legacy column before ALTER TABLE")
	}
	if err := migration.Func(ctx, db); err != nil {
		t.Fatalf("apply migration v64: %v", err)
	}

	var policyJSON string
	if err := db.QueryRowContext(
		ctx,
		`SELECT request_policy_snapshot_json
         FROM k12_model_invocations WHERE invocation_id='inv-legacy'`,
	).Scan(&policyJSON); err != nil {
		t.Fatalf("read migrated legacy row: %v", err)
	}
	if policyJSON != "" {
		t.Fatalf("legacy policy snapshot default=%q want empty", policyJSON)
	}

	if err := migration.Func(ctx, db); err != nil {
		t.Fatalf("migration v64 must be idempotent: %v", err)
	}
}
