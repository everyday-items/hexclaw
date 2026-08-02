package migrate

// ToolApprovalCoordinatorV70 makes the backend the sole durable authority for
// interactive tool approvals. The migration is additive: legacy grants remain
// present but start inactive because their scope schema was not recorded.
var ToolApprovalCoordinatorV70 = Migration{
	Version:     70,
	Description: "durable tool approval request decision ack release and revocable grants",
	SQL: `
ALTER TABLE remembered_permission_grants ADD COLUMN grant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE remembered_permission_grants ADD COLUMN created_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE remembered_permission_grants ADD COLUMN created_decision_id TEXT NOT NULL DEFAULT '';
ALTER TABLE remembered_permission_grants ADD COLUMN active INTEGER NOT NULL DEFAULT 0 CHECK(active IN (0, 1));
ALTER TABLE remembered_permission_grants ADD COLUMN revoked_at INTEGER;
ALTER TABLE remembered_permission_grants ADD COLUMN revoked_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE remembered_permission_grants ADD COLUMN schema_version INTEGER NOT NULL DEFAULT 0;

CREATE UNIQUE INDEX IF NOT EXISTS idx_remembered_permission_grants_grant_id
    ON remembered_permission_grants(grant_id) WHERE grant_id <> '';
CREATE INDEX IF NOT EXISTS idx_remembered_permission_grants_active_session
    ON remembered_permission_grants(resolved_session_id, active, schema_version);

CREATE TABLE IF NOT EXISTS tool_approval_requests (
    approval_request_id   TEXT PRIMARY KEY,
    invocation_id         TEXT NOT NULL UNIQUE,
    owner_id              TEXT NOT NULL,
    resolved_session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    canonical_tool_name   TEXT NOT NULL,
    arguments_digest      TEXT NOT NULL,
    security_scope_digest TEXT NOT NULL,
    scope_schema_version  INTEGER NOT NULL,
    arguments_envelope    TEXT NOT NULL DEFAULT '',
    deadline_at           INTEGER NOT NULL,
    state                 TEXT NOT NULL DEFAULT 'pending'
                          CHECK(state IN ('pending','approved_once','approved_remember','denied','expired','fenced')),
    decision_id           TEXT,
    decision              TEXT CHECK(decision IS NULL OR decision IN ('approved_once','approved_remember','denied')),
    idempotency_key       TEXT,
    decision_fingerprint  TEXT NOT NULL DEFAULT '',
    terminal_result       TEXT NOT NULL DEFAULT '',
    ack_status            TEXT NOT NULL DEFAULT 'pending'
                          CHECK(ack_status IN ('pending','accepted','expired','rejected')),
    release_state         TEXT NOT NULL DEFAULT 'held'
                          CHECK(release_state IN ('held','authorized','consumed','fenced')),
    created_at            INTEGER NOT NULL,
    decided_at            INTEGER,
    ack_committed_at      INTEGER,
    released_at           INTEGER,
    consumed_at           INTEGER,
    updated_at            INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_approval_decision_id
    ON tool_approval_requests(decision_id) WHERE decision_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_approval_idempotency_key
    ON tool_approval_requests(idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tool_approval_pending_owner_session
    ON tool_approval_requests(owner_id, resolved_session_id, state, deadline_at);
CREATE INDEX IF NOT EXISTS idx_tool_approval_session_release
    ON tool_approval_requests(resolved_session_id, release_state);
`,
}
