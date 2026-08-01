package migrate

// K12TextbookCatalogProofV66 adds the restart-durable catalog queue and the
// auditable logical-page -> physical-PDF mapping boundary. Existing manifest
// rows are retained; unverifiable ready rows and bindings are made fail-closed.
var K12TextbookCatalogProofV66 = Migration{
	Version:     66,
	Description: "P0 Knowledge/K12 owner boundary and durable textbook catalog proof page map",
	SQL:         K12TextbookCatalogProofV66DDL,
}

const K12TextbookCatalogProofV66DDL = `
CREATE TABLE IF NOT EXISTS k12_textbook_catalog_jobs (
    job_id              TEXT    PRIMARY KEY,
    manifest_id         TEXT    NOT NULL UNIQUE
                                REFERENCES k12_textbook_manifests(manifest_id) ON DELETE CASCADE,
    owner_id            TEXT    NOT NULL CHECK(length(trim(owner_id)) > 0),
    document_id         TEXT    NOT NULL REFERENCES kb_documents(id) ON DELETE RESTRICT,
    document_generation INTEGER NOT NULL CHECK(document_generation >= 1),
    source_digest       TEXT    NOT NULL CHECK(length(trim(source_digest)) > 0),
    state               TEXT    NOT NULL CHECK(state IN (
        'queued','running','retry_wait','succeeded',
        'failed_retryable','failed_terminal','cancelled'
    )),
    attempt             INTEGER NOT NULL DEFAULT 0 CHECK(attempt >= 0),
    lease_owner         TEXT    NOT NULL DEFAULT '',
    lease_epoch         INTEGER NOT NULL DEFAULT 0 CHECK(lease_epoch >= 0),
    lease_expires_at    INTEGER NOT NULL DEFAULT 0 CHECK(lease_expires_at >= 0),
    request_digest      TEXT    NOT NULL CHECK(length(trim(request_digest)) > 0),
    result_digest       TEXT    NOT NULL DEFAULT '',
    last_error          TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    CHECK(state <> 'succeeded' OR length(trim(result_digest)) > 0)
);
CREATE INDEX IF NOT EXISTS idx_k12_textbook_catalog_jobs_claim
    ON k12_textbook_catalog_jobs(state,lease_expires_at,created_at,job_id);

CREATE TABLE IF NOT EXISTS k12_textbook_page_mappings (
    mapping_id            TEXT    PRIMARY KEY,
    manifest_id           TEXT    NOT NULL
                                  REFERENCES k12_textbook_manifests(manifest_id) ON DELETE CASCADE,
    logical_page          INTEGER NOT NULL CHECK(logical_page >= 1),
    pdf_page              INTEGER NOT NULL CHECK(pdf_page >= 1),
    evidence_page         INTEGER NOT NULL CHECK(evidence_page >= 1),
    evidence_offset_start INTEGER NOT NULL CHECK(evidence_offset_start >= 0),
    evidence_offset_end   INTEGER NOT NULL CHECK(evidence_offset_end > evidence_offset_start),
    evidence_digest       TEXT    NOT NULL CHECK(length(evidence_digest) = 64),
    method                TEXT    NOT NULL CHECK(method IN ('printed_anchor','constant_offset_inferred')),
    verification_state    TEXT    NOT NULL CHECK(verification_state = 'verified'),
    document_id           TEXT    NOT NULL REFERENCES kb_documents(id) ON DELETE RESTRICT,
    document_generation   INTEGER NOT NULL CHECK(document_generation >= 1),
    source_digest         TEXT    NOT NULL CHECK(length(trim(source_digest)) > 0),
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    UNIQUE(manifest_id,logical_page),
    UNIQUE(manifest_id,pdf_page)
);
CREATE INDEX IF NOT EXISTS idx_k12_textbook_page_mappings_lookup
    ON k12_textbook_page_mappings(manifest_id,logical_page,pdf_page);

CREATE TRIGGER IF NOT EXISTS k12_textbook_catalog_job_identity_guard
BEFORE INSERT ON k12_textbook_catalog_jobs
WHEN NOT EXISTS (
    SELECT 1 FROM k12_textbook_manifests m
    WHERE m.manifest_id=NEW.manifest_id
      AND m.owner_id=NEW.owner_id
      AND m.document_id=NEW.document_id
      AND m.document_generation=NEW.document_generation
      AND m.source_digest=NEW.source_digest
)
BEGIN
    SELECT RAISE(ABORT, 'textbook catalog job identity mismatch');
END;

CREATE TRIGGER IF NOT EXISTS k12_textbook_page_mapping_identity_guard
BEFORE INSERT ON k12_textbook_page_mappings
WHEN NOT EXISTS (
    SELECT 1 FROM k12_textbook_manifests m
    WHERE m.manifest_id=NEW.manifest_id
      AND m.document_id=NEW.document_id
      AND m.document_generation=NEW.document_generation
      AND m.source_digest=NEW.source_digest
)
BEGIN
    SELECT RAISE(ABORT, 'textbook page mapping identity mismatch');
END;

CREATE TRIGGER IF NOT EXISTS k12_textbook_page_mapping_immutable
BEFORE UPDATE ON k12_textbook_page_mappings
BEGIN
    SELECT RAISE(ABORT, 'textbook page mapping is immutable');
END;

CREATE TRIGGER IF NOT EXISTS k12_textbook_manifest_ready_proof_guard
BEFORE UPDATE OF state ON k12_textbook_manifests
WHEN NEW.state='ready_for_confirmation' AND (
    NEW.catalog_json IS NULL OR length(trim(NEW.catalog_json))=0
    OR NEW.catalog_digest IS NULL OR length(trim(NEW.catalog_digest))=0
    OR NOT EXISTS (
        SELECT 1
        FROM k12_textbook_page_mappings p
        JOIN k12_textbook_manifest_segments s
          ON s.manifest_id=p.manifest_id
         AND s.logical_page=p.logical_page
         AND s.pdf_page=p.pdf_page
        WHERE p.manifest_id=NEW.manifest_id
          AND p.verification_state='verified'
          AND p.document_id=NEW.document_id
          AND p.document_generation=NEW.document_generation
          AND p.source_digest=NEW.source_digest
          AND s.document_id=NEW.document_id
          AND s.document_generation=NEW.document_generation
          AND s.source_digest=NEW.source_digest
    )
)
BEGIN
    SELECT RAISE(ABORT, 'textbook manifest ready without verified page map');
END;

-- A manifest may only belong to the exact trusted Knowledge owner. Metadata
-- such as kb_ingest_document_sources.agent_id cannot satisfy this relation.
UPDATE k12_textbook_manifests
SET state='stale',retryable=0,failure_message='',
    text_index_state='stale',vector_index_state='stale',updated_at=updated_at
WHERE state<>'stale' AND NOT EXISTS (
    SELECT 1 FROM kb_semantic_document_bindings b
    JOIN kb_documents d ON d.id=b.document_id
    WHERE b.owner_id=k12_textbook_manifests.owner_id
      AND b.document_id=k12_textbook_manifests.document_id
      AND b.content_generation=k12_textbook_manifests.document_generation
      AND b.lifecycle_state='active' AND d.deleted=0
);

DELETE FROM k12_textbook_manifest_segments
WHERE manifest_id IN (
    SELECT m.manifest_id
    FROM k12_textbook_manifests m
    WHERE m.state='ready_for_confirmation' AND NOT EXISTS (
        SELECT 1
        FROM k12_textbook_page_mappings p
        JOIN k12_textbook_manifest_segments s
          ON s.manifest_id=p.manifest_id
         AND s.logical_page=p.logical_page
         AND s.pdf_page=p.pdf_page
        WHERE p.manifest_id=m.manifest_id
          AND p.verification_state='verified'
    )
);

UPDATE k12_textbook_manifests
SET state='extracting',retryable=0,failure_message='',catalog_json=NULL,
    catalog_digest=NULL,updated_at=updated_at
WHERE state='ready_for_confirmation' AND NOT EXISTS (
    SELECT 1
    FROM k12_textbook_page_mappings p
    JOIN k12_textbook_manifest_segments s
      ON s.manifest_id=p.manifest_id
     AND s.logical_page=p.logical_page
     AND s.pdf_page=p.pdf_page
    WHERE p.manifest_id=k12_textbook_manifests.manifest_id
      AND p.verification_state='verified'
      AND p.document_id=k12_textbook_manifests.document_id
      AND p.document_generation=k12_textbook_manifests.document_generation
      AND p.source_digest=k12_textbook_manifests.source_digest
      AND s.document_id=k12_textbook_manifests.document_id
      AND s.document_generation=k12_textbook_manifests.document_generation
      AND s.source_digest=k12_textbook_manifests.source_digest
);

UPDATE k12_textbook_bindings
SET status='invalidated',updated_at=updated_at
WHERE status='active' AND NOT EXISTS (
    SELECT 1 FROM k12_textbook_manifests m
    WHERE m.manifest_id=k12_textbook_bindings.textbook_manifest_id
      AND m.owner_id=k12_textbook_bindings.owner_id
      AND m.state='ready_for_confirmation'
      AND EXISTS (
          SELECT 1
          FROM k12_textbook_page_mappings p
          JOIN k12_textbook_manifest_segments s
            ON s.manifest_id=p.manifest_id
           AND s.logical_page=p.logical_page
           AND s.pdf_page=p.pdf_page
          WHERE p.manifest_id=m.manifest_id
            AND p.verification_state='verified'
            AND p.document_id=m.document_id
            AND p.document_generation=m.document_generation
            AND p.source_digest=m.source_digest
            AND s.document_id=m.document_id
            AND s.document_generation=m.document_generation
            AND s.source_digest=m.source_digest
      )
);
`
