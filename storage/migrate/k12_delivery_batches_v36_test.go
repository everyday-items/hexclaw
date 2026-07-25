package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12DeliveryBatchesV36AddsBatchRootAndChildLink(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE agents (name TEXT PRIMARY KEY);
        CREATE TABLE k12_practice_sets (record_id TEXT PRIMARY KEY);` + K12DeliveryReceiptsV21DDL); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12DeliveryBatchesV36}); err != nil {
		t.Fatal(err)
	}
	var table string
	if err := db.QueryRow(`SELECT name FROM sqlite_master
        WHERE type='table' AND name='k12_delivery_batches'`).Scan(&table); err != nil {
		t.Fatalf("batch table missing: %v", err)
	}
	for _, column := range []string{"batch_id", "batch_ordinal"} {
		has, err := columnExists(context.Background(), db, "k12_delivery_receipts", column)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("k12_delivery_receipts.%s missing", column)
		}
	}
	has, err := columnExists(context.Background(), db, "k12_practice_sets", "delivery_batch_id")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("k12_practice_sets.delivery_batch_id missing")
	}
}

func TestK12DeliveryBatchesV36IsLatestNumberedMigration(t *testing.T) {
	if len(All) == 0 || All[len(All)-1].Version != K12DeliveryBatchesV36.Version {
		t.Fatalf("latest migration=%d want %d", All[len(All)-1].Version, K12DeliveryBatchesV36.Version)
	}
}
