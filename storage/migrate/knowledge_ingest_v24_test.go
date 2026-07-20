package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestKnowledgeIngestV24AddsDurableBlobAndDocumentSourceTables(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE kb_documents (
		id TEXT PRIMARY KEY, title TEXT NOT NULL, content TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE kb_chunks (
		id TEXT PRIMARY KEY, doc_id TEXT NOT NULL, content TEXT NOT NULL,
		chunk_index INTEGER NOT NULL, embedding BLOB
	)`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, []Migration{KnowledgeIndexV23, KnowledgeIngestV24}); err != nil {
		t.Fatalf("apply knowledge migrations: %v", err)
	}
	for _, table := range []string{"kb_ingest_blobs", "kb_ingest_document_sources"} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s missing: count=%d err=%v", table, count, err)
		}
	}
}

func TestKnowledgeIngestV24IsRegisteredAfterSemanticIndexV23(t *testing.T) {
	if KnowledgeIngestV24.Version != 24 {
		t.Fatalf("KnowledgeIngestV24.Version = %d, want 24", KnowledgeIngestV24.Version)
	}
	if len(All) < 24 || All[23].Version != 24 {
		t.Fatalf("migration sequence does not contain V24 at index 23: %+v", All)
	}
}
