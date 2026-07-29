package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12AgentCascadeV62 repairs the three V37 child tables whose default
// NO ACTION foreign keys prevented the canonical Agent-row ownership cascade.
// Existing V37 migrations stay frozen; this migration copies every persisted
// column and recreates their indexes and immutable-identity triggers atomically.
var K12AgentCascadeV62 = Migration{
	Version:     62,
	Description: "K12 V37 Agent ownership cascade repair",
	AtomicFunc:  migrateK12AgentCascadeV62,
}

func migrateK12AgentCascadeV62(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 Agent cascade migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, K12AgentCascadeV62DDL); err != nil {
		return fmt.Errorf("rebuild K12 V37 cascade tables: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("run K12 Agent cascade foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowID, foreignKeyID any
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			return fmt.Errorf("scan K12 Agent cascade foreign_key_check: %w", err)
		}
		return fmt.Errorf(
			"K12 Agent cascade foreign_key_check failed: table=%s parent=%s",
			table,
			parent,
		)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate K12 Agent cascade foreign_key_check: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close K12 Agent cascade foreign_key_check: %w", err)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 Agent cascade migration: %w", err)
	}
	return nil
}

const K12AgentCascadeV62DDL = `
CREATE TABLE k12_current_create_receipts_v62 (
    agent_name      TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    object_kind     TEXT NOT NULL
                         CHECK(object_kind IN ('creative_work','accumulation')),
    command_key     TEXT NOT NULL,
    request_digest  TEXT NOT NULL,
    object_id       TEXT NOT NULL,
    receipt_json    TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    PRIMARY KEY(agent_name, object_kind, command_key)
);
INSERT INTO k12_current_create_receipts_v62 (
    agent_name, object_kind, command_key, request_digest,
    object_id, receipt_json, created_at
)
SELECT agent_name, object_kind, command_key, request_digest,
       object_id, receipt_json, created_at
FROM k12_current_create_receipts;
DROP TABLE k12_current_create_receipts;
ALTER TABLE k12_current_create_receipts_v62
    RENAME TO k12_current_create_receipts;
CREATE INDEX idx_k12_current_create_receipt_object
    ON k12_current_create_receipts(agent_name, object_kind, object_id);

CREATE TABLE k12_work_feedback_generations_v62 (
    generation_id            TEXT PRIMARY KEY,
    work_id                  TEXT NOT NULL
                                  REFERENCES k12_creative_works(record_id)
                                  ON DELETE CASCADE,
    agent_name               TEXT NOT NULL
                                  REFERENCES agents(name)
                                  ON DELETE CASCADE,
    generation_no            INTEGER NOT NULL CHECK(generation_no > 0),
    command_key              TEXT NOT NULL,
    request_digest           TEXT NOT NULL,
    status                   TEXT NOT NULL DEFAULT 'queued'
                                  CHECK(status IN ('queued','running','succeeded','failed')),
    feedback_type            TEXT NOT NULL DEFAULT '',
    source_snapshot_json     TEXT NOT NULL DEFAULT '{}',
    request_snapshot_json    TEXT NOT NULL DEFAULT '{}',
    route_snapshot_json      TEXT NOT NULL DEFAULT '{}',
    invocation_snapshot_json TEXT NOT NULL DEFAULT '{}',
    feedback_json            TEXT NOT NULL DEFAULT '',
    projection_markdown      TEXT NOT NULL DEFAULT '',
    failure_reason           TEXT NOT NULL DEFAULT '',
    attempt                  INTEGER NOT NULL DEFAULT 0,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    UNIQUE(work_id, generation_no),
    UNIQUE(work_id, command_key)
);
INSERT INTO k12_work_feedback_generations_v62 (
    generation_id, work_id, agent_name, generation_no, command_key,
    request_digest, status, feedback_type, source_snapshot_json,
    request_snapshot_json, route_snapshot_json, invocation_snapshot_json,
    feedback_json, projection_markdown, failure_reason, attempt,
    created_at, updated_at
)
SELECT generation_id, work_id, agent_name, generation_no, command_key,
       request_digest, status, feedback_type, source_snapshot_json,
       request_snapshot_json, route_snapshot_json, invocation_snapshot_json,
       feedback_json, projection_markdown, failure_reason, attempt,
       created_at, updated_at
FROM k12_work_feedback_generations;
DROP TRIGGER IF EXISTS k12_work_feedback_generation_identity_immutable;
DROP TABLE k12_work_feedback_generations;
ALTER TABLE k12_work_feedback_generations_v62
    RENAME TO k12_work_feedback_generations;
CREATE UNIQUE INDEX idx_k12_work_feedback_one_active
    ON k12_work_feedback_generations(work_id)
    WHERE status IN ('queued','running');
CREATE INDEX idx_k12_work_feedback_owner_work
    ON k12_work_feedback_generations(agent_name, work_id, generation_no);
CREATE TRIGGER k12_work_feedback_generation_identity_immutable
BEFORE UPDATE OF generation_id, work_id, agent_name, generation_no, command_key,
                 request_digest, feedback_type, source_snapshot_json,
                 request_snapshot_json, route_snapshot_json
ON k12_work_feedback_generations
BEGIN
    SELECT RAISE(ABORT, 'work feedback generation identity is immutable');
END;

CREATE TABLE k12_accumulation_dictation_generations_v62 (
    generation_id            TEXT PRIMARY KEY,
    accumulation_id          TEXT NOT NULL
                                  REFERENCES k12_accumulations(record_id)
                                  ON DELETE CASCADE,
    agent_name               TEXT NOT NULL
                                  REFERENCES agents(name)
                                  ON DELETE CASCADE,
    command_key              TEXT NOT NULL,
    request_digest           TEXT NOT NULL,
    status                   TEXT NOT NULL DEFAULT 'queued'
                                  CHECK(status IN ('queued','generating','validating','committed','failed')),
    source_snapshot_json     TEXT NOT NULL DEFAULT '{}',
    route_snapshot_json      TEXT NOT NULL DEFAULT '{}',
    invocation_snapshot_json TEXT NOT NULL DEFAULT '{}',
    practice_item_id         TEXT NOT NULL DEFAULT '',
    failure_reason           TEXT NOT NULL DEFAULT '',
    attempt                  INTEGER NOT NULL DEFAULT 0,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    UNIQUE(accumulation_id, command_key)
);
INSERT INTO k12_accumulation_dictation_generations_v62 (
    generation_id, accumulation_id, agent_name, command_key, request_digest,
    status, source_snapshot_json, route_snapshot_json,
    invocation_snapshot_json, practice_item_id, failure_reason, attempt,
    created_at, updated_at
)
SELECT generation_id, accumulation_id, agent_name, command_key, request_digest,
       status, source_snapshot_json, route_snapshot_json,
       invocation_snapshot_json, practice_item_id, failure_reason, attempt,
       created_at, updated_at
FROM k12_accumulation_dictation_generations;
DROP TRIGGER IF EXISTS k12_accum_dictation_identity_immutable;
DROP TABLE k12_accumulation_dictation_generations;
ALTER TABLE k12_accumulation_dictation_generations_v62
    RENAME TO k12_accumulation_dictation_generations;
CREATE UNIQUE INDEX idx_k12_accum_dictation_practice_item
    ON k12_accumulation_dictation_generations(practice_item_id)
    WHERE practice_item_id != '';
CREATE UNIQUE INDEX idx_k12_accum_dictation_one_active
    ON k12_accumulation_dictation_generations(accumulation_id)
    WHERE status IN ('queued','generating','validating');
CREATE INDEX idx_k12_accum_dictation_owner
    ON k12_accumulation_dictation_generations(
        agent_name, accumulation_id, updated_at
    );
CREATE TRIGGER k12_accum_dictation_identity_immutable
BEFORE UPDATE OF generation_id, accumulation_id, agent_name, command_key,
                 request_digest, source_snapshot_json, route_snapshot_json
ON k12_accumulation_dictation_generations
BEGIN
    SELECT RAISE(ABORT, 'accumulation dictation generation identity is immutable');
END;
`
