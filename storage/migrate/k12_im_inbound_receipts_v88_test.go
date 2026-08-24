package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12IMInboundReceiptsV88CreatesIndependentReceiptAssetAndDispatchRoots(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Run(ctx, db, []Migration{All[0], K12IMInboundReceiptsV88}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"k12_im_inbound_receipts", "k12_im_inbound_assets", "k12_im_inbound_dispatches",
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("migration V88 table %s count=%d", table, count)
		}
	}
	if K12IMInboundReceiptsV88.Version != 88 {
		t.Fatalf("K12IMInboundReceiptsV88.Version=%d want 88", K12IMInboundReceiptsV88.Version)
	}
	if err := Run(ctx, db, []Migration{K12IMInboundReceiptsV88}); err != nil {
		t.Fatalf("V88 replay must be idempotent: %v", err)
	}
}
