package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// K12AccumulationDictationReAddV79 为已移出练习集的积累默写保留独立可再次加入状态。
var K12AccumulationDictationReAddV79 = Migration{
	Version:     79,
	Description: "K12 accumulation dictation durable re-add state",
	AtomicFunc:  migrateK12AccumulationDictationReAddV79,
}

const k12AccumulationDictationReAddV79DDL = `
CREATE TABLE k12_accumulation_dictation_generations_v79 (
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
                                  CHECK(status IN ('queued','generating','validating','committed','failed','re_add')),
    source_snapshot_json     TEXT NOT NULL DEFAULT '{}',
    route_snapshot_json      TEXT NOT NULL DEFAULT '{}',
    invocation_snapshot_json TEXT NOT NULL DEFAULT '{}',
    practice_item_id         TEXT NOT NULL DEFAULT '',
    failure_reason           TEXT NOT NULL DEFAULT '',
    attempt                  INTEGER NOT NULL DEFAULT 0,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    UNIQUE(accumulation_id, command_key)
);`

const k12AccumulationDictationReAddV79PostDDL = `
ALTER TABLE k12_accumulation_dictation_generations_v79
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
END;`

func migrateK12AccumulationDictationReAddV79(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 accumulation dictation V79 migration: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil &&
			!errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf(
				"roll back K12 accumulation dictation V79 migration: %w", rollbackErr,
			))
		}
	}()

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS k12_accumulation_dictation_generations_v79`); err != nil {
		return fmt.Errorf("drop stale K12 accumulation dictation V79 table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, k12AccumulationDictationReAddV79DDL); err != nil {
		return fmt.Errorf("create K12 accumulation dictation V79 table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_accumulation_dictation_generations_v79 (
        generation_id,accumulation_id,agent_name,command_key,request_digest,status,
        source_snapshot_json,route_snapshot_json,invocation_snapshot_json,
        practice_item_id,failure_reason,attempt,created_at,updated_at
    )
    SELECT generation_id,accumulation_id,agent_name,command_key,request_digest,status,
        source_snapshot_json,route_snapshot_json,invocation_snapshot_json,
        practice_item_id,failure_reason,attempt,created_at,updated_at
    FROM k12_accumulation_dictation_generations`); err != nil {
		return fmt.Errorf("copy K12 accumulation dictation V79 rows: %w", err)
	}
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS k12_accum_dictation_identity_immutable`,
		`DROP TABLE k12_accumulation_dictation_generations`,
		k12AccumulationDictationReAddV79PostDDL,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("activate K12 accumulation dictation V79 schema: %w", err)
		}
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return fmt.Errorf("check K12 accumulation dictation V79 foreign keys: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("K12 accumulation dictation V79 foreign-key check found %d conflicts", violations)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 accumulation dictation V79 migration: %w", err)
	}
	return nil
}
