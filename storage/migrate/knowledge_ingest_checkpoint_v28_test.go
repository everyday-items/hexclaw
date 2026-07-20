package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestKnowledgeIngestCheckpointV28AddsPageLedgerAndBackfillsChunkSourceDigest(t *testing.T) {
	db, err := sql.Open("sqlite", "file:knowledge-ingest-checkpoint-v28?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE kb_documents (
		id TEXT PRIMARY KEY, title TEXT NOT NULL, content TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT '', deleted INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE kb_chunks (
		id TEXT PRIMARY KEY, doc_id TEXT NOT NULL, content TEXT NOT NULL,
		chunk_index INTEGER NOT NULL, embedding BLOB, created_at DATETIME
	)`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, []Migration{
		KnowledgeIndexV23, KnowledgeIngestV24, KnowledgeIngestGenerationsV26,
	}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO kb_semantic_corpora
		 (corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
		 VALUES('corpus-1','owner-1','default','general',0,1,1)`,
		`INSERT INTO kb_documents(id,title,content,source,deleted)
		 VALUES('doc-1','lesson.pdf','legacy text','upload:lesson.pdf',0)`,
		`INSERT INTO kb_chunks(id,doc_id,content,chunk_index,created_at)
		 VALUES('chunk-1','doc-1','legacy chunk',0,CURRENT_TIMESTAMP)`,
		`INSERT INTO kb_semantic_document_generations
		 (owner_id,corpus_uid,document_id,content_generation,created_at)
		 VALUES('owner-1','corpus-1','doc-1',1,1)`,
		`INSERT INTO kb_semantic_document_bindings
		 (document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,version,created_at,updated_at)
		 VALUES('doc-1','owner-1','corpus-1',1,'active','ready',1,1,1)`,
		`INSERT INTO kb_ingest_blobs
		 (owner_id,corpus_uid,sha256,storage_path,size_bytes,media_type,created_at)
		 VALUES('owner-1','corpus-1','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','/objects/a',1,'application/pdf',1)`,
		`INSERT INTO kb_ingest_document_sources
		 (document_id,owner_id,corpus_uid,content_generation,blob_sha256,original_name,
		  extension,media_type,size_bytes,created_at,updated_at)
		 VALUES('doc-1','owner-1','corpus-1',1,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		        'lesson.pdf','.pdf','application/pdf',1,1,1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := Run(ctx, db, []Migration{KnowledgeIngestCheckpointV28}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{
		"page_start", "page_end", "source_digest", "source_offset_start", "source_offset_end",
	} {
		if ok, err := columnExists(ctx, db, "kb_chunks", column); err != nil || !ok {
			t.Fatalf("kb_chunks.%s missing: ok=%v err=%v", column, ok, err)
		}
	}
	var digest string
	if err := db.QueryRowContext(ctx, `SELECT source_digest FROM kb_chunks WHERE id='chunk-1'`).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("legacy chunk source_digest=%q", digest)
	}
	var table int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='kb_ingest_page_checkpoints'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != 1 {
		t.Fatal("kb_ingest_page_checkpoints missing")
	}
}

func TestKnowledgeIngestCheckpointV28IsLatest(t *testing.T) {
	if KnowledgeIngestCheckpointV28.Version != 28 {
		t.Fatalf("KnowledgeIngestCheckpointV28.Version=%d, want 28", KnowledgeIngestCheckpointV28.Version)
	}
	if len(All) == 0 || All[len(All)-1].Version != 29 {
		t.Fatalf("latest migration=%d, want 29", All[len(All)-1].Version)
	}
}
