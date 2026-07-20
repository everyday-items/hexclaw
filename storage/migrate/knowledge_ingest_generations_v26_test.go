package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestKnowledgeIngestGenerationsV26KeepsImmutableSourcePerDocumentGeneration(t *testing.T) {
	db, err := sql.Open("sqlite", "file:knowledge-ingest-v26?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE kb_documents (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, []Migration{KnowledgeIndexV23, KnowledgeIngestV24}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO kb_semantic_corpora
		 (corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
		 VALUES('corpus-1','owner-1','default','general',0,1,1)`,
		`INSERT INTO kb_documents(id) VALUES('doc-1')`,
		`INSERT INTO kb_semantic_document_generations
		 (owner_id,corpus_uid,document_id,content_generation,created_at)
		 VALUES('owner-1','corpus-1','doc-1',1,1)`,
		`INSERT INTO kb_ingest_blobs
		 (owner_id,corpus_uid,sha256,storage_path,size_bytes,media_type,created_at)
		 VALUES('owner-1','corpus-1','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','/objects/a',1,'text/plain',1)`,
		`INSERT INTO kb_ingest_document_sources
		 (document_id,owner_id,corpus_uid,content_generation,blob_sha256,original_name,
		  extension,media_type,size_bytes,created_at,updated_at)
		 VALUES('doc-1','owner-1','corpus-1',1,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		        'lesson.txt','.txt','text/plain',1,1,1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := Run(ctx, db, []Migration{KnowledgeIngestGenerationsV26}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"document_id", "content_generation"} {
		var primaryKeyPosition int
		rows, err := db.QueryContext(ctx, `PRAGMA table_info(kb_ingest_document_sources)`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if name == want {
				primaryKeyPosition = pk
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if primaryKeyPosition == 0 {
			t.Fatalf("%s is not part of the V26 composite primary key", want)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES('owner-1','corpus-1','doc-1',2,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_ingest_document_sources
		(document_id,owner_id,corpus_uid,content_generation,blob_sha256,original_name,
		 extension,media_type,size_bytes,created_at,updated_at)
		VALUES('doc-1','owner-1','corpus-1',2,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		       'lesson.txt','.txt','text/plain',1,2,2)`); err != nil {
		t.Fatalf("insert second immutable source generation: %v", err)
	}
	var generations int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_document_sources
		WHERE document_id='doc-1'`).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if generations != 2 {
		t.Fatalf("source generations=%d, want migrated generation plus new generation", generations)
	}
}

func TestKnowledgeIngestGenerationsV26IsRegisteredAfterGenericPrintV25(t *testing.T) {
	if KnowledgeIngestGenerationsV26.Version != 26 {
		t.Fatalf("KnowledgeIngestGenerationsV26.Version=%d, want 26", KnowledgeIngestGenerationsV26.Version)
	}
	found := false
	for i, migration := range All {
		if migration.Version != 26 {
			continue
		}
		found = true
		if i == 0 || All[i-1].Version != 25 {
			t.Fatalf("V26 predecessor=%v, want V25", func() int {
				if i == 0 {
					return 0
				}
				return All[i-1].Version
			}())
		}
	}
	if !found {
		t.Fatal("KnowledgeIngestGenerationsV26 is not registered")
	}
}
