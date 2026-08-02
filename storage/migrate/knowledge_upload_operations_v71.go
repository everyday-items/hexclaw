package migrate

// KnowledgeUploadOperationsV71 adds a durable upload-intent ledger in front of
// the asynchronous Knowledge ingest job. The intent is created before request
// bytes are copied, then atomically bound to the immutable document/source/job
// identities when acceptance commits. Renderer recovery therefore never has
// to infer an operation from a filename or create replacement work.
var KnowledgeUploadOperationsV71 = Migration{
	Version:     71,
	Description: "Knowledge owner/corpus scoped durable upload operation projection",
	SQL: `
CREATE TABLE IF NOT EXISTS kb_upload_operations (
    operation_id       TEXT    PRIMARY KEY CHECK(length(trim(operation_id)) > 0),
    owner_id           TEXT    NOT NULL CHECK(length(trim(owner_id)) > 0),
    corpus_uid         TEXT    NOT NULL CHECK(length(trim(corpus_uid)) > 0),
    idempotency_key    TEXT    NOT NULL CHECK(length(trim(idempotency_key)) > 0),
    request_fingerprint TEXT   NOT NULL DEFAULT ''
                                CHECK(request_fingerprint = '' OR length(request_fingerprint) = 64),
    display_name       TEXT    NOT NULL CHECK(length(trim(display_name)) > 0),
    media_type         TEXT    NOT NULL CHECK(length(trim(media_type)) > 0),
    size_bytes         INTEGER NOT NULL CHECK(size_bytes >= 0),
    content_digest     TEXT
                                CHECK(content_digest IS NULL OR length(content_digest) = 64),
    document_id        TEXT,
    job_id             TEXT,
    state              TEXT    NOT NULL
                                CHECK(state IN (
                                    'receiving','pending_response','queued','running',
                                    'retry_wait','succeeded','failed','cancelled'
                                )),
    last_error         TEXT    NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    UNIQUE(owner_id, corpus_uid, idempotency_key),
    UNIQUE(job_id),
    FOREIGN KEY(owner_id, corpus_uid)
        REFERENCES kb_semantic_corpora(owner_id, corpus_uid) ON DELETE CASCADE,
    FOREIGN KEY(job_id, owner_id, corpus_uid)
        REFERENCES kb_knowledge_jobs(job_id, owner_id, corpus_uid) ON DELETE CASCADE,
    CHECK(
        (document_id IS NULL AND job_id IS NULL AND content_digest IS NULL)
        OR
        (document_id IS NOT NULL AND job_id IS NOT NULL AND content_digest IS NOT NULL)
    ),
    CHECK(updated_at >= created_at)
);
CREATE INDEX IF NOT EXISTS idx_kb_upload_operations_scope_activity
    ON kb_upload_operations(owner_id, corpus_uid, updated_at DESC, operation_id);

-- Existing root upload jobs remain recoverable after an upgrade. Their
-- operation identity is derived solely from the immutable job identity; no
-- filename lookup or title matching participates in recovery.
INSERT OR IGNORE INTO kb_upload_operations (
    operation_id,owner_id,corpus_uid,idempotency_key,request_fingerprint,
    display_name,media_type,size_bytes,content_digest,document_id,job_id,state,
    last_error,created_at,updated_at
)
SELECT 'upload_legacy_' || j.job_id,j.owner_id,j.corpus_uid,j.idempotency_key,'',
       s.original_name,s.media_type,s.size_bytes,s.blob_sha256,j.document_id,j.job_id,
       j.state,j.last_error,j.created_at,j.updated_at
FROM kb_knowledge_jobs j
JOIN kb_ingest_document_sources s
  ON s.owner_id=j.owner_id AND s.corpus_uid=j.corpus_uid
 AND s.document_id=j.document_id AND s.content_generation=j.document_generation
WHERE j.kind='ingest' AND j.parent_job_id IS NULL
  AND j.idempotency_key NOT LIKE 'document-retry|%';
`,
}
