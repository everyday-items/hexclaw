package migrate

// K12ModelInvocationsV15DDL is the single schema source for the durable
// external-model call ledger (DD-020). Runtime stores never auto-create it;
// installation is release-governed through numbered migration V15.
const K12ModelInvocationsV15DDL = `
CREATE TABLE IF NOT EXISTS k12_model_invocations (
    invocation_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    stage TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    route_snapshot_json TEXT NOT NULL,
    request_policy_snapshot_json TEXT NOT NULL DEFAULT '',
    provider_idempotency_key TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN ('prepared','sent','succeeded','failed','outcome_unknown','reconciled')),
    attempt INTEGER NOT NULL CHECK(attempt >= 1),
    result_digest TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL DEFAULT ''
        CHECK(result_json='' OR json_valid(result_json)),
    external_request_id TEXT NOT NULL DEFAULT '',
    failure_kind TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(job_id, stage, attempt),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE,
    FOREIGN KEY(job_id) REFERENCES k12_grading_jobs(record_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_k12_model_invocations_job
    ON k12_model_invocations(agent_name, job_id, stage, attempt);
CREATE INDEX IF NOT EXISTS idx_k12_model_invocations_status
    ON k12_model_invocations(status, updated_at);`
