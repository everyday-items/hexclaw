package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openBUG20260726034A02MigrationDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "bug-20260726-034-a02.db") +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, context.Background()
}

func bug20260726034A02MigrationIndex() int {
	for i, migration := range All {
		contract := strings.ToLower(migration.Description + "\n" + migration.SQL)
		if strings.Contains(contract, "k12_textbook_manifests") {
			return i
		}
	}
	return -1
}

func bug20260726034A02TableColumns(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	table string,
) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatalf("inspect %s: %v", table, err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(
			&cid, &name, &kind, &notNull, &defaultValue, &primaryKey,
		); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		columns[name] = true
	}
	return columns
}

func bug20260726034A02RequireColumns(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	table string,
	required ...string,
) {
	t.Helper()
	columns := bug20260726034A02TableColumns(t, ctx, db, table)
	if len(columns) == 0 {
		t.Errorf("BUG-20260726-034-A02: required additive table %s is absent", table)
		return
	}
	for _, column := range required {
		if !columns[column] {
			t.Errorf("BUG-20260726-034-A02: %s.%s is absent", table, column)
		}
	}
}

func bug20260726034A02SeedKnowledgePDF(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ownerID, documentID string,
	generation int64,
) {
	t.Helper()
	corpusUID := "corpus-" + ownerID
	statements := []string{
		`INSERT OR IGNORE INTO agents(name) VALUES(?)`,
		`INSERT OR IGNORE INTO kb_semantic_corpora
		 (corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
		 VALUES(?,?,'default','general',1,1,1)`,
		`INSERT INTO kb_documents
		 (id,title,content,source,deleted,corpus_uid,created_at,updated_at)
		 VALUES(?,?,'教材正文',?,0,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO NOTHING`,
		`INSERT OR IGNORE INTO kb_semantic_document_generations
		 (owner_id,corpus_uid,document_id,content_generation,created_at)
		 VALUES(?,?,?,?,1)`,
		`INSERT OR IGNORE INTO kb_semantic_document_bindings
		 (document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,
		  version,created_at,updated_at)
		 VALUES(?,?,?,?,'active','ready',1,1,1)`,
	}
	args := [][]any{
		{ownerID},
		{corpusUID, ownerID},
		{documentID, documentID + ".pdf", "upload:" + documentID + ".pdf", corpusUID},
		{ownerID, corpusUID, documentID, generation},
		{documentID, ownerID, corpusUID, generation},
	}
	for i, statement := range statements {
		if _, err := db.ExecContext(ctx, statement, args[i]...); err != nil {
			t.Fatalf("seed knowledge PDF statement %d: %v", i, err)
		}
	}
}

func bug20260726034A02InsertManifest(
	ctx context.Context,
	db *sql.DB,
	manifestID, ownerID, documentID string,
	generation int64,
	state string,
) error {
	const catalog = `{"subject":"math","textbook_edition":"人教版","textbook_version":"2025","title":"义务教育教科书·数学五年级下册","volume":"下册","page_min":1,"page_max":100,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":20,"lessons":[{"lesson_id":"l1","title":"第1课","page_from":1,"page_to":10}]}],"page_refs":[{"logical_page":1,"pdf_page":1,"segment_refs":["segment-1"]}]}`
	_, err := db.ExecContext(ctx, `INSERT INTO k12_textbook_manifests
		(manifest_id,owner_id,document_id,document_generation,document_title,subject,
		 source_digest,state,retryable,failure_message,text_index_state,vector_index_state,
		 catalog_json,catalog_digest,created_at,updated_at)
		VALUES(?,?,?,?,?,'math',?,?,0,'','ready','ready',?,?,1,1)`,
		manifestID, ownerID, documentID, generation,
		"义务教育教科书·数学五年级下册.pdf",
		strings.Repeat("a", 64), state, catalog, strings.Repeat("b", 64))
	return err
}

