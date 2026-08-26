package migrate

// K12KnowledgeInvocationLedgersV91 为知识库 OCR 与 K12 教材检索建立调用前声明账本。
// 账本只保存可恢复所需的身份、摘要与脱敏结果，不向外部投影原图、凭据或原始查询。
var K12KnowledgeInvocationLedgersV91 = Migration{
	Version:     91,
	Description: "K12 durable OCR and grounding retrieval invocation ledgers",
	SQL: `
CREATE TABLE IF NOT EXISTS kb_ingest_page_invocations (
    invocation_id       TEXT PRIMARY KEY,
    job_id              TEXT NOT NULL REFERENCES kb_knowledge_jobs(job_id) ON DELETE CASCADE,
    page_number         INTEGER NOT NULL CHECK(page_number > 0),
    pages_total         INTEGER NOT NULL CHECK(pages_total > 0 AND page_number <= pages_total),
    source_digest       TEXT NOT NULL CHECK(length(source_digest)=64),
    request_digest      TEXT NOT NULL CHECK(length(request_digest)=64),
    provider            TEXT NOT NULL CHECK(length(trim(provider)) > 0),
    model               TEXT NOT NULL CHECK(length(trim(model)) > 0),
    operation           TEXT NOT NULL DEFAULT 'knowledge_pdf_page_ocr',
    status              TEXT NOT NULL DEFAULT 'running'
                        CHECK(status IN ('prepared','running','succeeded','failed','outcome_unknown')),
    content             TEXT NOT NULL DEFAULT '',
    content_digest      TEXT NOT NULL DEFAULT '',
    route_receipt_json  TEXT NOT NULL DEFAULT '{}',
    lease_epoch         INTEGER NOT NULL CHECK(lease_epoch >= 0),
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    UNIQUE(job_id,page_number,source_digest,request_digest)
);
CREATE INDEX IF NOT EXISTS idx_kb_ingest_page_invocations_recovery
    ON kb_ingest_page_invocations(job_id,status,updated_at,invocation_id);

CREATE TABLE IF NOT EXISTS k12_grounding_retrieval_invocations (
    invocation_id              TEXT PRIMARY KEY,
    invocation_key              TEXT NOT NULL UNIQUE,
    owner_id                    TEXT NOT NULL,
    agent_name                  TEXT NOT NULL,
    job_id                      TEXT NOT NULL,
    problem_id                  TEXT NOT NULL,
    operation                   TEXT NOT NULL DEFAULT 'k12_grounding_retrieval',
    grounding_snapshot_digest   TEXT NOT NULL CHECK(length(grounding_snapshot_digest)=64),
    query_digest                TEXT NOT NULL CHECK(length(query_digest)=64),
    document_id                 TEXT NOT NULL DEFAULT '',
    document_generation         INTEGER NOT NULL DEFAULT 0 CHECK(document_generation >= 0),
    revision_id                 TEXT NOT NULL DEFAULT '',
    profile_config_hash         TEXT NOT NULL DEFAULT '',
    scope_digest                TEXT NOT NULL DEFAULT '',
    provider                    TEXT NOT NULL DEFAULT '',
    model                       TEXT NOT NULL DEFAULT '',
    status                      TEXT NOT NULL DEFAULT 'running'
                                CHECK(status IN ('prepared','running','succeeded','failed','outcome_unknown')),
    result_json                 TEXT NOT NULL DEFAULT '',
    query_receipt_digest        TEXT NOT NULL DEFAULT '',
    hit_set_digest              TEXT NOT NULL DEFAULT '',
    citation_set_digest         TEXT NOT NULL DEFAULT '',
    created_at                  INTEGER NOT NULL,
    updated_at                  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_k12_grounding_retrieval_invocations_recovery
    ON k12_grounding_retrieval_invocations(owner_id,agent_name,job_id,status,updated_at,invocation_id);

CREATE TABLE IF NOT EXISTS k12_im_inbound_routing_snapshots (
    receipt_id               TEXT PRIMARY KEY
                             REFERENCES k12_im_inbound_receipts(receipt_id) ON DELETE CASCADE,
    stage                    TEXT NOT NULL CHECK(stage IN ('intent','candidate')),
    snapshot_digest          TEXT NOT NULL CHECK(length(snapshot_digest)=64),
    candidates_json          TEXT NOT NULL DEFAULT '[]',
    selected_practice_set_id TEXT NOT NULL DEFAULT '',
    selection_digest         TEXT NOT NULL DEFAULT '',
    version                  INTEGER NOT NULL DEFAULT 0 CHECK(version >= 0),
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_k12_im_inbound_routing_snapshots_stage
    ON k12_im_inbound_routing_snapshots(stage,updated_at,receipt_id);
`,
}
