package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12WebhooksV18CreatesFormalSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrateK12WebhooksV18(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"k12_webhook_bindings", "k12_webhook_receipts", "k12_webhook_nonces", "k12_webhook_audit",
	} {
		exists, err := tableExists(context.Background(), db, table)
		if err != nil || !exists {
			t.Fatalf("table %s exists=%v err=%v", table, exists, err)
		}
	}
	hasDispatch, err := columnExists(context.Background(), db, "k12_webhook_receipts", "dispatch_json")
	if err != nil || !hasDispatch {
		t.Fatalf("durable dispatch column exists=%v err=%v", hasDispatch, err)
	}
}

func TestK12WebhooksV18UpgradesPreMigrationReceiptTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE k12_webhook_receipts (
      receipt_id TEXT PRIMARY KEY,
      binding_id TEXT NOT NULL,
      event_id TEXT NOT NULL,
      event_type TEXT NOT NULL,
      payload_digest TEXT NOT NULL,
      status TEXT NOT NULL,
      reference TEXT NOT NULL DEFAULT '',
      failure_kind TEXT NOT NULL DEFAULT '',
      created_at INTEGER NOT NULL,
      updated_at INTEGER NOT NULL,
      UNIQUE(binding_id,event_id)
    )`); err != nil {
		t.Fatal(err)
	}
	if err := migrateK12WebhooksV18(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	hasDispatch, err := columnExists(context.Background(), db, "k12_webhook_receipts", "dispatch_json")
	if err != nil || !hasDispatch {
		t.Fatalf("legacy Receipt was not upgraded: exists=%v err=%v", hasDispatch, err)
	}
	// Re-entry must be a no-op rather than a duplicate-column failure.
	if err := migrateK12WebhooksV18(context.Background(), db); err != nil {
		t.Fatalf("V18 is not re-entrant: %v", err)
	}
}

func TestK12WebhooksIsNumberedV18(t *testing.T) {
	for _, migration := range All {
		if migration.Version == 18 {
			if migration.Func == nil || migration.SQL != "" {
				t.Fatalf("V18 must use the schema-probing migration function: %+v", migration)
			}
			return
		}
	}
	t.Fatal("formal K12 webhook migration V18 missing from All")
}
