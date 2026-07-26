package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12WeeklyPracticeV52 adds the canonical weekly-practice aggregates. All
// tables are additive; existing profile metadata and printable artifacts remain
// their respective sources of truth.
var K12WeeklyPracticeV52 = Migration{
	Version:     52,
	Description: "K12 本周该练：档案三 revision CAS、三轨计划、不可变快照与副作用收据",
	AtomicFunc:  migrateK12WeeklyPracticeV52,
}

func migrateK12WeeklyPracticeV52(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取 V52 迁移连接: %w", err)
	}
	defer conn.Close()
	var foreignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("读取 V52 foreign_keys: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("关闭 V52 foreign_keys: %w", err)
	}
	defer func() {
		if foreignKeys != 0 {
			if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); retErr == nil && err != nil {
				retErr = fmt.Errorf("恢复 V52 foreign_keys: %w", err)
			}
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 V52 事务: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, K12WeeklyPracticePrintArtifactDDL); err != nil {
		return fmt.Errorf("扩展 V52 PrintableArtifact source_kind: %w", err)
	}
	if _, err := tx.ExecContext(ctx, K12WeeklyPracticeV52DDL); err != nil {
		return fmt.Errorf("创建 V52 周练表: %w", err)
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return fmt.Errorf("检查 V52 外键: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("V52 外键检查发现 %d 个冲突", violations)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 V52 迁移: %w", err)
	}
	return nil
}

const K12WeeklyPracticePrintArtifactDDL = `
DROP TABLE IF EXISTS k12_print_artifacts_v52;
CREATE TABLE k12_print_artifacts_v52 (
    artifact_id        TEXT    PRIMARY KEY,
    agent_name         TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    source_kind        TEXT    NOT NULL CHECK(source_kind IN
        ('tutoring_tips','creative_observation_card','practice_question',
         'practice_answer','grading_final_artifact','weekly_practice_snapshot')),
    source_ref         TEXT    NOT NULL CHECK(length(trim(source_ref)) BETWEEN 1 AND 512),
    title              TEXT    NOT NULL CHECK(length(trim(title)) BETWEEN 1 AND 256),
    canonical_markdown TEXT    NOT NULL CHECK(length(trim(canonical_markdown)) BETWEEN 1 AND 4194304),
    source_digest      TEXT    NOT NULL CHECK(length(source_digest) = 64),
    created_at         INTEGER NOT NULL CHECK(created_at > 0),
    UNIQUE(agent_name, source_kind, source_ref, source_digest)
);
INSERT INTO k12_print_artifacts_v52 (
    artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,
    source_digest,created_at
)
SELECT artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,
       source_digest,created_at
FROM k12_print_artifacts;
DROP TRIGGER IF EXISTS trg_k12_print_artifacts_immutable;
DROP TABLE k12_print_artifacts;
ALTER TABLE k12_print_artifacts_v52 RENAME TO k12_print_artifacts;
CREATE INDEX IF NOT EXISTS idx_k12_print_artifacts_source
    ON k12_print_artifacts(agent_name, source_kind, source_ref, created_at);
CREATE TRIGGER IF NOT EXISTS trg_k12_print_artifacts_immutable
BEFORE UPDATE ON k12_print_artifacts
BEGIN
    SELECT RAISE(ABORT, 'k12 print artifact is immutable');
END;
`

