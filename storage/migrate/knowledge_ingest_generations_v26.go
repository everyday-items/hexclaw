package migrate

// KnowledgeIngestGenerationsV26 makes the original upload source immutable per
// semantic document generation. A tombstoned document can therefore be
// revived without overwriting the bytes/binding facts retained by its prior
// ingest job.
var KnowledgeIngestGenerationsV26 = Migration{
	Version:     26,
	Description: "v0.5.0 Knowledge 摄取源按文档 generation 留痕",
	SQL:         KnowledgeIngestGenerationsV26DDL,
}

const KnowledgeIngestGenerationsV26DDL = `
CREATE TABLE kb_ingest_document_sources_v26 (
    document_id        TEXT    NOT NULL
                               REFERENCES kb_documents(id) ON DELETE CASCADE,
    owner_id           TEXT    NOT NULL,
    corpus_uid         TEXT    NOT NULL,
    content_generation INTEGER NOT NULL DEFAULT 1 CHECK(content_generation >= 1),
    blob_sha256        TEXT    NOT NULL,
    original_name      TEXT    NOT NULL CHECK(length(trim(original_name)) > 0),
    extension          TEXT    NOT NULL CHECK(length(trim(extension)) > 0),
    media_type         TEXT    NOT NULL CHECK(length(trim(media_type)) > 0),
    size_bytes         INTEGER NOT NULL CHECK(size_bytes > 0),
    agent_id           TEXT    NOT NULL DEFAULT '',
    learner_id         TEXT    NOT NULL DEFAULT '',
    subject            TEXT    NOT NULL DEFAULT '',
    grade              TEXT    NOT NULL DEFAULT '',
    page_count         INTEGER CHECK(page_count IS NULL OR page_count >= 0),
    warnings_json      TEXT    NOT NULL DEFAULT '[]',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    PRIMARY KEY(document_id, content_generation),
    UNIQUE(owner_id, corpus_uid, document_id, content_generation),
    FOREIGN KEY(owner_id, corpus_uid)
        REFERENCES kb_semantic_corpora(owner_id, corpus_uid) ON DELETE CASCADE,
    FOREIGN KEY(owner_id, corpus_uid, blob_sha256)
        REFERENCES kb_ingest_blobs(owner_id, corpus_uid, sha256) ON DELETE RESTRICT,
    FOREIGN KEY(corpus_uid, document_id, content_generation)
        REFERENCES kb_semantic_document_generations(corpus_uid, document_id, content_generation)
        ON DELETE RESTRICT
);

INSERT INTO kb_ingest_document_sources_v26 (
    document_id,owner_id,corpus_uid,content_generation,blob_sha256,original_name,
    extension,media_type,size_bytes,agent_id,learner_id,subject,grade,page_count,
    warnings_json,created_at,updated_at
)
SELECT document_id,owner_id,corpus_uid,content_generation,blob_sha256,original_name,
       extension,media_type,size_bytes,agent_id,learner_id,subject,grade,page_count,
       warnings_json,created_at,updated_at
FROM kb_ingest_document_sources;

DROP TABLE kb_ingest_document_sources;
ALTER TABLE kb_ingest_document_sources_v26 RENAME TO kb_ingest_document_sources;

CREATE INDEX idx_kb_ingest_document_sources_scope
    ON kb_ingest_document_sources(owner_id, corpus_uid, document_id, content_generation);
CREATE INDEX idx_kb_ingest_document_sources_blob
    ON kb_ingest_document_sources(owner_id, corpus_uid, blob_sha256);
`
