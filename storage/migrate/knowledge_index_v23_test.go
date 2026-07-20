package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestKnowledgeIndexV23IsAdditiveAndRejectsInvalidPolicyShapes(t *testing.T) {
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
	if err := Run(ctx, db, []Migration{KnowledgeIndexV23}); err != nil {
		t.Fatalf("apply semantic index migration: %v", err)
	}

	wantTables := []string{
		"kb_semantic_corpora",
		"kb_embedding_policies",
		"kb_embedding_profile_snapshots",
		"kb_index_revisions",
		"kb_semantic_document_bindings",
		"kb_semantic_document_generations",
		"kb_revision_documents",
		"kb_revision_vectors",
		"kb_knowledge_jobs",
		"kb_job_stage_checkpoints",
		"kb_embedding_batch_manifests",
	}
	for _, table := range wantTables {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing: count=%d err=%v", table, n, err)
		}
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO kb_semantic_corpora
		(corpus_uid,owner_id,corpus_alias,content_version,created_at,updated_at)
		VALUES('corpus-1','owner-1','default',0,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_embedding_policies
		(corpus_uid,selection_kind,selected_profile_id,version,updated_at)
		VALUES('corpus-1','auto','must-not-be-present',0,1)`); err == nil {
		t.Fatal("auto policy carrying selected_profile_id must violate the DB tagged-union check")
	}

	// Historical revision rows must remain valid when the mutable binding moves
	// to a newer content generation. The immutable generation table is their FK
	// parent; pointing the FK at the current binding would make Replace fail.
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents(id,title,content)
		VALUES('doc-1','Doc','v1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_semantic_document_bindings
		(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,
		 version,created_at,updated_at)
		VALUES('doc-1','owner-1','corpus-1',1,'active','ready',0,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES('owner-1','corpus-1','doc-1',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_embedding_profile_snapshots
		(profile_snapshot_id,corpus_uid,resolved_profile_id,provider_id,provider_name,
		 provider_location,model_name,dimension,normalization,chunk_config_hash,
		 profile_config_hash,availability,created_at)
		VALUES('snapshot-1','corpus-1','profile-1','local','Local','local','model-1',3,
		 'l2','chunk-v1','profile-v1','installed',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_index_revisions
		(revision_id,corpus_uid,profile_snapshot_id,policy_version,previous_selection_kind,
		 base_content_version,publish_state,created_at,updated_at)
		VALUES('revision-1','corpus-1','snapshot-1',0,'disabled',0,'staged',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_revision_documents
		(revision_id,corpus_uid,document_id,content_generation,vector_state,expected_chunks,updated_at)
		VALUES('revision-1','corpus-1','doc-1',1,'pending',0,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES('owner-1','corpus-1','doc-1',2,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE kb_semantic_document_bindings
		SET content_generation=2,version=version+1,updated_at=2 WHERE document_id='doc-1'`); err != nil {
		t.Fatalf("advance current generation with historical revision present: %v", err)
	}
	var generations, historical int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_semantic_document_generations
		WHERE corpus_uid='corpus-1' AND document_id='doc-1'`).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE revision_id='revision-1' AND document_id='doc-1' AND content_generation=1`).Scan(&historical); err != nil {
		t.Fatal(err)
	}
	if generations != 2 || historical != 1 {
		t.Fatalf("generation history lost: generations=%d historical manifests=%d", generations, historical)
	}

	// V23 is additive: the legacy bare embedding column remains untouched for
	// rollback, but the new revision-vector schema must not add metadata to it.
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(kb_chunks)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	for _, forbidden := range []string{"revision_id", "profile_config_hash", "model_name", "dimension"} {
		if columns[forbidden] {
			t.Fatalf("legacy kb_chunks unexpectedly gained %s; vectors must live in kb_revision_vectors", forbidden)
		}
	}
}

func TestKnowledgeIndexV23IsRegisteredAfterV22(t *testing.T) {
	if KnowledgeIndexV23.Version != 23 {
		t.Fatalf("KnowledgeIndexV23.Version = %d, want 23", KnowledgeIndexV23.Version)
	}
	if len(All) < 24 || All[22].Version != 23 || All[23].Version != 24 {
		t.Fatalf("migration sequence at v23 = %+v, want v23 followed by v24", All[22:24])
	}
}