func bug20260726034A02InsertBinding(
	ctx context.Context,
	db *sql.DB,
	bindingID, ownerID, agentName, manifestID, documentID string,
	generation int64,
	status string,
) error {
	_, err := db.ExecContext(ctx, `INSERT INTO k12_textbook_bindings
		(textbook_binding_id,owner_id,agent_name,subject,textbook_manifest_id,
		 document_id,document_generation,status,created_at,updated_at)
		VALUES(?,?,?,'math',?,?,?,?,1,1)`,
		bindingID, ownerID, agentName, manifestID, documentID, generation, status)
	return err
}

func TestBUG20260726034A02_TextbookMigrationIsAdditiveAndRegistered(t *testing.T) {
	index := bug20260726034A02MigrationIndex()
	if index < 0 {
		t.Fatal("BUG-20260726-034-A02: additive textbook manifest/binding migration is not registered")
	}
	db, ctx := openBUG20260726034A02MigrationDB(t)
	if err := Run(ctx, db, All[:index]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO agents(name,description) VALUES('migration-sentinel','must-survive')`,
	); err != nil {
		t.Fatalf("seed pre-migration row: %v", err)
	}
	if err := Run(ctx, db, All[index:]); err != nil {
		t.Fatalf("run additive textbook migration: %v", err)
	}
	var description string
	if err := db.QueryRowContext(ctx,
		`SELECT description FROM agents WHERE name='migration-sentinel'`,
	).Scan(&description); err != nil || description != "must-survive" {
		t.Fatalf("additive migration changed existing data: description=%q err=%v",
			description, err)
	}

	bug20260726034A02RequireColumns(t, ctx, db, "k12_textbook_manifests",
		"manifest_id", "owner_id", "document_id", "document_generation",
		"document_title", "subject", "source_digest", "state", "retryable",
		"failure_message", "text_index_state", "vector_index_state",
		"catalog_digest", "created_at", "updated_at")
	bug20260726034A02RequireColumns(t, ctx, db, "k12_textbook_manifest_segments",
		"manifest_id", "logical_page", "segment_ref", "pdf_page", "document_id",
		"document_generation")
	bug20260726034A02RequireColumns(t, ctx, db, "k12_textbook_bindings",
		"textbook_binding_id", "owner_id", "agent_name", "subject",
		"textbook_manifest_id", "document_id", "document_generation", "status",
		"created_at", "updated_at")
	bug20260726034A02RequireColumns(t, ctx, db, "k12_curriculum_progress",
		"textbook_binding_id", "textbook_manifest_id")
}

func TestBUG20260726034A02_ManifestAndBindingConstraintsAreDurable(t *testing.T) {
	db, ctx := openBUG20260726034A02MigrationDB(t)
	if err := Run(ctx, db, All); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		owner, document string
	}{
		{"mingming", "doc-math-a"},
		{"mingming", "doc-math-b"},
		{"other", "doc-other"},
	} {
		bug20260726034A02SeedKnowledgePDF(t, ctx, db, fixture.owner, fixture.document, 1)
	}
	if err := bug20260726034A02InsertManifest(
		ctx, db, "manifest-a", "mingming", "doc-math-a", 1,
		"ready_for_confirmation",
	); err != nil {
		t.Fatalf("insert canonical manifest: %v", err)
	}
	if err := bug20260726034A02InsertManifest(
		ctx, db, "manifest-a-duplicate", "mingming", "doc-math-a", 1,
		"ready_for_confirmation",
	); err == nil {
		t.Fatal("BUG-20260726-034-A02: duplicate owner/document/generation/subject manifest was accepted")
	}
	if _, err := db.ExecContext(ctx, `UPDATE k12_textbook_manifests
		SET document_generation=2 WHERE manifest_id='manifest-a'`); err == nil {
		t.Fatal("BUG-20260726-034-A02: immutable manifest generation was updated in place")
	}
	if _, err := db.ExecContext(ctx, `UPDATE k12_textbook_manifests
		SET source_digest=? WHERE manifest_id='manifest-a'`,
		strings.Repeat("c", 64)); err == nil {
		t.Fatal("BUG-20260726-034-A02: immutable manifest source digest was updated in place")
	}
	if err := bug20260726034A02InsertManifest(
		ctx, db, "manifest-invalid-state", "other", "doc-other", 1, "ready",
	); err == nil {
		t.Fatal("BUG-20260726-034-A02: manifest state outside exact-set was accepted")
	}
	if err := bug20260726034A02InsertManifest(
		ctx, db, "manifest-b", "mingming", "doc-math-b", 1,
		"ready_for_confirmation",
	); err != nil {
		t.Fatalf("insert second canonical manifest: %v", err)
	}

	if err := bug20260726034A02InsertBinding(
		ctx, db, "binding-cross-owner", "other", "other", "manifest-a",
		"doc-math-a", 1, "active",
	); err == nil {
		t.Fatal("BUG-20260726-034-A02: cross-owner binding to another owner's manifest was accepted")
	}
	if err := bug20260726034A02InsertBinding(
		ctx, db, "binding-invalid", "mingming", "mingming", "manifest-a",
		"doc-math-a", 1, "ready",
	); err == nil {
		t.Fatal("BUG-20260726-034-A02: binding status outside exact-set was accepted")
	}
	if err := bug20260726034A02InsertBinding(
		ctx, db, "binding-a", "mingming", "mingming", "manifest-a",
		"doc-math-a", 1, "active",
	); err != nil {
		t.Fatalf("insert first active binding: %v", err)
	}
	if err := bug20260726034A02InsertBinding(
		ctx, db, "binding-b", "mingming", "mingming", "manifest-b",
		"doc-math-b", 1, "active",
	); err == nil {
		t.Fatal("BUG-20260726-034-A02: second active owner/agent/subject binding was accepted")
	}
	if _, err := db.ExecContext(ctx, `UPDATE k12_textbook_bindings
		SET status='superseded',updated_at=2 WHERE textbook_binding_id='binding-a'`); err != nil {
		t.Fatalf("supersede first binding: %v", err)
	}
	if err := bug20260726034A02InsertBinding(
		ctx, db, "binding-b", "mingming", "mingming", "manifest-b",
		"doc-math-b", 1, "active",
	); err != nil {
		t.Fatalf("insert replacement active binding: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE k12_textbook_bindings
		SET textbook_manifest_id='manifest-a' WHERE textbook_binding_id='binding-b'`); err == nil {
		t.Fatal("BUG-20260726-034-A02: immutable binding manifest was updated in place")
	}
	var active int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_textbook_bindings
		WHERE owner_id='mingming' AND agent_name='mingming' AND subject='math'
		  AND status='active'`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active binding count=%d err=%v, want 1", active, err)
	}
}

func TestBUG20260726034A02_LegacyUploadBecomesCandidateWithoutAutoBinding(t *testing.T) {
	index := bug20260726034A02MigrationIndex()
	if index < 0 {
		t.Fatal("BUG-20260726-034-A02: additive textbook migration is absent")
	}
	db, ctx := openBUG20260726034A02MigrationDB(t)
	if err := Run(ctx, db, All[:index]); err != nil {
		t.Fatal(err)
	}
	bug20260726034A02SeedKnowledgePDF(t, ctx, db, "mingming", "legacy-pdf", 1)
	if err := Run(ctx, db, All[index:]); err != nil {
		t.Fatal(err)
	}
	var manifests, bindings int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_textbook_manifests
		WHERE owner_id='mingming' AND document_id='legacy-pdf'
		  AND document_generation=1 AND subject='math'`).Scan(&manifests); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_textbook_bindings
		WHERE owner_id='mingming'`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if manifests != 1 || bindings != 0 {
		t.Fatalf("BUG-20260726-034-A02: legacy upload migration manifests/bindings=%d/%d, want 1/0",
			manifests, bindings)
	}
	var state string
	if err := db.QueryRowContext(ctx, `SELECT state FROM k12_textbook_manifests
		WHERE owner_id='mingming' AND document_id='legacy-pdf'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"waiting_ingest": true, "extracting": true, "ready_for_confirmation": true,
		"failed_retryable": true, "failed_terminal": true, "stale": true,
	}
	if !allowed[state] {
		t.Fatalf("BUG-20260726-034-A02: backfilled candidate state=%q outside exact-set", state)
	}
}
