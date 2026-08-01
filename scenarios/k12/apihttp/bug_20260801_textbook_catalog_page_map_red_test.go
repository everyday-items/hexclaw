package apihttp_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
)

// REG-K12-TEXTBOOK-PAGE-MAP: kb_chunks page_start/page_end are physical PDF
// coordinates. They are not evidence for a printed/logical page number.
func TestREGTextbookPageMap_ProducerNeverCopiesPhysicalPageIntoLogicalPage(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02KnowledgePDF(t, db, "mingming", "doc-physical-page", 1)
	if _, err := db.Exec(`UPDATE kb_chunks SET page_start=3,page_end=3
		WHERE doc_id='doc-physical-page'`); err != nil {
		t.Fatal(err)
	}
	h = apihttp.NewHandler(apihttp.Runtime{
		Records: deps.Records, Deps: deps,
		PrincipalMode: "local_loopback", OwnerScope: "mingming",
	})

	rec, _ := do(t, h, http.MethodGet,
		"/textbook-binding-options?agent=mingming&subject=math", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("REG-K12-TEXTBOOK-PAGE-MAP: options status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	if got := countBUG20260726034A02Rows(t, db,
		"k12_textbook_manifest_segments"); got != 0 {
		t.Fatalf("REG-K12-TEXTBOOK-PAGE-MAP: producer persisted %d unverified "+
			"logical-page rows from physical page 3; want 0", got)
	}
	var jobState, failureCode string
	if err := db.QueryRow(`SELECT j.state,j.failure_code
		FROM k12_textbook_catalog_jobs j
		JOIN k12_textbook_manifests m ON m.manifest_id=j.manifest_id
		WHERE m.document_id='doc-physical-page'`).Scan(&jobState, &failureCode); err != nil {
		t.Fatalf("REG-K12-TEXTBOOK-PAGE-MAP: catalog failure proof missing: %v", err)
	}
	if jobState != "failed_terminal" || failureCode != "source_evidence_incomplete" {
		t.Fatalf("REG-K12-TEXTBOOK-PAGE-MAP: incomplete source state/code=%s/%s",
			jobState, failureCode)
	}
}

// REG-K12-TEXTBOOK-CATALOG: terminal Knowledge facts enqueue exactly one
// durable materialization job so restart/replay cannot lose or multiply work.
func TestREGTextbookCatalog_ProducerQueuesOneDurableJobIdempotently(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	seedBUG20260726034A02KnowledgePDF(t, db, "mingming", "doc-catalog-job", 1)
	seedREGTextbookCatalogSourceEvidence(t, db, "mingming", "doc-catalog-job", 1)
	h = apihttp.NewHandler(apihttp.Runtime{
		Records: deps.Records, Deps: deps,
		PrincipalMode: "local_loopback", OwnerScope: "mingming",
	})

	for attempt := 0; attempt < 2; attempt++ {
		rec, _ := do(t, h, http.MethodGet,
			"/textbook-binding-options?agent=mingming&subject=math", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("REG-K12-TEXTBOOK-CATALOG: replay %d status=%d body=%s",
				attempt, rec.Code, rec.Body.String())
		}
	}
	var jobs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_textbook_catalog_jobs j
		JOIN k12_textbook_manifests m ON m.manifest_id=j.manifest_id
		WHERE m.document_id='doc-catalog-job' AND j.state='queued'`).Scan(&jobs); err != nil {
		t.Fatalf("REG-K12-TEXTBOOK-CATALOG: durable catalog queue missing: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("REG-K12-TEXTBOOK-CATALOG: queued jobs=%d want 1", jobs)
	}
}

func seedREGTextbookCatalogSourceEvidence(
	t *testing.T,
	db *sql.DB,
	ownerID, documentID string,
	generation int64,
) {
	t.Helper()
	const content = "第一单元教材内容"
	sourceDigest := strings.Repeat("a", 64)
	contentSum := sha256.Sum256([]byte(content))
	contentDigest := hex.EncodeToString(contentSum[:])
	corpusUID := "corpus-" + ownerID
	jobID := "ingest-" + documentID
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO kb_ingest_blobs
			(owner_id,corpus_uid,sha256,storage_path,size_bytes,media_type,created_at)
			VALUES(?,?,?,?,?,'application/pdf',1)`,
			[]any{ownerID, corpusUID, sourceDigest, "/" + documentID + ".pdf", len(content)}},
		{`INSERT INTO kb_ingest_document_sources
			(document_id,owner_id,corpus_uid,content_generation,blob_sha256,
			 original_name,extension,media_type,size_bytes,page_count,created_at,updated_at)
			VALUES(?,?,?,?,?,? ,'.pdf','application/pdf',?,1,1,1)`,
			[]any{documentID, ownerID, corpusUID, generation, sourceDigest,
				documentID + ".pdf", len(content)}},
		{`UPDATE kb_chunks SET source_offset_start=0,source_offset_end=?
			WHERE doc_id=?`, []any{len(content), documentID}},
		{`INSERT INTO kb_knowledge_jobs
			(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,
			 document_generation,target_revision_id,idempotency_key,state,stage,
			 attempt,cancel_requested,lease_owner,lease_epoch,last_error,created_at,
			 updated_at,finished_at,pages_total,pages_done)
			VALUES(?,NULL,'ingest',?,?,?,?,NULL,?,'succeeded','publishing',1,0,'',1,'',
			 1,1,1,1,1)`,
			[]any{jobID, ownerID, corpusUID, documentID, generation, jobID}},
		{`INSERT INTO kb_ingest_page_checkpoints
			(job_id,page_number,pages_total,source_digest,extraction_mode,content,
			 content_digest,source_offset_start,source_offset_end,lease_epoch,
			 created_at,updated_at)
			VALUES(?,1,1,?,'text',?,?,0,?,1,1,1)`,
			[]any{jobID, sourceDigest, content, contentDigest, len(content)}},
	}
	for index, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed catalog source evidence statement %d: %v", index, err)
		}
	}
}
