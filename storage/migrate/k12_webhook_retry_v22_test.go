package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

const legacyK12WebhookReceiptsV18DDL = `
CREATE TABLE k12_webhook_receipts (
    receipt_id     TEXT PRIMARY KEY,
    binding_id     TEXT NOT NULL,
    event_id       TEXT NOT NULL,
    event_type     TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    status         TEXT NOT NULL,
    reference      TEXT NOT NULL DEFAULT '',
    failure_kind   TEXT NOT NULL DEFAULT '',
    dispatch_json  TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE(binding_id, event_id)
);`

func TestK12WebhookRetryV22UpgradesV18AndDefaultsFailClosed(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, legacyK12WebhookReceiptsV18DDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_webhook_receipts
		(receipt_id,binding_id,event_id,event_type,payload_digest,status,created_at,updated_at)
		VALUES('receipt-old','binding-old','event-old','k12.submission.requested.v1','digest','failed',1,1)`); err != nil {
		t.Fatal(err)
	}

	if err := migrateK12WebhookRetryV22(ctx, db); err != nil {
		t.Fatalf("upgrade V18 to V22: %v", err)
	}
	for _, column := range []string{"retry_safe", "attempt_count"} {
		has, err := columnExists(ctx, db, "k12_webhook_receipts", column)
		if err != nil || !has {
			t.Fatalf("V22 column %s missing: has=%v err=%v", column, has, err)
		}
	}
	var retrySafe, attempts int
	if err := db.QueryRowContext(ctx, `SELECT retry_safe,attempt_count FROM k12_webhook_receipts WHERE receipt_id='receipt-old'`).Scan(&retrySafe, &attempts); err != nil {
		t.Fatal(err)
	}
	if retrySafe != 0 || attempts != 0 {
		t.Fatalf("legacy row must default fail-closed, retry_safe=%d attempts=%d", retrySafe, attempts)
	}
}

func TestK12WebhookRetryV22HalfMigrationReentry(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, legacyK12WebhookReceiptsV18DDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE k12_webhook_receipts ADD COLUMN retry_safe INTEGER NOT NULL DEFAULT 0 CHECK(retry_safe IN (0,1))`); err != nil {
		t.Fatal(err)
	}

	if err := migrateK12WebhookRetryV22(ctx, db); err != nil {
		t.Fatalf("complete half migration: %v", err)
	}
	if err := migrateK12WebhookRetryV22(ctx, db); err != nil {
		t.Fatalf("V22 re-entry must be safe: %v", err)
	}
	for _, column := range []string{"retry_safe", "attempt_count"} {
		has, err := columnExists(ctx, db, "k12_webhook_receipts", column)
		if err != nil || !has {
			t.Fatalf("V22 column %s missing after re-entry: has=%v err=%v", column, has, err)
		}
	}
}

func TestK12WebhookRetryV22HasUniqueOrderedMigrationNumber(t *testing.T) {
	seen := make(map[int]bool, len(All))
	last := 0
	found := 0
	for _, migration := range All {
		if seen[migration.Version] {
			t.Fatalf("duplicate migration version %d", migration.Version)
		}
		seen[migration.Version] = true
		if migration.Version <= last {
			t.Fatalf("migrations not strictly increasing: %d after %d", migration.Version, last)
		}
		last = migration.Version
		if migration.Version == 22 {
			found++
			if migration.Func == nil || migration.SQL != "" {
				t.Fatalf("V22 must be a probe-based Func migration: %+v", migration)
			}
		}
	}
	if found != 1 || last < 22 {
		t.Fatalf("expected exactly one ordered V22 migration, found=%d latest=%d", found, last)
	}
}
