package migrate

// K12TextbookPageMappingMethodsV95 增加相邻页脚锚点的确定性页映射方法。
var K12TextbookPageMappingMethodsV95 = Migration{
	Version:     95,
	Description: "K12 教材相邻页脚锚点映射方法",
	SQL:         K12TextbookPageMappingMethodsV95DDL,
}

const K12TextbookPageMappingMethodsV95DDL = `
CREATE TABLE k12_textbook_page_mappings_v95 (
    mapping_id            TEXT    PRIMARY KEY,
    manifest_id           TEXT    NOT NULL
                                  REFERENCES k12_textbook_manifests(manifest_id) ON DELETE CASCADE,
    logical_page          INTEGER NOT NULL CHECK(logical_page >= 1),
    pdf_page              INTEGER NOT NULL CHECK(pdf_page >= 1),
    evidence_page         INTEGER NOT NULL CHECK(evidence_page >= 1),
    evidence_offset_start INTEGER NOT NULL CHECK(evidence_offset_start >= 0),
    evidence_offset_end   INTEGER NOT NULL CHECK(evidence_offset_end > evidence_offset_start),
    evidence_digest       TEXT    NOT NULL CHECK(length(evidence_digest) = 64),
    method                TEXT    NOT NULL CHECK(method IN (
                                  'printed_anchor','adjacent_printed_anchors','constant_offset_inferred')),
    verification_state    TEXT    NOT NULL CHECK(verification_state = 'verified'),
    document_id           TEXT    NOT NULL REFERENCES kb_documents(id) ON DELETE RESTRICT,
    document_generation   INTEGER NOT NULL CHECK(document_generation >= 1),
    source_digest         TEXT    NOT NULL CHECK(length(trim(source_digest)) > 0),
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    UNIQUE(manifest_id,logical_page),
    UNIQUE(manifest_id,pdf_page)
);

INSERT INTO k12_textbook_page_mappings_v95 (
    mapping_id,manifest_id,logical_page,pdf_page,evidence_page,
    evidence_offset_start,evidence_offset_end,evidence_digest,method,
    verification_state,document_id,document_generation,source_digest,
    created_at,updated_at
)
SELECT mapping_id,manifest_id,logical_page,pdf_page,evidence_page,
       evidence_offset_start,evidence_offset_end,evidence_digest,method,
       verification_state,document_id,document_generation,source_digest,
       created_at,updated_at
FROM k12_textbook_page_mappings;

DROP TRIGGER IF EXISTS k12_textbook_manifest_ready_proof_guard;
DROP TRIGGER IF EXISTS k12_textbook_page_mapping_identity_guard;
DROP TRIGGER IF EXISTS k12_textbook_page_mapping_immutable;
DROP TABLE k12_textbook_page_mappings;
ALTER TABLE k12_textbook_page_mappings_v95 RENAME TO k12_textbook_page_mappings;

CREATE INDEX idx_k12_textbook_page_mappings_lookup
    ON k12_textbook_page_mappings(manifest_id,logical_page,pdf_page);

CREATE TRIGGER k12_textbook_page_mapping_identity_guard
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

CREATE TRIGGER k12_textbook_page_mapping_immutable
BEFORE UPDATE ON k12_textbook_page_mappings
BEGIN
    SELECT RAISE(ABORT, 'textbook page mapping is immutable');
END;

CREATE TRIGGER k12_textbook_manifest_ready_proof_guard
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

UPDATE k12_textbook_manifests
SET state='extracting',retryable=0,failure_message='',
    updated_at=CAST(strftime('%s','now') AS INTEGER)*1000
WHERE state='failed_terminal'
  AND (catalog_json IS NULL OR length(trim(catalog_json))=0)
  AND (catalog_digest IS NULL OR length(trim(catalog_digest))=0)
  AND NOT EXISTS (
      SELECT 1 FROM k12_textbook_page_mappings p
      WHERE p.manifest_id=k12_textbook_manifests.manifest_id
  )
  AND NOT EXISTS (
      SELECT 1 FROM k12_textbook_manifest_segments s
      WHERE s.manifest_id=k12_textbook_manifests.manifest_id
  )
  AND EXISTS (
      SELECT 1 FROM k12_textbook_catalog_jobs j
      WHERE j.manifest_id=k12_textbook_manifests.manifest_id
        AND j.state='failed_terminal'
        AND j.failure_code='catalog_transient_failure'
        AND j.extractor_contract='checkpoint-toc-footer-v2'
        AND j.result_digest=''
        AND length(j.ingest_job_id)>0
        AND length(j.source_plan_digest)=64
  );

UPDATE k12_textbook_catalog_jobs
SET state='queued',attempt=0,lease_owner='',lease_expires_at=0,
    result_digest='',last_error='',next_attempt_at=0,heartbeat_at=0,
    failure_code='',updated_at=CAST(strftime('%s','now') AS INTEGER)*1000
WHERE state='failed_terminal'
  AND failure_code='catalog_transient_failure'
  AND extractor_contract='checkpoint-toc-footer-v2'
  AND result_digest=''
  AND length(ingest_job_id)>0
  AND length(source_plan_digest)=64
  AND NOT EXISTS (
      SELECT 1 FROM k12_textbook_page_mappings p
      WHERE p.manifest_id=k12_textbook_catalog_jobs.manifest_id
  )
  AND NOT EXISTS (
      SELECT 1 FROM k12_textbook_manifest_segments s
      WHERE s.manifest_id=k12_textbook_catalog_jobs.manifest_id
  )
  AND EXISTS (
      SELECT 1 FROM k12_textbook_manifests m
      WHERE m.manifest_id=k12_textbook_catalog_jobs.manifest_id
        AND m.state='extracting'
        AND m.text_index_state='ready'
        AND (m.catalog_json IS NULL OR length(trim(m.catalog_json))=0)
        AND (m.catalog_digest IS NULL OR length(trim(m.catalog_digest))=0)
  );
`