const K12WeeklyPracticeV52DDL = `
CREATE TABLE IF NOT EXISTS k12_profile_revisions (
    agent_name TEXT PRIMARY KEY,
    revision INTEGER NOT NULL CHECK(revision >= 1),
    updated_at INTEGER NOT NULL CHECK(updated_at > 0),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS trg_k12_profile_revision_after_metadata_update
AFTER UPDATE OF metadata ON agents
FOR EACH ROW
BEGIN
    INSERT INTO k12_profile_revisions(agent_name,revision,updated_at)
    VALUES(NEW.name,1,MAX(1,CAST(strftime('%s','now') AS INTEGER)))
    ON CONFLICT(agent_name) DO UPDATE SET
        revision=k12_profile_revisions.revision+1,
        updated_at=excluded.updated_at;
END;

CREATE TABLE IF NOT EXISTS k12_curriculum_progress (
    progress_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    subject TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(revision >= 1),
    textbook_binding_id TEXT NOT NULL,
    textbook_edition TEXT NOT NULL,
    textbook_version TEXT NOT NULL,
    title TEXT NOT NULL,
    volume TEXT NOT NULL,
    unit_id TEXT NOT NULL,
    unit_title TEXT NOT NULL,
    lesson_id TEXT NOT NULL DEFAULT '',
    lesson_title TEXT NOT NULL DEFAULT '',
    requested_page_from INTEGER,
    requested_page_to INTEGER,
    verified_page_from INTEGER,
    verified_page_to INTEGER,
    page_verification_status TEXT NOT NULL CHECK(page_verification_status IN
        ('not_requested','verified','partially_verified','rejected')),
    segment_refs_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(segment_refs_json)),
    evidence_source TEXT NOT NULL CHECK(evidence_source='parent_confirmed'),
    confirmed_at INTEGER NOT NULL CHECK(confirmed_at > 0),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    UNIQUE(agent_name,subject),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_weekly_practice_settings (
    agent_name TEXT PRIMARY KEY,
    revision INTEGER NOT NULL CHECK(revision >= 1),
    timezone TEXT NOT NULL,
    due_review_enabled INTEGER NOT NULL DEFAULT 1 CHECK(due_review_enabled=1),
    textbook_consolidation_enabled INTEGER NOT NULL DEFAULT 0
        CHECK(textbook_consolidation_enabled IN (0,1)),
    arithmetic_warmup_enabled INTEGER NOT NULL DEFAULT 0
        CHECK(arithmetic_warmup_enabled IN (0,1)),
    arithmetic_minutes INTEGER NOT NULL DEFAULT 2 CHECK(arithmetic_minutes BETWEEN 1 AND 5),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_profile_bundle_commands (
    agent_name TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    response_json TEXT NOT NULL CHECK(json_valid(response_json)),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(agent_name,idempotency_key),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_weekly_practice_plans (
    plan_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(revision >= 1),
    iso_week_year INTEGER NOT NULL,
    iso_week_number INTEGER NOT NULL CHECK(iso_week_number BETWEEN 1 AND 53),
    timezone TEXT NOT NULL,
    week_start INTEGER NOT NULL CHECK(week_start > 0),
    week_end INTEGER NOT NULL CHECK(week_end >= week_start),
    local_start_date TEXT NOT NULL,
    local_end_date TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('draft','frozen','archived','expired_unused')),
    settings_revision INTEGER NOT NULL CHECK(settings_revision >= 0),
    curriculum_progress_revision INTEGER,
    source_digest TEXT NOT NULL,
    plan_json TEXT NOT NULL CHECK(json_valid(plan_json)),
    answer_keys_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(answer_keys_json)),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    UNIQUE(agent_name,iso_week_year,iso_week_number,timezone),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_weekly_practice_plan_commands (
    agent_name TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    plan_revision INTEGER NOT NULL,
    response_json TEXT NOT NULL CHECK(json_valid(response_json)),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(agent_name,idempotency_key),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE,
    FOREIGN KEY(plan_id) REFERENCES k12_weekly_practice_plans(plan_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_weekly_practice_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    plan_id TEXT NOT NULL,
    plan_revision INTEGER NOT NULL CHECK(plan_revision >= 1),
    agent_name TEXT NOT NULL,
    snapshot_digest TEXT NOT NULL,
    snapshot_json TEXT NOT NULL CHECK(json_valid(snapshot_json)),
    answer_keys_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(answer_keys_json)),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    UNIQUE(plan_id,plan_revision),
    FOREIGN KEY(plan_id) REFERENCES k12_weekly_practice_plans(plan_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_weekly_practice_attempts (
    attempt_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    item_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    attempt_json TEXT NOT NULL CHECK(json_valid(attempt_json)),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    UNIQUE(snapshot_id,item_id,idempotency_key),
    FOREIGN KEY(snapshot_id) REFERENCES k12_weekly_practice_snapshots(snapshot_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_weekly_practice_saves (
    save_receipt_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    plan_revision INTEGER NOT NULL CHECK(plan_revision >= 1),
    snapshot_id TEXT NOT NULL,
    practice_set_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    receipt_json TEXT NOT NULL CHECK(json_valid(receipt_json)),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    UNIQUE(plan_id,plan_revision),
    UNIQUE(agent_name,idempotency_key),
    FOREIGN KEY(plan_id) REFERENCES k12_weekly_practice_plans(plan_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id) REFERENCES k12_weekly_practice_snapshots(snapshot_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS k12_weekly_practice_sends (
    agent_name TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    delivery_batch_id TEXT NOT NULL,
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(agent_name,idempotency_key),
    FOREIGN KEY(snapshot_id) REFERENCES k12_weekly_practice_snapshots(snapshot_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);
`
