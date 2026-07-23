package migrate

// K12GenericPrintJobsV25 extends DD-023 to printable objects that must not
// mutate their source domain object when a native receipt is committed.
var K12GenericPrintJobsV25 = Migration{
	Version:     25,
	Description: "v0.5.0 DD-023：通用不可变打印 Artifact 与持久原生 PrintJob",
	SQL:         K12GenericPrintJobsV25DDL,
}

const K12GenericPrintJobsV25DDL = `
CREATE TABLE IF NOT EXISTS k12_print_artifacts (
    artifact_id        TEXT    PRIMARY KEY,
    agent_name         TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    source_kind        TEXT    NOT NULL CHECK(source_kind IN
        ('tutoring_tips','creative_observation_card','practice_question','practice_answer')),
    source_ref         TEXT    NOT NULL CHECK(length(trim(source_ref)) BETWEEN 1 AND 512),
    title              TEXT    NOT NULL CHECK(length(trim(title)) BETWEEN 1 AND 256),
    canonical_markdown TEXT    NOT NULL CHECK(length(trim(canonical_markdown)) BETWEEN 1 AND 4194304),
    source_digest      TEXT    NOT NULL CHECK(length(source_digest) = 64),
    created_at         INTEGER NOT NULL CHECK(created_at > 0),
    UNIQUE(agent_name, source_kind, source_ref, source_digest)
);
CREATE INDEX IF NOT EXISTS idx_k12_print_artifacts_source
    ON k12_print_artifacts(agent_name, source_kind, source_ref, created_at);

-- Artifacts are facts frozen at prepare time. A retry creates/reuses a Job;
-- correcting content creates a new digest/Artifact instead of rewriting history.
CREATE TRIGGER IF NOT EXISTS trg_k12_print_artifacts_immutable
BEFORE UPDATE ON k12_print_artifacts
BEGIN
    SELECT RAISE(ABORT, 'k12 print artifact is immutable');
END;

CREATE TABLE IF NOT EXISTS k12_generic_print_jobs (
    print_job_id          TEXT    PRIMARY KEY,
    agent_name            TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    idempotency_key       TEXT    NOT NULL CHECK(length(trim(idempotency_key)) BETWEEN 1 AND 512),
    request_digest        TEXT    NOT NULL CHECK(length(request_digest) = 64),
    artifact_id           TEXT    NOT NULL REFERENCES k12_print_artifacts(artifact_id) ON DELETE RESTRICT,
    status                TEXT    NOT NULL DEFAULT 'preparing' CHECK(status IN
        ('preparing','dialog_open','submitted','printed','cancelled','failed','outcome_unknown')),
    attempt_count         INTEGER NOT NULL DEFAULT 1 CHECK(attempt_count BETWEEN 1 AND 3),
    native_job_id         TEXT    NOT NULL DEFAULT '',
    native_receipt_id     TEXT    NOT NULL DEFAULT '',
    printer_snapshot_json TEXT    NOT NULL DEFAULT '{}',
    failure_kind          TEXT    NOT NULL DEFAULT '',
    failure_detail        TEXT    NOT NULL DEFAULT '',
    prepared_at           INTEGER NOT NULL CHECK(prepared_at > 0),
    printed_at            INTEGER NOT NULL DEFAULT 0,
    created_at            INTEGER NOT NULL CHECK(created_at > 0),
    updated_at            INTEGER NOT NULL CHECK(updated_at > 0),
    version               INTEGER NOT NULL DEFAULT 0,
    UNIQUE(agent_name, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_k12_generic_print_jobs_owner_status
    ON k12_generic_print_jobs(agent_name, status, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_generic_print_jobs_unresolved_artifact
    ON k12_generic_print_jobs(agent_name, artifact_id)
    WHERE status IN ('preparing','dialog_open','submitted','outcome_unknown');
`
