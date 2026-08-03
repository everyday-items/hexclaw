package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ProblemSourceRecognitionV73IsRegisteredAndCreatesImmutableLedgers(t *testing.T) {
	var registered *Migration
	for index := range All {
		if All[index].Version == 73 {
			registered = &All[index]
			break
		}
	}
	if registered == nil {
		t.Fatal("migration v73 is not registered in migrate.All")
	}
	if registered.AtomicFunc == nil || registered.Func != nil || registered.SQL != "" {
		t.Fatalf("migration v73 must be one additive AtomicFunc: %+v", *registered)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("run full migration chain through V73: %v", err)
	}
	for _, table := range []string{
		"k12_problem_source_recognition_results",
		"k12_problem_source_recognition_items",
		"k12_problem_source_recognition_physical_results",
	} {
		exists, tableErr := tableExists(ctx, db, table)
		if tableErr != nil || !exists {
			t.Fatalf("V73 table %s: exists=%v err=%v", table, exists, tableErr)
		}
	}
	for _, trigger := range []string{
		"k12_problem_source_recognition_result_immutable",
		"k12_problem_source_recognition_item_immutable",
		"k12_problem_source_recognition_physical_result_immutable",
		"k12_problem_source_reconciliation_state_guard",
	} {
		var count int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM sqlite_master
			WHERE type='trigger' AND name=?`, trigger).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("V73 immutable trigger %q count=%d, want 1", trigger, count)
		}
	}
	for _, column := range []string{
		"reconciliation_owner",
		"reconciliation_epoch",
		"reconciliation_expires_at",
		"reconciliation_attempt_count",
		"next_reconcile_at",
	} {
		has, err := columnExists(
			ctx,
			db,
			"k12_problem_source_reprocess_jobs",
			column,
		)
		if err != nil || !has {
			t.Fatalf("V73 reconciliation column %s: has=%v err=%v", column, has, err)
		}
	}
	var versionCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM schema_migrations WHERE version=73`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 {
		t.Fatalf("V73 migration ledger count=%d, want 1", versionCount)
	}
}

func TestK12ProblemSourceRecognitionV73NoOpsWithoutV72Parents(t *testing.T) {
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
		)`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(ctx, db, K12ProblemSourceRecognitionV73); err != nil {
		t.Fatalf("optional V73 without K12/V72 schema: %v", err)
	}
	for _, table := range []string{
		"k12_problem_source_recognition_results",
		"k12_problem_source_recognition_items",
		"k12_problem_source_recognition_physical_results",
	} {
		exists, tableErr := tableExists(ctx, db, table)
		if tableErr != nil {
			t.Fatal(tableErr)
		}
		if exists {
			t.Fatalf("optional V73 created dangling table %s", table)
		}
	}
}
