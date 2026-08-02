package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestKnowledgeCJKFTSV2SchemaIsOwnedByCentralMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := Run(ctx, db, All); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"kb_chunks_fts_v2", "kb_search_index_metadata"} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE name=? AND type IN ('table','view')`, table,
		).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("central migration did not create %s", table)
		}
	}
	for _, trigger := range []string{
		"kb_chunks_cjk_fts_v2_dirty_insert",
		"kb_chunks_cjk_fts_v2_dirty_update",
		"kb_chunks_cjk_fts_v2_dirty_delete",
		"kb_documents_cjk_fts_v2_dirty_lifecycle",
	} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?`, trigger,
		).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", trigger, err)
		}
		if count != 1 {
			t.Fatalf("central migration did not create trigger %s", trigger)
		}
	}

	if len(All) < KnowledgeCJKFTSV2V69.Version ||
		All[KnowledgeCJKFTSV2V69.Version-1].Version != KnowledgeCJKFTSV2V69.Version {
		t.Fatalf("migration v69 is not registered at its ordered position")
	}
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("central migrations must be restart-idempotent: %v", err)
	}
}
