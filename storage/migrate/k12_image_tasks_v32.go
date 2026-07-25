package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ImageTasksV32 is the additive cutover for the sole public image facade.
// Dispatch and intake are operational aggregates, deliberately separate from
// records.Store so they never leak into formal work lists or .hexbak records.
var K12ImageTasksV32 = Migration{
	Version:     32,
	Description: "v0.5.0 ImageTaskDispatch facade、CreativeWorkIntake 与作品原子晋升",
	AtomicFunc:  migrateK12ImageTasksV32,
}

const k12ImageTasksV32DDL = `
CREATE TABLE k12_image_task_dispatches (
    dispatch_id                        TEXT    PRIMARY KEY,
    agent_name                        TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    learner_id                        TEXT    NOT NULL,
    source_kind                       TEXT    NOT NULL CHECK(source_kind IN ('desktop','api','im_direct')),
    source_ref                        TEXT    NOT NULL,
    source_session_id                 TEXT    NOT NULL DEFAULT '',
    source_asset_refs_json            TEXT    NOT NULL,
    source_digest                     TEXT    NOT NULL,
    message_intent                    TEXT    NOT NULL DEFAULT '',
    task_intent                       TEXT    NOT NULL DEFAULT 'unknown'
        CHECK(task_intent IN ('completed_homework','blank_worksheet','writing','artwork','unknown')),
    intent_evidence_json               TEXT    NOT NULL DEFAULT '[]',
    intent_confidence                  REAL    NOT NULL DEFAULT 0
        CHECK(intent_confidence >= 0 AND intent_confidence <= 1),
    confirmation_candidates_json       TEXT    NOT NULL DEFAULT '[]',
    status                             TEXT    NOT NULL DEFAULT 'routing'
        CHECK(status IN ('routing','awaiting_confirmation','routed','failed','cancelled')),
    target_object_type                 TEXT    NOT NULL DEFAULT ''
        CHECK(target_object_type IN ('','homework_submission','creative_work_intake')),
    target_object_id                   TEXT    NOT NULL DEFAULT '',
    classification_route_snapshot_json TEXT    NOT NULL,
    classification_invocation_id       TEXT    NOT NULL,
    route_policy_snapshot_json          TEXT    NOT NULL,
    idempotency_key                    TEXT    NOT NULL,
    request_digest                     TEXT    NOT NULL,
    attempt_generation                 INTEGER NOT NULL DEFAULT 1 CHECK(attempt_generation >= 1),
    retry_safe                         INTEGER NOT NULL DEFAULT 0 CHECK(retry_safe IN (0,1)),
    failure_kind                       TEXT    NOT NULL DEFAULT '',
    version                            INTEGER NOT NULL DEFAULT 0,
    created_at                         INTEGER NOT NULL,
    updated_at                         INTEGER NOT NULL,
    UNIQUE(agent_name, idempotency_key),
    CHECK(
        (status = 'routed' AND target_object_type != '' AND target_object_id != '') OR
        (status != 'routed')
    ),
    CHECK(
        (task_intent IN ('completed_homework','blank_worksheet') AND
            (target_object_type IN ('','homework_submission'))) OR
        (task_intent IN ('writing','artwork') AND
            (target_object_type IN ('','creative_work_intake'))) OR
        (task_intent = 'unknown' AND target_object_type = '')
    )
);
CREATE INDEX idx_k12_image_dispatch_owner_status
    ON k12_image_task_dispatches(agent_name, status, updated_at);
CREATE INDEX idx_k12_image_dispatch_source
    ON k12_image_task_dispatches(agent_name, source_kind, source_ref, attempt_generation);

CREATE TABLE k12_homework_submissions (
    submission_id         TEXT    PRIMARY KEY,
    dispatch_id           TEXT    NOT NULL UNIQUE
        REFERENCES k12_image_task_dispatches(dispatch_id) ON DELETE CASCADE,
    agent_name            TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    learner_id            TEXT    NOT NULL,
    source_kind           TEXT    NOT NULL CHECK(source_kind IN ('desktop','api','im_direct')),
    source_ref            TEXT    NOT NULL,
    source_asset_refs_json TEXT   NOT NULL,
    task_intent           TEXT    NOT NULL
        CHECK(task_intent IN ('completed_homework','blank_worksheet')),
    status                TEXT    NOT NULL DEFAULT 'received'
        CHECK(status IN ('received','processing','awaiting_confirmation','completed','failed','cancelled')),
    grading_job_id        TEXT    NOT NULL DEFAULT '',
    idempotency_key       TEXT    NOT NULL,
    version               INTEGER NOT NULL DEFAULT 0,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    UNIQUE(agent_name, idempotency_key)
);
CREATE INDEX idx_k12_homework_submission_owner_status
    ON k12_homework_submissions(agent_name, status, updated_at);

CREATE TABLE k12_creative_work_intakes (
    intake_id                      TEXT    PRIMARY KEY,
    dispatch_id                    TEXT    NOT NULL UNIQUE
        REFERENCES k12_image_task_dispatches(dispatch_id) ON DELETE CASCADE,
    agent_name                     TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    learner_id                     TEXT    NOT NULL,
    work_type                      TEXT    NOT NULL CHECK(work_type IN ('writing','art')),
    source_asset_refs_json         TEXT    NOT NULL,
    source_digest                  TEXT    NOT NULL,
    work_title_candidate_json      TEXT    NOT NULL DEFAULT '',
    task_requirement_candidate_json TEXT   NOT NULL DEFAULT '',
    ocr_evidence_json              TEXT    NOT NULL DEFAULT '',
    route_policy_snapshot_json     TEXT    NOT NULL,
    operation_invocations_json     TEXT    NOT NULL DEFAULT '[]',
    status                         TEXT    NOT NULL DEFAULT 'preparing'
        CHECK(status IN ('preparing','awaiting_confirmation','ready','promoted','failed','cancelled')),
    confirmation_provenance        TEXT    NOT NULL DEFAULT ''
        CHECK(confirmation_provenance IN ('','evidence_auto_freeze','parent_confirmed','parent_corrected')),
    promoted_work_id               TEXT    NOT NULL DEFAULT '',
    idempotency_key                TEXT    NOT NULL,
    request_digest                 TEXT    NOT NULL,
    attempt_generation             INTEGER NOT NULL DEFAULT 1 CHECK(attempt_generation >= 1),
    retry_safe                     INTEGER NOT NULL DEFAULT 0 CHECK(retry_safe IN (0,1)),
    failure_kind                   TEXT    NOT NULL DEFAULT '',
    version                        INTEGER NOT NULL DEFAULT 0,
    created_at                     INTEGER NOT NULL,
    updated_at                     INTEGER NOT NULL,
    UNIQUE(agent_name, idempotency_key),
    CHECK(
        (status = 'promoted' AND promoted_work_id != '') OR
        (status != 'promoted' AND promoted_work_id = '')
    )
);
CREATE INDEX idx_k12_creative_intake_owner_status
    ON k12_creative_work_intakes(agent_name, status, updated_at);

CREATE TABLE k12_image_task_invocations (
    invocation_id           TEXT    PRIMARY KEY,
    agent_name              TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    dispatch_id             TEXT REFERENCES k12_image_task_dispatches(dispatch_id) ON DELETE CASCADE,
    intake_id               TEXT REFERENCES k12_creative_work_intakes(intake_id) ON DELETE CASCADE,
    work_record_id          TEXT REFERENCES k12_creative_works(record_id) ON DELETE CASCADE,
    operation               TEXT    NOT NULL CHECK(operation IN ('classification','writing_ocr','work_feedback')),
    operation_key           TEXT    NOT NULL,
    request_digest          TEXT    NOT NULL,
    route_snapshot_json     TEXT    NOT NULL,
    status                  TEXT    NOT NULL DEFAULT 'prepared'
        CHECK(status IN ('prepared','sent','succeeded','failed','outcome_unknown','reconciled')),
    attempt                 INTEGER NOT NULL DEFAULT 1 CHECK(attempt >= 1),
    provider_request_key    TEXT    NOT NULL DEFAULT '',
    result_digest           TEXT    NOT NULL DEFAULT '',
    result_json             TEXT    NOT NULL DEFAULT '',
    error_kind              TEXT    NOT NULL DEFAULT '',
    retry_safe              INTEGER NOT NULL DEFAULT 0 CHECK(retry_safe IN (0,1)),
    started_at              INTEGER NOT NULL DEFAULT 0,
    finished_at             INTEGER NOT NULL DEFAULT 0,
    created_at              INTEGER NOT NULL,
    updated_at              INTEGER NOT NULL,
    UNIQUE(agent_name, operation_key, attempt),
    CHECK(
        (operation = 'classification' AND dispatch_id IS NOT NULL AND intake_id IS NULL AND work_record_id IS NULL) OR
        (operation = 'writing_ocr' AND dispatch_id IS NULL AND intake_id IS NOT NULL AND work_record_id IS NULL) OR
        (operation = 'work_feedback' AND dispatch_id IS NULL AND intake_id IS NULL AND work_record_id IS NOT NULL)
    )
);
CREATE INDEX idx_k12_image_invocation_recovery
    ON k12_image_task_invocations(agent_name, status, updated_at);

CREATE TRIGGER k12_image_dispatch_identity_immutable
BEFORE UPDATE OF agent_name, learner_id, source_kind, source_ref, source_asset_refs_json,
    source_digest, classification_route_snapshot_json, classification_invocation_id,
    route_policy_snapshot_json, idempotency_key, request_digest, attempt_generation
ON k12_image_task_dispatches
BEGIN
    SELECT RAISE(ABORT, 'image task dispatch identity is immutable');
END;

CREATE TRIGGER k12_creative_intake_identity_immutable
BEFORE UPDATE OF dispatch_id, agent_name, learner_id, work_type, source_asset_refs_json,
    source_digest, route_policy_snapshot_json, idempotency_key, request_digest, attempt_generation
ON k12_creative_work_intakes
BEGIN
    SELECT RAISE(ABORT, 'creative work intake identity is immutable');
END;

CREATE TRIGGER k12_image_invocation_identity_immutable
BEFORE UPDATE OF agent_name, dispatch_id, intake_id, work_record_id, operation,
    operation_key, request_digest, route_snapshot_json, attempt
ON k12_image_task_invocations
BEGIN
    SELECT RAISE(ABORT, 'image task invocation identity is immutable');
END;
`

