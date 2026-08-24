package migrate

// KnowledgeOCRRouteReceiptsV87 为逐页视觉转写增加不可变、脱敏的执行回执。
// 回执与 page checkpoint 同事务写入；公开投影只返回路由身份和摘要。
var KnowledgeOCRRouteReceiptsV87 = Migration{
	Version:     87,
	Description: "Knowledge OCR per-page route receipts",
	SQL:         knowledgeOCRRouteReceiptsV87DDL,
}

const knowledgeOCRRouteReceiptsV87DDL = `
CREATE TABLE IF NOT EXISTS kb_ingest_page_route_receipts (
    job_id          TEXT    NOT NULL,
    page_number     INTEGER NOT NULL CHECK(page_number > 0),
    pages_total     INTEGER NOT NULL CHECK(pages_total > 0 AND page_number <= pages_total),
    provider        TEXT    NOT NULL CHECK(length(trim(provider)) BETWEEN 1 AND 256),
    model           TEXT    NOT NULL CHECK(length(trim(model)) BETWEEN 1 AND 512),
    operation       TEXT    NOT NULL CHECK(length(trim(operation)) BETWEEN 1 AND 128),
    status          TEXT    NOT NULL CHECK(status = 'succeeded'),
    source_digest   TEXT    NOT NULL CHECK(length(source_digest) = 64),
    content_digest  TEXT    NOT NULL CHECK(length(content_digest) = 64),
    fake            INTEGER NOT NULL CHECK(fake IN (0,1)),
    created_at      INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(job_id,page_number),
    FOREIGN KEY(job_id,page_number)
        REFERENCES kb_ingest_page_checkpoints(job_id,page_number) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_kb_ingest_page_route_receipts_document
ON kb_ingest_page_route_receipts(job_id,page_number,source_digest);

CREATE TRIGGER IF NOT EXISTS trg_kb_ingest_page_route_receipts_immutable
BEFORE UPDATE ON kb_ingest_page_route_receipts
BEGIN
    SELECT RAISE(ABORT, 'knowledge OCR page route receipt is immutable');
END;

-- 旧的非终态 OCR checkpoint 没有可追溯执行回执，按未完成页重新执行；
-- 已成功历史文档保持只读兼容，不伪造回执或重写既有正文。
DELETE FROM kb_ingest_page_checkpoints
WHERE extraction_mode='ocr_vlm'
  AND job_id IN (
      SELECT job_id FROM kb_knowledge_jobs
      WHERE state IN ('queued','running','retry_wait','failed')
  );

UPDATE kb_knowledge_jobs
SET pages_done=(
    SELECT COUNT(*) FROM kb_ingest_page_checkpoints AS checkpoint
    WHERE checkpoint.job_id=kb_knowledge_jobs.job_id
)
WHERE kind='ingest' AND state IN ('queued','running','retry_wait','failed')
  AND pages_total IS NOT NULL;
`
