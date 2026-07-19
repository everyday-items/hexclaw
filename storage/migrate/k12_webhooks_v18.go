package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12WebhooksV18DDL is the single schema source for the DD-019 K12 TriggerAdapter.
// Runtime webhook initialization must not create these tables: production
// installation is release-governed by numbered migration V18.
const K12WebhooksV18DDL = `
CREATE TABLE IF NOT EXISTS k12_webhook_bindings (
    binding_id        TEXT PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    agent_id          TEXT NOT NULL,
    learner_id        TEXT NOT NULL,
    scope             TEXT NOT NULL CHECK(scope = 'direct'),
    allowed_events    TEXT NOT NULL,
    allowed_workflows TEXT NOT NULL DEFAULT '[]',
    secret            TEXT NOT NULL,
    secret_version    INTEGER NOT NULL CHECK(secret_version >= 1),
    status            TEXT NOT NULL CHECK(status IN ('disabled','enabled')),
    created_by        TEXT NOT NULL,
    rotated_at        INTEGER NOT NULL DEFAULT 0,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_k12_webhook_bindings_created_by
    ON k12_webhook_bindings(created_by, created_at);
CREATE INDEX IF NOT EXISTS idx_k12_webhook_bindings_agent
    ON k12_webhook_bindings(agent_id, status);

CREATE TABLE IF NOT EXISTS k12_webhook_receipts (
    receipt_id     TEXT PRIMARY KEY,
    binding_id     TEXT NOT NULL,
    event_id       TEXT NOT NULL,
    event_type     TEXT NOT NULL CHECK(event_type IN (
        '',
        'k12.submission.requested.v1',
        'k12.practice_return.requested.v1',
        'k12.workflow_run.requested.v1'
    )),
    payload_digest TEXT NOT NULL,
    status         TEXT NOT NULL CHECK(status IN (
        'accepted','processing','succeeded','failed','outcome_unknown','rejected'
    )),
    reference      TEXT NOT NULL DEFAULT '',
    failure_kind   TEXT NOT NULL DEFAULT '',
    dispatch_json  TEXT NOT NULL DEFAULT '',
	-- Fresh installations include the latest additive Receipt fields. V22
	-- remains the numbered upgrade path for databases that already applied V18.
	retry_safe     INTEGER NOT NULL DEFAULT 0 CHECK(retry_safe IN (0,1)),
	attempt_count  INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE(binding_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_k12_webhook_receipts_binding
    ON k12_webhook_receipts(binding_id, created_at);
CREATE INDEX IF NOT EXISTS idx_k12_webhook_receipts_recovery
    ON k12_webhook_receipts(status, created_at);

CREATE TABLE IF NOT EXISTS k12_webhook_nonces (
    binding_id TEXT NOT NULL,
    nonce      TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    PRIMARY KEY(binding_id, nonce)
);
CREATE INDEX IF NOT EXISTS idx_k12_webhook_nonces_expiry
    ON k12_webhook_nonces(expires_at);

CREATE TABLE IF NOT EXISTS k12_webhook_audit (
    audit_id     TEXT PRIMARY KEY,
    binding_id   TEXT NOT NULL,
    action       TEXT NOT NULL,
    outcome      TEXT NOT NULL,
    subject_hash TEXT NOT NULL DEFAULT '',
    failure_kind TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_k12_webhook_audit_binding
    ON k12_webhook_audit(binding_id, created_at);`

// migrateK12WebhooksV18 also upgrades databases created by the short-lived
// pre-migration implementation, whose Receipt table did not yet contain the
// durable dispatch envelope. The column probe keeps re-entry safe.
func migrateK12WebhooksV18(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, K12WebhooksV18DDL); err != nil {
		return fmt.Errorf("create K12 webhook schema: %w", err)
	}
	hasDispatch, err := columnExists(ctx, db, "k12_webhook_receipts", "dispatch_json")
	if err != nil {
		return fmt.Errorf("inspect K12 webhook Receipt schema: %w", err)
	}
	if !hasDispatch {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE k12_webhook_receipts ADD COLUMN dispatch_json TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add durable K12 webhook dispatch: %w", err)
		}
	}
	return nil
}
