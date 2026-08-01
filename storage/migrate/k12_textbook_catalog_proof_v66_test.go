package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12TextbookCatalogProofV66IsRegisteredAndAdditive(t *testing.T) {
	var found bool
	for _, migration := range All {
		if migration.Version == 66 {
			found = true
			if migration.SQL == "" || migration.Func != nil || migration.AtomicFunc != nil {
				t.Fatalf("v66 must be one additive SQL migration: %+v", migration)
			}
		}
	}
	if !found {
		t.Fatal("REG-K12-TEXTBOOK-CATALOG: migration v66 is not registered")
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatal(err)
	}
	for table, columns := range map[string][]string{
		"k12_textbook_catalog_jobs": {
			"job_id", "manifest_id", "owner_id", "document_id",
			"document_generation", "source_digest", "state", "attempt",
			"lease_owner", "lease_epoch", "lease_expires_at", "request_digest",
			"result_digest", "last_error", "created_at", "updated_at",
		},
		"k12_textbook_page_mappings": {
			"mapping_id", "manifest_id", "logical_page", "pdf_page",
			"evidence_page", "evidence_offset_start", "evidence_offset_end",
			"evidence_digest", "method", "verification_state", "document_id",
			"document_generation", "source_digest", "created_at", "updated_at",
		},
	} {
		has, err := tableExists(ctx, db, table)
		if err != nil || !has {
			t.Fatalf("%s missing: has=%v err=%v", table, has, err)
		}
		for _, column := range columns {
			hasColumn, columnErr := columnExists(ctx, db, table, column)
			if columnErr != nil || !hasColumn {
				t.Fatalf("%s.%s missing: has=%v err=%v",
					table, column, hasColumn, columnErr)
			}
		}
	}
}

func TestK12TextbookCatalogProofV66ClearsLegacyUnprovedArtifacts(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	beforeV66 := make([]Migration, 0, len(All))
	for _, migration := range All {
		if migration.Version < 66 {
			beforeV66 = append(beforeV66, migration)
		}
	}
	if err := Run(ctx, db, beforeV66); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO kb_semantic_corpora
			(corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
			VALUES('legacy-corpus','desktop-user','default','general',1,1,1)`, nil},
		{`INSERT INTO kb_documents
			(id,title,content,source,deleted,corpus_uid,created_at,updated_at)
			VALUES('legacy-doc','Math.pdf','legacy','upload:math.pdf',0,
			'legacy-corpus',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, nil},
		{`INSERT INTO kb_semantic_document_generations
			(owner_id,corpus_uid,document_id,content_generation,created_at)
			VALUES('desktop-user','legacy-corpus','legacy-doc',1,1)`, nil},
		{`INSERT INTO kb_semantic_document_bindings
			(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,
			 text_state,version,created_at,updated_at)
			VALUES('legacy-doc','desktop-user','legacy-corpus',1,'active','ready',1,1,1)`, nil},
		{`INSERT INTO k12_textbook_manifests
			(manifest_id,owner_id,document_id,document_generation,document_title,
			 subject,source_digest,state,retryable,failure_message,text_index_state,
			 vector_index_state,catalog_json,catalog_digest,created_at,updated_at)
			VALUES('legacy-manifest','desktop-user','legacy-doc',1,'Math.pdf','math',?,
			'ready_for_confirmation',0,'','ready','ready','{"subject":"math"}',?,1,1)`,
			[]any{digest, strings.Repeat("b", 64)}},
		{`INSERT INTO k12_textbook_manifest_segments
			(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
			 document_generation,source_digest,created_at,updated_at)
			VALUES('legacy-segment','legacy-manifest',1,'legacy-chunk',1,
			'legacy-doc',1,?,1,1)`, []any{digest}},
	}
	for index, statement := range statements {
		if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed legacy statement %d: %v", index, err)
		}
	}
	if err := Run(ctx, db, All); err != nil {
		t.Fatal(err)
	}
	var state string
	var catalogPresent, segments int
	if err := db.QueryRowContext(ctx, `SELECT state,catalog_json IS NOT NULL
		FROM k12_textbook_manifests WHERE manifest_id='legacy-manifest'`).Scan(
		&state, &catalogPresent,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM k12_textbook_manifest_segments WHERE manifest_id='legacy-manifest'`).Scan(
		&segments,
	); err != nil {
		t.Fatal(err)
	}
	if state != "extracting" || catalogPresent != 0 || segments != 0 {
		t.Fatalf("legacy repair state/catalog/segments=%s/%d/%d want extracting/0/0",
			state, catalogPresent, segments)
	}
}
