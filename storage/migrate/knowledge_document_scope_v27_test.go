package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestKnowledgeDocumentScopeV27BackfillsAndScopesActiveUniqueness(t *testing.T) {
	db, err := sql.Open("sqlite", "file:knowledge-document-scope-v27?mode=memory&cache=shared")
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
	if _, err := db.ExecContext(ctx, `CREATE UNIQUE INDEX idx_kb_documents_unique
		ON kb_documents(source,title) WHERE source<>''`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, []Migration{KnowledgeIndexV23}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO kb_semantic_corpora
		 (corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
		 VALUES('corpus-a','owner-a','default','general',1,1,1)`,
		`INSERT INTO kb_embedding_policies
		 (corpus_uid,selection_kind,version,updated_at)
		 VALUES('corpus-a','disabled',0,1)`,
		`INSERT INTO kb_documents(id,title,content,source,deleted)
		 VALUES('doc-a','lesson.txt','alpha','upload:lesson.txt',0)`,
		`INSERT INTO kb_semantic_document_bindings
		 (document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,version,created_at,updated_at)
		 VALUES('doc-a','owner-a','corpus-a',1,'active','ready',1,1,1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := Run(ctx, db, []Migration{KnowledgeDocumentScopeV27}); err != nil {
		t.Fatal(err)
	}

	var corpusUID string
	if err := db.QueryRowContext(ctx, `SELECT corpus_uid FROM kb_documents WHERE id='doc-a'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	if corpusUID != "corpus-a" {
		t.Fatalf("backfilled corpus_uid=%q, want corpus-a", corpusUID)
	}
	var oldIndex int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='index' AND name='idx_kb_documents_unique'`).Scan(&oldIndex); err != nil {
		t.Fatal(err)
	}
	if oldIndex != 0 {
		t.Fatal("global source/title unique index still exists")
	}

	// Same filename/source is valid in a different corpus.
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents
		(id,title,content,source,deleted,corpus_uid)
		VALUES('doc-b','lesson.txt','beta','upload:lesson.txt',0,'corpus-b')`); err != nil {
		t.Fatalf("cross-corpus duplicate rejected: %v", err)
	}
	// A second active generation in the same corpus must fail closed.
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents
		(id,title,content,source,deleted,corpus_uid)
		VALUES('doc-a-duplicate','lesson.txt','duplicate','upload:lesson.txt',0,'corpus-a')`); err == nil {
		t.Fatal("same-corpus active duplicate unexpectedly succeeded")
	}
	// Tombstoning releases the active-name slot for a later logical row.
	if _, err := db.ExecContext(ctx, `UPDATE kb_documents SET deleted=1 WHERE id='doc-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents
		(id,title,content,source,deleted,corpus_uid)
		VALUES('doc-a-next','lesson.txt','next','upload:lesson.txt',0,'corpus-a')`); err != nil {
		t.Fatalf("active row after tombstone rejected: %v", err)
	}

	// Unbound legacy NULL rows retain their own active uniqueness boundary.
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents
		(id,title,content,source,deleted,corpus_uid)
		VALUES('legacy-a','legacy.txt','legacy','upload:legacy.txt',0,NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents
		(id,title,content,source,deleted,corpus_uid)
		VALUES('legacy-b','legacy.txt','legacy','upload:legacy.txt',0,NULL)`); err == nil {
		t.Fatal("active legacy duplicate unexpectedly succeeded")
	}
}

func TestKnowledgeDocumentScopeV27IsRegisteredAfterV26(t *testing.T) {
	if KnowledgeDocumentScopeV27.Version != 27 {
		t.Fatalf("KnowledgeDocumentScopeV27.Version=%d, want 27", KnowledgeDocumentScopeV27.Version)
	}
	found := false
	for index, migration := range All {
		if migration.Version != 27 {
			continue
		}
		found = true
		if index == 0 || All[index-1].Version != 26 {
			t.Fatalf("V27 predecessor is not V26")
		}
	}
	if !found {
		t.Fatal("KnowledgeDocumentScopeV27 is not registered")
	}
}
