package migrate

// KnowledgeIndexV23 is the additive storage boundary for the versioned
// semantic index. The legacy kb_chunks.embedding column intentionally remains
// untouched so the previous release can still be used as a rollback artifact;
// all new vectors are scoped by an immutable revision/profile snapshot in
// kb_revision_vectors.
var KnowledgeIndexV23 = Migration{
	Version:     23,
	Description: "v0.5.0 Knowledge 语义索引：corpus policy、不可变 profile/revision、持久 Job/checkpoint/batch 与 revision-scoped vectors",
	SQL:         KnowledgeIndexV23DDL,
}

// KnowledgeIndexV23DDL contains only additive DDL. Runtime knowledge stores
// must not recreate these tables: storage/migrate is their single schema
// owner. All INTEGER timestamps in this schema use Unix milliseconds.
const KnowledgeIndexV23DDL = `
CREATE TABLE IF NOT EXISTS kb_semantic_corpora (
    corpus_uid         TEXT    PRIMARY KEY,
    owner_id           TEXT    NOT NULL CHECK(length(trim(owner_id)) > 0),
    corpus_alias       TEXT    NOT NULL CHECK(length(trim(corpus_alias)) > 0),
    kind               TEXT    NOT NULL DEFAULT 'general'
                               CHECK(kind IN ('general','agent')),
    content_version    INTEGER NOT NULL DEFAULT 0 CHECK(content_version >= 0),
    active_revision_id TEXT,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    UNIQUE(owner_id, corpus_alias),
    UNIQUE(owner_id, corpus_uid),
    FOREIGN KEY(corpus_uid, active_revision_id)
        REFERENCES kb_index_revisions(corpus_uid, revision_id)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX IF NOT EXISTS idx_kb_semantic_corpora_owner
    ON kb_semantic_corpora(owner_id, corpus_alias);

CREATE TABLE IF NOT EXISTS kb_embedding_profile_snapshots (
    profile_snapshot_id TEXT    PRIMARY KEY,
    corpus_uid          TEXT    NOT NULL
                                REFERENCES kb_semantic_corpora(corpus_uid) ON DELETE CASCADE,
    resolved_profile_id TEXT    NOT NULL CHECK(length(trim(resolved_profile_id)) > 0),
    provider_id         TEXT    NOT NULL CHECK(length(trim(provider_id)) > 0),
    provider_name       TEXT    NOT NULL CHECK(length(trim(provider_name)) > 0),
    provider_location   TEXT    NOT NULL CHECK(provider_location IN ('local','cloud')),
    model_name          TEXT    NOT NULL CHECK(length(trim(model_name)) > 0),
    dimension           INTEGER NOT NULL CHECK(dimension > 0),
    normalization       TEXT    NOT NULL CHECK(length(trim(normalization)) > 0),
    chunk_config_hash   TEXT    NOT NULL CHECK(length(trim(chunk_config_hash)) > 0),
    profile_config_hash TEXT    NOT NULL CHECK(length(trim(profile_config_hash)) > 0),
    availability        TEXT    NOT NULL CHECK(availability IN (
        'installed','downloadable','downloading','connected','unavailable'
    )),
    created_at          INTEGER NOT NULL,
    UNIQUE(profile_snapshot_id, corpus_uid),
    UNIQUE(
        profile_snapshot_id, corpus_uid, profile_config_hash,
        provider_id, provider_location, model_name, dimension
    )
);
CREATE INDEX IF NOT EXISTS idx_kb_embedding_profile_snapshots_config
    ON kb_embedding_profile_snapshots(corpus_uid, profile_config_hash);

CREATE TABLE IF NOT EXISTS kb_index_revisions (
    revision_id                 TEXT    PRIMARY KEY,
    corpus_uid                  TEXT    NOT NULL
                                        REFERENCES kb_semantic_corpora(corpus_uid) ON DELETE CASCADE,
    profile_snapshot_id         TEXT    NOT NULL,
    policy_version              INTEGER NOT NULL CHECK(policy_version >= 0),
    previous_selection_kind     TEXT    NOT NULL
                                        CHECK(previous_selection_kind IN ('auto','profile','disabled')),
    previous_selected_profile_id TEXT,
    chunk_set_digest            TEXT    NOT NULL DEFAULT '',
    base_content_version        INTEGER NOT NULL CHECK(base_content_version >= 0),
    indexed_through_version     INTEGER NOT NULL DEFAULT 0
                                        CHECK(indexed_through_version >= 0),
    previous_active_revision_id TEXT,
    publish_state               TEXT    NOT NULL
                                        CHECK(publish_state IN ('staged','active','superseded','abandoned')),
    expected_chunks             INTEGER CHECK(expected_chunks IS NULL OR expected_chunks >= 0),
    embedded_chunks             INTEGER NOT NULL DEFAULT 0 CHECK(embedded_chunks >= 0),
    failed_chunks               INTEGER NOT NULL DEFAULT 0 CHECK(failed_chunks >= 0),
    lease_epoch                 INTEGER NOT NULL DEFAULT 0 CHECK(lease_epoch >= 0),
    created_at                  INTEGER NOT NULL,
    published_at                INTEGER,
    updated_at                  INTEGER NOT NULL,
    UNIQUE(corpus_uid, revision_id),
    UNIQUE(revision_id, corpus_uid, profile_snapshot_id),
    CHECK(
        (previous_selection_kind = 'profile'
            AND previous_selected_profile_id IS NOT NULL
            AND length(trim(previous_selected_profile_id)) > 0)
        OR
        (previous_selection_kind IN ('auto','disabled')
            AND (previous_selected_profile_id IS NULL
                OR length(trim(previous_selected_profile_id)) = 0))
    ),
    CHECK(previous_active_revision_id IS NULL OR previous_active_revision_id <> revision_id),
    CHECK(expected_chunks IS NULL OR embedded_chunks + failed_chunks <= expected_chunks),
    CHECK(
        (publish_state IN ('active','superseded') AND published_at IS NOT NULL)
        OR
        (publish_state IN ('staged','abandoned') AND published_at IS NULL)
    ),
    FOREIGN KEY(profile_snapshot_id, corpus_uid)
        REFERENCES kb_embedding_profile_snapshots(profile_snapshot_id, corpus_uid)
        ON DELETE RESTRICT,
    FOREIGN KEY(corpus_uid, previous_active_revision_id)
        REFERENCES kb_index_revisions(corpus_uid, revision_id)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_index_revisions_one_active
    ON kb_index_revisions(corpus_uid) WHERE publish_state = 'active';
CREATE INDEX IF NOT EXISTS idx_kb_index_revisions_state
    ON kb_index_revisions(corpus_uid, publish_state, created_at);

CREATE TABLE IF NOT EXISTS kb_embedding_policies (
    corpus_uid          TEXT    PRIMARY KEY
                                REFERENCES kb_semantic_corpora(corpus_uid) ON DELETE CASCADE,
    selection_kind      TEXT    NOT NULL CHECK(selection_kind IN ('auto','profile','disabled')),
    selected_profile_id TEXT,
    desired_revision_id TEXT,
    version             INTEGER NOT NULL DEFAULT 0 CHECK(version >= 0),
    updated_at          INTEGER NOT NULL,
    CHECK(
        (selection_kind = 'profile'
            AND selected_profile_id IS NOT NULL
            AND length(trim(selected_profile_id)) > 0)
        OR
        (selection_kind IN ('auto','disabled')
            AND (selected_profile_id IS NULL OR length(trim(selected_profile_id)) = 0))
    ),
    CHECK(selection_kind <> 'disabled' OR desired_revision_id IS NULL),
    FOREIGN KEY(corpus_uid, desired_revision_id)
        REFERENCES kb_index_revisions(corpus_uid, revision_id)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX IF NOT EXISTS idx_kb_embedding_policies_desired
    ON kb_embedding_policies(desired_revision_id)
    WHERE desired_revision_id IS NOT NULL;

-- This one-to-one bridge adds owner/corpus/generation and orthogonal index
-- state without rebuilding the rollback-sensitive kb_documents table.
CREATE TABLE IF NOT EXISTS kb_semantic_document_bindings (
    document_id        TEXT    PRIMARY KEY
                               REFERENCES kb_documents(id) ON DELETE CASCADE,
    owner_id           TEXT    NOT NULL,
    corpus_uid         TEXT    NOT NULL,
    content_generation INTEGER NOT NULL DEFAULT 1 CHECK(content_generation >= 1),
    lifecycle_state    TEXT    NOT NULL DEFAULT 'active'
                               CHECK(lifecycle_state IN ('active','tombstoned')),
    text_state         TEXT    NOT NULL DEFAULT 'pending'
                               CHECK(text_state IN ('pending','building','ready','failed')),
    deleted_at         INTEGER,
    version            INTEGER NOT NULL DEFAULT 0 CHECK(version >= 0),
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    UNIQUE(corpus_uid, document_id, content_generation),
    FOREIGN KEY(owner_id, corpus_uid)
        REFERENCES kb_semantic_corpora(owner_id, corpus_uid) ON DELETE CASCADE,
    CHECK(
        (lifecycle_state = 'active' AND deleted_at IS NULL)
        OR
        (lifecycle_state = 'tombstoned' AND deleted_at IS NOT NULL)
    )
);
CREATE INDEX IF NOT EXISTS idx_kb_semantic_document_bindings_scope
    ON kb_semantic_document_bindings(corpus_uid, lifecycle_state, text_state, document_id);

-- Immutable generation facts are the stable parent of revision/job history.
-- kb_semantic_document_bindings points at the current generation only; moving
-- that pointer therefore cannot invalidate historical revision foreign keys.
CREATE TABLE IF NOT EXISTS kb_semantic_document_generations (
    owner_id           TEXT    NOT NULL,
    corpus_uid         TEXT    NOT NULL,
    document_id        TEXT    NOT NULL
                              REFERENCES kb_documents(id) ON DELETE RESTRICT,
    content_generation INTEGER NOT NULL CHECK(content_generation >= 1),
    created_at         INTEGER NOT NULL,
    PRIMARY KEY(corpus_uid, document_id, content_generation),
    UNIQUE(owner_id, corpus_uid, document_id, content_generation),
    FOREIGN KEY(owner_id, corpus_uid)
        REFERENCES kb_semantic_corpora(owner_id, corpus_uid) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_kb_semantic_document_generations_owner
    ON kb_semantic_document_generations(owner_id, corpus_uid, document_id, content_generation);

CREATE TABLE IF NOT EXISTS kb_revision_documents (
    revision_id       TEXT    NOT NULL,
    corpus_uid        TEXT    NOT NULL,
    document_id       TEXT    NOT NULL,
    content_generation INTEGER NOT NULL CHECK(content_generation >= 1),
    vector_state      TEXT    NOT NULL DEFAULT 'pending'
                              CHECK(vector_state IN (
                                  'pending','building','retry_wait','ready','failed','cancelled'
                              )),
    expected_chunks   INTEGER CHECK(expected_chunks IS NULL OR expected_chunks >= 0),
    embedded_chunks   INTEGER NOT NULL DEFAULT 0 CHECK(embedded_chunks >= 0),
    failed_chunks     INTEGER NOT NULL DEFAULT 0 CHECK(failed_chunks >= 0),
    visible_at        INTEGER,
    last_error        TEXT    NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL,
    PRIMARY KEY(revision_id, document_id, content_generation),
    UNIQUE(revision_id, corpus_uid, document_id, content_generation),
    FOREIGN KEY(corpus_uid, revision_id)
        REFERENCES kb_index_revisions(corpus_uid, revision_id) ON DELETE CASCADE,
    FOREIGN KEY(corpus_uid, document_id, content_generation)
        REFERENCES kb_semantic_document_generations(corpus_uid, document_id, content_generation)
        ON DELETE RESTRICT,
    CHECK(expected_chunks IS NULL OR embedded_chunks + failed_chunks <= expected_chunks),
    CHECK(
        visible_at IS NULL
        OR
        (vector_state = 'ready'
            AND expected_chunks IS NOT NULL
            AND embedded_chunks = expected_chunks
            AND failed_chunks = 0)
    )
);
CREATE INDEX IF NOT EXISTS idx_kb_revision_documents_visible
    ON kb_revision_documents(revision_id, document_id)
    WHERE visible_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_kb_revision_documents_state
    ON kb_revision_documents(revision_id, vector_state, updated_at);

-- A vector row repeats the immutable routing facts deliberately. Composite
-- foreign keys prove they are the same facts captured by its profile snapshot,
-- while the BLOB length check rejects wrong-dimensional provider responses.
CREATE TABLE IF NOT EXISTS kb_revision_vectors (
    revision_id         TEXT    NOT NULL,
    corpus_uid          TEXT    NOT NULL,
    document_id         TEXT    NOT NULL,
    content_generation  INTEGER NOT NULL CHECK(content_generation >= 1),
    chunk_id            TEXT    NOT NULL,
    chunk_index         INTEGER NOT NULL CHECK(chunk_index >= 0),
    chunk_content_hash  TEXT    NOT NULL CHECK(length(trim(chunk_content_hash)) > 0),
    profile_snapshot_id TEXT    NOT NULL,
    profile_config_hash TEXT    NOT NULL CHECK(length(trim(profile_config_hash)) > 0),
    provider_id         TEXT    NOT NULL CHECK(length(trim(provider_id)) > 0),
    provider_location   TEXT    NOT NULL CHECK(provider_location IN ('local','cloud')),
    model_name          TEXT    NOT NULL CHECK(length(trim(model_name)) > 0),
    dimension           INTEGER NOT NULL CHECK(dimension > 0),
    embedding           BLOB    NOT NULL,
    created_at          INTEGER NOT NULL,
    PRIMARY KEY(revision_id, document_id, content_generation, chunk_id),
    FOREIGN KEY(revision_id, corpus_uid, document_id, content_generation)
        REFERENCES kb_revision_documents(
            revision_id, corpus_uid, document_id, content_generation
        ) ON DELETE CASCADE,
    FOREIGN KEY(revision_id, corpus_uid, profile_snapshot_id)
        REFERENCES kb_index_revisions(revision_id, corpus_uid, profile_snapshot_id)
        ON DELETE CASCADE,
    FOREIGN KEY(
        profile_snapshot_id, corpus_uid, profile_config_hash,
        provider_id, provider_location, model_name, dimension
    ) REFERENCES kb_embedding_profile_snapshots(
        profile_snapshot_id, corpus_uid, profile_config_hash,
        provider_id, provider_location, model_name, dimension
    ) ON DELETE RESTRICT,
    CHECK(length(embedding) = dimension * 4)
);
CREATE INDEX IF NOT EXISTS idx_kb_revision_vectors_scan
    ON kb_revision_vectors(revision_id, document_id, content_generation, chunk_index);

CREATE TABLE IF NOT EXISTS kb_knowledge_jobs (
    job_id              TEXT    PRIMARY KEY,
    parent_job_id       TEXT,
    kind                TEXT    NOT NULL
                                CHECK(kind IN ('ingest','download_model','embed_document','rebuild_revision','gc')),
    owner_id            TEXT    NOT NULL,
    corpus_uid          TEXT    NOT NULL,
    document_id         TEXT,
    document_generation INTEGER,
    target_revision_id  TEXT,
    idempotency_key     TEXT    NOT NULL CHECK(length(trim(idempotency_key)) > 0),
    state               TEXT    NOT NULL
                                CHECK(state IN (
                                    'queued','running','retry_wait','succeeded','failed','cancelled'
                                )),
    stage               TEXT    NOT NULL
                                CHECK(stage IN (
                                    'extracting','ocr','chunking','text_indexing',
                                    'embedding','publishing','gc'
                                )),
    pages_done          INTEGER,
    pages_total         INTEGER,
    chunks_done         INTEGER,
    chunks_total        INTEGER,
    attempt             INTEGER NOT NULL DEFAULT 0 CHECK(attempt >= 0),
    next_attempt_at     INTEGER,
    cancel_requested    INTEGER NOT NULL DEFAULT 0 CHECK(cancel_requested IN (0,1)),
    lease_owner         TEXT    NOT NULL DEFAULT '',
    lease_epoch         INTEGER NOT NULL DEFAULT 0 CHECK(lease_epoch >= 0),
    lease_expires_at    INTEGER,
    heartbeat_at        INTEGER,
    last_error          TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    finished_at         INTEGER,
    UNIQUE(job_id, owner_id, corpus_uid),
    UNIQUE(job_id, target_revision_id),
    UNIQUE(owner_id, corpus_uid, kind, idempotency_key),
    FOREIGN KEY(owner_id, corpus_uid)
        REFERENCES kb_semantic_corpora(owner_id, corpus_uid) ON DELETE CASCADE,
    FOREIGN KEY(parent_job_id, owner_id, corpus_uid)
        REFERENCES kb_knowledge_jobs(job_id, owner_id, corpus_uid) ON DELETE CASCADE,
    FOREIGN KEY(corpus_uid, document_id, document_generation)
        REFERENCES kb_semantic_document_generations(corpus_uid, document_id, content_generation)
        ON DELETE RESTRICT,
    FOREIGN KEY(corpus_uid, target_revision_id)
        REFERENCES kb_index_revisions(corpus_uid, revision_id) ON DELETE CASCADE,
    CHECK(parent_job_id IS NULL OR parent_job_id <> job_id),
    CHECK(
        (document_id IS NULL AND document_generation IS NULL)
        OR
        (document_id IS NOT NULL AND document_generation IS NOT NULL)
    ),
    CHECK(kind <> 'embed_document' OR document_id IS NOT NULL),
    CHECK(
        (kind IN ('download_model','embed_document','rebuild_revision') AND target_revision_id IS NOT NULL)
        OR
        (kind IN ('ingest','gc') AND target_revision_id IS NULL)
    ),
    CHECK(
        (pages_done IS NULL AND pages_total IS NULL)
        OR
        (pages_done IS NOT NULL AND pages_total IS NOT NULL
            AND pages_done >= 0 AND pages_total >= 0 AND pages_done <= pages_total)
    ),
    CHECK(
        (chunks_done IS NULL AND chunks_total IS NULL)
        OR
        (chunks_done IS NOT NULL AND chunks_total IS NOT NULL
            AND chunks_done >= 0 AND chunks_total >= 0 AND chunks_done <= chunks_total)
    ),
    CHECK(
        (state = 'running'
            AND lease_owner <> ''
            AND lease_expires_at IS NOT NULL
            AND heartbeat_at IS NOT NULL)
        OR
        (state <> 'running' AND lease_owner = '' AND lease_expires_at IS NULL)
    ),
    CHECK(
        (state = 'retry_wait' AND next_attempt_at IS NOT NULL)
        OR
        (state <> 'retry_wait' AND next_attempt_at IS NULL)
    ),
    CHECK(
        (state IN ('succeeded','failed','cancelled') AND finished_at IS NOT NULL)
        OR
        (state IN ('queued','running','retry_wait') AND finished_at IS NULL)
    )
);
CREATE INDEX IF NOT EXISTS idx_kb_knowledge_jobs_runnable
    ON kb_knowledge_jobs(state, next_attempt_at, created_at)
    WHERE state IN ('queued','retry_wait');
CREATE INDEX IF NOT EXISTS idx_kb_knowledge_jobs_expired_lease
    ON kb_knowledge_jobs(lease_expires_at)
    WHERE state = 'running';
CREATE INDEX IF NOT EXISTS idx_kb_knowledge_jobs_activity
    ON kb_knowledge_jobs(corpus_uid, target_revision_id, state, kind, updated_at);
CREATE INDEX IF NOT EXISTS idx_kb_knowledge_jobs_parent
    ON kb_knowledge_jobs(parent_job_id, state)
    WHERE parent_job_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_kb_knowledge_jobs_document
    ON kb_knowledge_jobs(corpus_uid, document_id, document_generation, state)
    WHERE document_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS kb_job_stage_checkpoints (
    job_id             TEXT    NOT NULL
                               REFERENCES kb_knowledge_jobs(job_id) ON DELETE CASCADE,
    stage              TEXT    NOT NULL
                               CHECK(stage IN (
                                   'extracting','ocr','chunking','text_indexing',
                                   'embedding','publishing','gc'
                               )),
    input_fingerprint  TEXT    NOT NULL CHECK(length(trim(input_fingerprint)) > 0),
    artifact_ref       TEXT    NOT NULL DEFAULT '',
    artifact_digest    TEXT    NOT NULL DEFAULT '',
    state              TEXT    NOT NULL CHECK(state IN ('prepared','succeeded')),
    lease_epoch        INTEGER NOT NULL CHECK(lease_epoch >= 0),
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    PRIMARY KEY(job_id, stage)
);

CREATE TABLE IF NOT EXISTS kb_embedding_batch_manifests (
    batch_id            TEXT    PRIMARY KEY,
    job_id              TEXT    NOT NULL,
    revision_id         TEXT    NOT NULL,
    profile_config_hash TEXT    NOT NULL CHECK(length(trim(profile_config_hash)) > 0),
    chunk_ids_digest    TEXT    NOT NULL CHECK(length(trim(chunk_ids_digest)) > 0),
    payload_digest      TEXT    NOT NULL CHECK(length(trim(payload_digest)) > 0),
    client_request_key  TEXT    NOT NULL CHECK(length(trim(client_request_key)) > 0),
    state               TEXT    NOT NULL CHECK(state IN (
        'prepared','in_flight','retry_wait','succeeded','failed','cancelled','outcome_unknown'
    )),
    attempts            INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
    next_attempt_at     INTEGER,
    provider_request_id TEXT    NOT NULL DEFAULT '',
    lease_epoch         INTEGER NOT NULL CHECK(lease_epoch >= 0),
    last_error          TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    UNIQUE(job_id, revision_id, chunk_ids_digest, payload_digest),
    UNIQUE(client_request_key),
    FOREIGN KEY(job_id, revision_id)
        REFERENCES kb_knowledge_jobs(job_id, target_revision_id) ON DELETE CASCADE,
    CHECK(
        (state = 'retry_wait' AND next_attempt_at IS NOT NULL)
        OR
        (state <> 'retry_wait' AND next_attempt_at IS NULL)
    )
);
CREATE INDEX IF NOT EXISTS idx_kb_embedding_batch_manifests_retry
    ON kb_embedding_batch_manifests(state, next_attempt_at)
    WHERE state = 'retry_wait';
CREATE INDEX IF NOT EXISTS idx_kb_embedding_batch_manifests_job
    ON kb_embedding_batch_manifests(job_id, state, created_at);

CREATE TABLE IF NOT EXISTS kb_embedding_batch_chunks (
    batch_id   TEXT    NOT NULL
                       REFERENCES kb_embedding_batch_manifests(batch_id) ON DELETE CASCADE,
    ordinal    INTEGER NOT NULL CHECK(ordinal >= 0),
    chunk_id   TEXT    NOT NULL,
    content_hash TEXT  NOT NULL CHECK(length(trim(content_hash)) > 0),
    PRIMARY KEY(batch_id, ordinal),
    UNIQUE(batch_id, chunk_id)
);

CREATE TABLE IF NOT EXISTS kb_provider_throttle_states (
    provider_id           TEXT    NOT NULL CHECK(length(trim(provider_id)) > 0),
    model_name            TEXT    NOT NULL CHECK(length(trim(model_name)) > 0),
    credential_fingerprint TEXT   NOT NULL CHECK(length(trim(credential_fingerprint)) > 0),
    cooldown_until        INTEGER,
    reset_at              INTEGER,
    updated_at            INTEGER NOT NULL,
    PRIMARY KEY(provider_id, model_name, credential_fingerprint)
);

CREATE TRIGGER IF NOT EXISTS kb_embedding_profile_snapshots_immutable
BEFORE UPDATE ON kb_embedding_profile_snapshots
BEGIN
    SELECT RAISE(ABORT, 'embedding profile snapshot is immutable');
END;

CREATE TRIGGER IF NOT EXISTS kb_index_revision_identity_immutable
BEFORE UPDATE OF
    corpus_uid, profile_snapshot_id, policy_version,
    previous_selection_kind, previous_selected_profile_id,
    base_content_version, previous_active_revision_id
ON kb_index_revisions
BEGIN
    SELECT RAISE(ABORT, 'knowledge index revision identity is immutable');
END;

CREATE TRIGGER IF NOT EXISTS kb_revision_vectors_immutable
BEFORE UPDATE ON kb_revision_vectors
BEGIN
    SELECT RAISE(ABORT, 'knowledge revision vector is immutable');
END;

CREATE TRIGGER IF NOT EXISTS kb_knowledge_job_identity_immutable
BEFORE UPDATE OF
    parent_job_id, kind, owner_id, corpus_uid, document_id,
    document_generation, target_revision_id, idempotency_key
ON kb_knowledge_jobs
BEGIN
    SELECT RAISE(ABORT, 'knowledge job identity is immutable');
END;

CREATE TRIGGER IF NOT EXISTS kb_job_checkpoint_succeeded_immutable
BEFORE UPDATE OF input_fingerprint, artifact_ref, artifact_digest
ON kb_job_stage_checkpoints
WHEN OLD.state = 'succeeded'
BEGIN
    SELECT RAISE(ABORT, 'succeeded knowledge job checkpoint is immutable');
END;

CREATE TRIGGER IF NOT EXISTS kb_embedding_batch_identity_immutable
BEFORE UPDATE OF
    job_id, revision_id, profile_config_hash, chunk_ids_digest,
    payload_digest, client_request_key
ON kb_embedding_batch_manifests
BEGIN
    SELECT RAISE(ABORT, 'embedding batch identity is immutable');
END;
`