func migrateK12ImageTasksV32(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 ImageTaskDispatch 迁移事务: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, k12ImageTasksV32DDL); err != nil {
		return fmt.Errorf("创建 ImageTaskDispatch/CreativeWorkIntake: %w", err)
	}

	for _, column := range []struct {
		table string
		name  string
		def   string
	}{
		{"k12_creative_works", "display_name", "TEXT NOT NULL DEFAULT ''"},
		{"k12_creative_works", "work_title", "TEXT NOT NULL DEFAULT ''"},
		{"k12_creative_works", "task_requirement", "TEXT NOT NULL DEFAULT ''"},
		{"k12_creative_works", "title_task_provenance_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"k12_creative_works", "source_intake_id", "TEXT NOT NULL DEFAULT ''"},
		{"k12_creative_work_ocr_jobs", "route_policy_snapshot_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"k12_creative_work_ocr_jobs", "route_snapshot_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"k12_creative_work_ocr_jobs", "invocation_id", "TEXT NOT NULL DEFAULT ''"},
		{"k12_work_feedback", "route_snapshot_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"k12_work_feedback", "invocation_id", "TEXT NOT NULL DEFAULT ''"},
	} {
		tableExists, err := txTableExists(ctx, tx, column.table)
		if err != nil {
			return fmt.Errorf("检查表 %s: %w", column.table, err)
		}
		if !tableExists {
			continue
		}
		has, err := txColumnExists(ctx, tx, column.table, column.name)
		if err != nil {
			return fmt.Errorf("检查 %s.%s: %w", column.table, column.name, err)
		}
		if has {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN %s %s`, column.table, column.name, column.def)); err != nil {
			return fmt.Errorf("新增 %s.%s: %w", column.table, column.name, err)
		}
	}

	hasCreativeWorks, err := txTableExists(ctx, tx, "k12_creative_works")
	if err != nil {
		return fmt.Errorf("检查 k12_creative_works: %w", err)
	}
	if hasCreativeWorks {
		if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_works
        SET work_title=title,
            task_requirement=task,
            display_name=CASE
                WHEN trim(title) != '' THEN title
                WHEN work_type='writing' THEN '语文写作'
                WHEN work_type='art' THEN '美术作品'
                ELSE ''
            END
        WHERE display_name=''`); err != nil {
			return fmt.Errorf("回填 CreativeWork 显示名与历史事实: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX idx_k12_creative_work_source_intake
        ON k12_creative_works(agent_name, source_intake_id) WHERE source_intake_id != ''`); err != nil {
			return fmt.Errorf("创建 CreativeWork intake 唯一索引: %w", err)
		}
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 ImageTaskDispatch 迁移: %w", err)
	}
	return nil
}

func txTableExists(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&count)
	return count == 1, err
}

func txColumnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
