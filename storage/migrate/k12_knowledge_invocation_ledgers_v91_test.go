package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12KnowledgeInvocationLedgersV91CreatesDurableOCRAndRetrievalLedgers(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	beforeV91 := make([]Migration, 0, len(All))
	for _, migration := range All {
		if migration.Version < 91 {
			beforeV91 = append(beforeV91, migration)
		}
	}
	if err := Run(ctx, db, beforeV91); err != nil {
		t.Fatalf("migrate before V91: %v", err)
	}
	if err := Run(ctx, db, []Migration{K12KnowledgeInvocationLedgersV91}); err != nil {
		t.Fatalf("migrate V91: %v", err)
	}
	for _, table := range []string{
		"kb_ingest_page_invocations",
		"k12_grounding_retrieval_invocations",
	} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("V91 table %s count=%d", table, count)
		}
	}
	if K12KnowledgeInvocationLedgersV91.Version != 91 {
		t.Fatalf("V91 version=%d want 91", K12KnowledgeInvocationLedgersV91.Version)
	}
	if err := Run(ctx, db, []Migration{K12KnowledgeInvocationLedgersV91}); err != nil {
		t.Fatalf("V91 replay must be idempotent: %v", err)
	}
}
