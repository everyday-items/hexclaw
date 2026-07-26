package migrate

// KnowledgeIngestExecutionV46 keeps one public Knowledge document while
// freezing every root ingest attempt's executable vision route and invisible
// adaptive page plan. Terminal structured failures are immutable facts of the
// corresponding Job, not mutable text copied into a UI projection.
var KnowledgeIngestExecutionV46 = Migration{
	Version:     46,
	Description: "Knowledge 单文档自适应分段、默认视觉路由冻结与结构化失败",
	SQL:         KnowledgeIngestExecutionV46DDL,
}

const KnowledgeIngestExecutionV46DDL = `
CREATE TABLE IF NOT EXISTS kb_ingest_execution_snapshots (
    job_id                  TEXT    PRIMARY KEY
                                    REFERENCES kb_knowledge_jobs(job_id) ON DELETE CASCADE,
    provider_instance_id    TEXT    NOT NULL CHECK(length(trim(provider_instance_id)) BETWEEN 1 AND 256),
    provider_name           TEXT    NOT NULL CHECK(length(trim(provider_name)) BETWEEN 1 AND 256),
    provider_display_name   TEXT    NOT NULL CHECK(length(trim(provider_display_name)) BETWEEN 1 AND 256),
    model                   TEXT    NOT NULL CHECK(length(trim(model)) BETWEEN 1 AND 512),
    capabilities_json       TEXT    NOT NULL CHECK(json_valid(capabilities_json)),
    selection_fingerprint   TEXT    NOT NULL CHECK(length(selection_fingerprint) = 64),
    created_at              INTEGER NOT NULL CHECK(created_at > 0)
);

CREATE TRIGGER IF NOT EXISTS trg_kb_ingest_execution_snapshots_immutable
BEFORE UPDATE ON kb_ingest_execution_snapshots
BEGIN
    SELECT RAISE(ABORT, 'knowledge ingest execution snapshot is immutable');
END;

CREATE TABLE IF NOT EXISTS kb_ingest_segments (
    segment_id          TEXT    PRIMARY KEY,
    job_id              TEXT    NOT NULL
                                REFERENCES kb_knowledge_jobs(job_id) ON DELETE CASCADE,
    document_id         TEXT    NOT NULL
                                REFERENCES kb_documents(id) ON DELETE CASCADE,
    document_generation INTEGER NOT NULL CHECK(document_generation > 0),
    ordinal             INTEGER NOT NULL CHECK(ordinal > 0),
    page_start          INTEGER NOT NULL CHECK(page_start > 0),
    page_end            INTEGER NOT NULL CHECK(page_end >= page_start),
    extraction_mode     TEXT    NOT NULL CHECK(extraction_mode IN ('text','ocr_vlm')),
    state               TEXT    NOT NULL DEFAULT 'planned'
                                CHECK(state IN ('planned','processing','ready','failed','cancelled')),
    source_digest       TEXT    NOT NULL CHECK(length(source_digest) = 64),
    plan_digest         TEXT    NOT NULL CHECK(length(plan_digest) = 64),
    last_error          TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL CHECK(created_at > 0),
    updated_at          INTEGER NOT NULL CHECK(updated_at >= created_at),
    UNIQUE(job_id, ordinal),
    UNIQUE(job_id, page_start, page_end)
);

CREATE INDEX IF NOT EXISTS idx_kb_ingest_segments_ready
ON kb_ingest_segments(document_id, document_generation, page_start, page_end)
WHERE state='ready';

CREATE TRIGGER IF NOT EXISTS trg_kb_ingest_segments_identity_immutable
BEFORE UPDATE OF segment_id,job_id,document_id,document_generation,ordinal,
                 page_start,page_end,extraction_mode,source_digest,plan_digest
ON kb_ingest_segments
BEGIN
    SELECT RAISE(ABORT, 'knowledge ingest segment identity is immutable');
END;

CREATE TABLE IF NOT EXISTS kb_job_failures (
    job_id                  TEXT    PRIMARY KEY
                                    REFERENCES kb_knowledge_jobs(job_id) ON DELETE CASCADE,
    code                    TEXT    NOT NULL CHECK(length(trim(code)) BETWEEN 1 AND 128),
    message                 TEXT    NOT NULL CHECK(length(trim(message)) BETWEEN 1 AND 4096),
    affected_pages_json     TEXT    NOT NULL DEFAULT '[]' CHECK(json_valid(affected_pages_json)),
    provider_display_name   TEXT    NOT NULL DEFAULT '',
    model                   TEXT    NOT NULL DEFAULT '',
    action_code             TEXT    NOT NULL DEFAULT '',
    created_at              INTEGER NOT NULL CHECK(created_at > 0)
);

CREATE TRIGGER IF NOT EXISTS trg_kb_job_failures_immutable
BEFORE UPDATE ON kb_job_failures
BEGIN
    SELECT RAISE(ABORT, 'knowledge job failure is immutable');
END;
`
