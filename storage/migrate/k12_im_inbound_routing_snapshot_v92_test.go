package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12IMInboundRoutingSnapshotV92AddsRequestDigestColumnIdempotently(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('k12_im_inbound_routing_snapshots') WHERE name='request_digest'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("request digest column count=%d", count)
	}
	if err := Run(ctx, db, []Migration{K12IMInboundRoutingSnapshotV92}); err != nil {
		t.Fatalf("V92 replay must be idempotent: %v", err)
	}
}
