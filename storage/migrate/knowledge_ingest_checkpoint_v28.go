package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// KnowledgeIngestCheckpointV28 adds the durable per-page extraction ledger
// and structured source coordinates used by Knowledge search/detail results.
// Page artifacts remain job-scoped: an expired worker may be replaced for the
// same job, while a new retry job cannot accidentally inherit unverified OCR.
var KnowledgeIngestCheckpointV28 = Migration{
	Version:     28,
	Description: "v0.5.0 Knowledge 逐页摄取 checkpoint 与结构化来源跨度",
	Func:        migrateKnowledgeIngestCheckpointV28,
}

func migrateKnowledgeIngestCheckpointV28(ctx context.Context, db *sql.DB) error {
	for _, column := range []struct {
		name string
		ddl  string
	}{
		{"page_start", "INTEGER"},
		{"page_end", "INTEGER"},
		{"source_digest", "TEXT NOT NULL DEFAULT ''"},
		{"source_offset_start", "INTEGER"},
		{"source_offset_end", "INTEGER"},
	} {
		has, err := columnExists(ctx, db, "kb_chunks", column.name)
		if err != nil {
			return fmt.Errorf("检查 kb_chunks.%s: %w", column.name, err)
		}
		if has {
			continue
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE kb_chunks ADD COLUMN %s %s`, column.name, column.ddl,
		)); err != nil {
			return fmt.Errorf("新增 kb_chunks.%s: %w", column.name, err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS kb_ingest_page_checkpoints (
		job_id              TEXT    NOT NULL
		                            REFERENCES kb_knowledge_jobs(job_id) ON DELETE CASCADE,
		page_number         INTEGER NOT NULL CHECK(page_number > 0),
		pages_total         INTEGER NOT NULL CHECK(pages_total > 0 AND page_number <= pages_total),
		source_digest       TEXT    NOT NULL CHECK(length(source_digest) = 64),
		extraction_mode     TEXT    NOT NULL CHECK(extraction_mode IN ('text','ocr_vlm','image','document')),
		content             TEXT    NOT NULL CHECK(length(trim(content)) > 0),
		content_digest      TEXT    NOT NULL CHECK(length(content_digest) = 64),
		source_offset_start INTEGER NOT NULL DEFAULT 0 CHECK(source_offset_start >= 0),
		source_offset_end   INTEGER NOT NULL DEFAULT 0 CHECK(source_offset_end >= source_offset_start),
		lease_epoch         INTEGER NOT NULL CHECK(lease_epoch > 0),
		created_at          INTEGER NOT NULL,
		updated_at          INTEGER NOT NULL,
		PRIMARY KEY(job_id,page_number)
	)`); err != nil {
		return fmt.Errorf("创建 Knowledge 逐页 checkpoint: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_kb_ingest_page_checkpoints_digest
		ON kb_ingest_page_checkpoints(source_digest,job_id,page_number)`); err != nil {
		return fmt.Errorf("创建 Knowledge checkpoint digest 索引: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_kb_chunks_source_span
		ON kb_chunks(doc_id,page_start,page_end)`); err != nil {
		return fmt.Errorf("创建 Knowledge chunk 来源跨度索引: %w", err)
	}
	// Legacy chunks have no reliable page coordinates, but an active uploaded
	// document does have an immutable source digest. Backfill that fact without
	// fabricating page/offset precision.
	if _, err := tx.ExecContext(ctx, `UPDATE kb_chunks
		SET source_digest=COALESCE((
			SELECT s.blob_sha256
			FROM kb_semantic_document_bindings b
			JOIN kb_ingest_document_sources s
			  ON s.document_id=b.document_id
			 AND s.corpus_uid=b.corpus_uid
			 AND s.content_generation=b.content_generation
			WHERE b.document_id=kb_chunks.doc_id
		), '')
		WHERE source_digest=''`); err != nil {
		return fmt.Errorf("回填 Knowledge chunk 来源摘要: %w", err)
	}
	return tx.Commit()
}
