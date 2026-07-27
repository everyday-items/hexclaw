package migrate

// K12TextbookBindingV54 adds the immutable textbook-manifest boundary and the
// server-owned active binding used by the K12 profile bundle. Existing uploads
// become confirmation candidates only; this migration never auto-binds one to
// a learner profile.
var K12TextbookBindingV54 = Migration{
	Version:     54,
	Description: "BUG-20260726-034-A02: k12_textbook_manifests immutable generations and server-owned active bindings",
	SQL:         K12TextbookBindingV54DDL,
}

const K12TextbookBindingV54DDL = `
CREATE TABLE IF NOT EXISTS k12_textbook_manifests (
    manifest_id          TEXT    PRIMARY KEY,
    owner_id             TEXT    NOT NULL CHECK(length(trim(owner_id)) > 0),
    document_id          TEXT    NOT NULL REFERENCES kb_documents(id) ON DELETE RESTRICT,
    document_generation  INTEGER NOT NULL CHECK(document_generation >= 1),
    document_title       TEXT    NOT NULL CHECK(length(trim(document_title)) > 0),
    subject              TEXT    NOT NULL CHECK(subject = 'math'),
    source_digest        TEXT    NOT NULL CHECK(length(trim(source_digest)) > 0),
    state                TEXT    NOT NULL CHECK(state IN (
        'waiting_ingest','extracting','ready_for_confirmation',
        'failed_retryable','failed_terminal','stale'
    )),
    retryable            INTEGER NOT NULL DEFAULT 0 CHECK(retryable IN (0,1)),
    failure_message      TEXT    NOT NULL DEFAULT '',
    text_index_state     TEXT    NOT NULL DEFAULT 'pending' CHECK(text_index_state IN (
        'pending','building','ready','failed','stale'
    )),
    vector_index_state   TEXT    NOT NULL DEFAULT 'pending' CHECK(vector_index_state IN (
        'pending','building','ready','failed','stale'
    )),
    catalog_json         TEXT,
    catalog_digest       TEXT,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    UNIQUE(owner_id,document_id,document_generation,subject),
    CHECK(
        (state = 'failed_retryable' AND retryable = 1)
        OR
        (state <> 'failed_retryable' AND retryable = 0)
    )
);
CREATE INDEX IF NOT EXISTS idx_k12_textbook_manifests_owner
    ON k12_textbook_manifests(owner_id,subject,state,updated_at);

CREATE TABLE IF NOT EXISTS k12_textbook_manifest_segments (
    segment_id           TEXT    PRIMARY KEY,
    manifest_id          TEXT    NOT NULL
                                REFERENCES k12_textbook_manifests(manifest_id) ON DELETE CASCADE,
    logical_page         INTEGER NOT NULL CHECK(logical_page >= 1),
    segment_ref          TEXT    NOT NULL CHECK(length(trim(segment_ref)) > 0),
    pdf_page             INTEGER NOT NULL CHECK(pdf_page >= 1),
    document_id          TEXT    NOT NULL REFERENCES kb_documents(id) ON DELETE RESTRICT,
    document_generation  INTEGER NOT NULL CHECK(document_generation >= 1),
    source_digest        TEXT    NOT NULL CHECK(length(trim(source_digest)) > 0),
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    UNIQUE(manifest_id,logical_page,segment_ref)
);
CREATE INDEX IF NOT EXISTS idx_k12_textbook_manifest_segments_lookup
    ON k12_textbook_manifest_segments(manifest_id,logical_page,segment_ref);

CREATE TABLE IF NOT EXISTS k12_textbook_bindings (
    textbook_binding_id  TEXT    PRIMARY KEY,
    owner_id             TEXT    NOT NULL CHECK(length(trim(owner_id)) > 0),
    agent_name           TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    subject              TEXT    NOT NULL CHECK(subject = 'math'),
    textbook_manifest_id TEXT    NOT NULL
                                REFERENCES k12_textbook_manifests(manifest_id) ON DELETE RESTRICT,
    document_id          TEXT    NOT NULL REFERENCES kb_documents(id) ON DELETE RESTRICT,
    document_generation  INTEGER NOT NULL CHECK(document_generation >= 1),
    status               TEXT    NOT NULL CHECK(status IN ('active','superseded','invalidated')),
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_textbook_bindings_one_active
    ON k12_textbook_bindings(owner_id,agent_name,subject) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_k12_textbook_bindings_manifest
    ON k12_textbook_bindings(textbook_manifest_id,status);

ALTER TABLE k12_curriculum_progress
    ADD COLUMN textbook_manifest_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_k12_curriculum_progress_manifest
    ON k12_curriculum_progress(textbook_manifest_id);

CREATE TRIGGER IF NOT EXISTS k12_textbook_manifest_identity_immutable
BEFORE UPDATE OF owner_id,document_id,document_generation,document_title,subject,source_digest
ON k12_textbook_manifests
BEGIN
    SELECT RAISE(ABORT, 'textbook manifest identity is immutable');
END;

CREATE TRIGGER IF NOT EXISTS k12_textbook_manifest_segment_identity_guard
BEFORE INSERT ON k12_textbook_manifest_segments
WHEN NOT EXISTS (
    SELECT 1 FROM k12_textbook_manifests m
    WHERE m.manifest_id=NEW.manifest_id
      AND m.document_id=NEW.document_id
      AND m.document_generation=NEW.document_generation
      AND m.source_digest=NEW.source_digest
)
BEGIN
    SELECT RAISE(ABORT, 'textbook manifest segment identity mismatch');
END;

CREATE TRIGGER IF NOT EXISTS k12_textbook_binding_identity_guard
BEFORE INSERT ON k12_textbook_bindings
WHEN NOT EXISTS (
    SELECT 1 FROM k12_textbook_manifests m
    WHERE m.manifest_id=NEW.textbook_manifest_id
      AND m.owner_id=NEW.owner_id
      AND m.subject=NEW.subject
      AND m.document_id=NEW.document_id
      AND m.document_generation=NEW.document_generation
)
BEGIN
    SELECT RAISE(ABORT, 'textbook binding manifest identity mismatch');
END;

CREATE TRIGGER IF NOT EXISTS k12_textbook_binding_identity_immutable
BEFORE UPDATE OF
    owner_id,agent_name,subject,textbook_manifest_id,document_id,document_generation
ON k12_textbook_bindings
BEGIN
    SELECT RAISE(ABORT, 'textbook binding identity is immutable');
END;

INSERT OR IGNORE INTO k12_textbook_manifests (
    manifest_id,owner_id,document_id,document_generation,document_title,subject,
    source_digest,state,retryable,failure_message,text_index_state,vector_index_state,
    catalog_json,catalog_digest,created_at,updated_at
)
SELECT
    'legacy:' || b.owner_id || ':' || b.document_id || ':' ||
        CAST(b.content_generation AS TEXT) || ':math',
    b.owner_id,b.document_id,b.content_generation,d.title,'math',
    COALESCE(NULLIF(src.blob_sha256,''),'unknown:' || b.document_id || ':' ||
        CAST(b.content_generation AS TEXT)),
    'waiting_ingest',0,'','pending','pending',NULL,'',b.created_at,b.updated_at
FROM kb_semantic_document_bindings b
JOIN kb_documents d ON d.id=b.document_id
LEFT JOIN kb_ingest_document_sources src
  ON src.document_id=b.document_id AND src.content_generation=b.content_generation
WHERE b.lifecycle_state='active' AND d.deleted=0
  AND (
      lower(COALESCE(src.extension,''))='.pdf'
      OR lower(COALESCE(src.media_type,''))='application/pdf'
      OR lower(d.title) LIKE '%.pdf'
      OR lower(d.source) LIKE '%.pdf'
  );
`
