package migrate

// KnowledgeIngestV24 adds the durable, content-addressed source boundary used
// by the asynchronous Knowledge document ingest job. It is additive: the
// original bytes live outside SQLite while this schema records the immutable
// digest/path and the owner/corpus-scoped document association.
var KnowledgeIngestV24 = Migration{
	Version:     24,
	Description: "v0.5.0 Knowledge 大文件异步摄取：持久 Blob 与 owner/corpus 文档源",
	SQL:         KnowledgeIngestV24DDL,
}

const KnowledgeIngestV24DDL = `
CREATE TABLE IF NOT EXISTS kb_ingest_blobs (
    owner_id     TEXT    NOT NULL,
    corpus_uid   TEXT    NOT NULL,
    sha256       TEXT    NOT NULL CHECK(length(sha256) = 64),
    storage_path TEXT    NOT NULL CHECK(length(trim(storage_path)) > 0),
    size_bytes   INTEGER NOT NULL CHECK(size_bytes > 0),
    media_type   TEXT    NOT NULL CHECK(length(trim(media_type)) > 0),
    created_at   INTEGER NOT NULL,
    PRIMARY KEY(owner_id, corpus_uid, sha256),
    UNIQUE(owner_id, corpus_uid, storage_path),
    FOREIGN KEY(owner_id, corpus_uid)
        REFERENCES kb_semantic_corpora(owner_id, corpus_uid) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS kb_ingest_document_sources (
    document_id        TEXT    PRIMARY KEY
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
    UNIQUE(owner_id, corpus_uid, document_id, content_generation),
    FOREIGN KEY(owner_id, corpus_uid)
        REFERENCES kb_semantic_corpora(owner_id, corpus_uid) ON DELETE CASCADE,
    FOREIGN KEY(owner_id, corpus_uid, blob_sha256)
        REFERENCES kb_ingest_blobs(owner_id, corpus_uid, sha256) ON DELETE RESTRICT,
    FOREIGN KEY(corpus_uid, document_id, content_generation)
        REFERENCES kb_semantic_document_generations(corpus_uid, document_id, content_generation)
        ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_kb_ingest_document_sources_scope
    ON kb_ingest_document_sources(owner_id, corpus_uid, document_id);
CREATE INDEX IF NOT EXISTS idx_kb_ingest_document_sources_blob
    ON kb_ingest_document_sources(owner_id, corpus_uid, blob_sha256);
`
