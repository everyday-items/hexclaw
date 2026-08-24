package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestKnowledgeOCRRouteReceiptsV87AddsImmutableLedgerAndResetsUnreceiptedWork(t *testing.T) {
	db, err := sql.Open("sqlite", "file:knowledge-ocr-route-receipts-v87?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE kb_documents (
			id TEXT PRIMARY KEY, title TEXT NOT NULL, content TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT '', deleted INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE kb_chunks (
			id TEXT PRIMARY KEY, doc_id TEXT NOT NULL, content TEXT NOT NULL,
			chunk_index INTEGER NOT NULL, embedding BLOB, created_at DATETIME
		)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := Run(ctx, db, []Migration{
		KnowledgeIndexV23, KnowledgeIngestV24, KnowledgeIngestGenerationsV26,
		KnowledgeIngestCheckpointV28, KnowledgeIngestExecutionV46,
	}); err != nil {
		t.Fatal(err)
	}
	const sourceDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const contentDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, statement := range []string{
		`INSERT INTO kb_semantic_corpora
		 (corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
		 VALUES('corpus-1','owner-1','default','general',0,1,1)`,
		`INSERT INTO kb_documents(id,title,content,source,deleted)
		 VALUES('doc-1','lesson.pdf','','upload:lesson.pdf',0)`,
		`INSERT INTO kb_semantic_document_generations
		 (owner_id,corpus_uid,document_id,content_generation,created_at)
		 VALUES('owner-1','corpus-1','doc-1',1,1)`,
		`INSERT INTO kb_knowledge_jobs
		 (job_id,kind,owner_id,corpus_uid,document_id,document_generation,idempotency_key,
		  state,stage,pages_done,pages_total,lease_owner,lease_epoch,lease_expires_at,
		  heartbeat_at,created_at,updated_at)
		 VALUES('job-running','ingest','owner-1','corpus-1','doc-1',1,'running-key',
		        'running','ocr',2,2,'worker-1',1,1000,1,1,1)`,
		`INSERT INTO kb_knowledge_jobs
		 (job_id,kind,owner_id,corpus_uid,document_id,document_generation,idempotency_key,
		  state,stage,pages_done,pages_total,finished_at,created_at,updated_at)
		 VALUES('job-succeeded','ingest','owner-1','corpus-1','doc-1',1,'succeeded-key',
		        'succeeded','publishing',1,1,1,1,1)`,
		`INSERT INTO kb_ingest_page_checkpoints
		 (job_id,page_number,pages_total,source_digest,extraction_mode,content,content_digest,
		  source_offset_start,source_offset_end,lease_epoch,created_at,updated_at)
		 VALUES('job-running',1,2,'` + sourceDigest + `','ocr_vlm','legacy OCR','` + contentDigest + `',0,0,1,1,1)`,
		`INSERT INTO kb_ingest_page_checkpoints
		 (job_id,page_number,pages_total,source_digest,extraction_mode,content,content_digest,
		  source_offset_start,source_offset_end,lease_epoch,created_at,updated_at)
		 VALUES('job-running',2,2,'` + sourceDigest + `','text','text layer','` + contentDigest + `',0,0,1,1,1)`,
		`INSERT INTO kb_ingest_page_checkpoints
		 (job_id,page_number,pages_total,source_digest,extraction_mode,content,content_digest,
		  source_offset_start,source_offset_end,lease_epoch,created_at,updated_at)
		 VALUES('job-succeeded',1,1,'` + sourceDigest + `','ocr_vlm','historical OCR','` + contentDigest + `',0,0,1,1,1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := Run(ctx, db, []Migration{KnowledgeOCRRouteReceiptsV87}); err != nil {
		t.Fatal(err)
	}
	var runningOCR, runningText, succeededOCR, pagesDone int
	if err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM kb_ingest_page_checkpoints WHERE job_id='job-running' AND extraction_mode='ocr_vlm'),
		(SELECT COUNT(*) FROM kb_ingest_page_checkpoints WHERE job_id='job-running' AND extraction_mode='text'),
		(SELECT COUNT(*) FROM kb_ingest_page_checkpoints WHERE job_id='job-succeeded' AND extraction_mode='ocr_vlm'),
		(SELECT pages_done FROM kb_knowledge_jobs WHERE job_id='job-running')`).Scan(
		&runningOCR, &runningText, &succeededOCR, &pagesDone,
	); err != nil {
		t.Fatal(err)
	}
	if runningOCR != 0 || runningText != 1 || succeededOCR != 1 || pagesDone != 1 {
		t.Fatalf("post-migration checkpoints running_ocr/text=%d/%d succeeded_ocr=%d pages_done=%d",
			runningOCR, runningText, succeededOCR, pagesDone)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_ingest_page_route_receipts
		(job_id,page_number,pages_total,provider,model,operation,status,source_digest,
		 content_digest,fake,created_at)
		VALUES('job-succeeded',1,1,'hexclaw-gpt','gpt-5.6-sol','knowledge_pdf_page_ocr',
		       'succeeded',?,?,0,1)`, sourceDigest, contentDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE kb_ingest_page_route_receipts
		SET model='changed' WHERE job_id='job-succeeded' AND page_number=1`); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("mutable OCR receipt err=%v", err)
	}
	if err := Run(ctx, db, []Migration{KnowledgeOCRRouteReceiptsV87}); err != nil {
		t.Fatalf("V87 replay: %v", err)
	}
}

func TestKnowledgeOCRRouteReceiptsV87UsesReservedVersion(t *testing.T) {
	if KnowledgeOCRRouteReceiptsV87.Version != 87 {
		t.Fatalf("KnowledgeOCRRouteReceiptsV87.Version=%d, want 87",
			KnowledgeOCRRouteReceiptsV87.Version)
	}
}
