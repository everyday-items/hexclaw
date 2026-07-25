package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// K12CreativeAccumulationV37 introduces the current CreativeWork feedback
// generations, server-derived Accumulation metadata, durable dictation
// generations, and CAS/idempotent tombstones. It is deliberately additive:
// every legacy table and row remains available to the read adapter.
var K12CreativeAccumulationV37 = Migration{
	Version:     37,
	Description: "v0.5.0 作品点评代次、积累派生与危险删除 tombstone",
	AtomicFunc:  migrateK12CreativeAccumulationV37,
}

func migrateK12CreativeAccumulationV37(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 K12 作品/积累 V37 迁移事务: %w", err)
	}
	defer tx.Rollback()

	worksExist, err := txTableExists(ctx, tx, "k12_creative_works")
	if err != nil {
		return fmt.Errorf("检查 k12_creative_works: %w", err)
	}
	accumulationsExist, err := txTableExists(ctx, tx, "k12_accumulations")
	if err != nil {
		return fmt.Errorf("检查 k12_accumulations: %w", err)
	}
	if worksExist || accumulationsExist {
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS k12_current_create_receipts (
    agent_name      TEXT NOT NULL REFERENCES agents(name),
    object_kind     TEXT NOT NULL
                         CHECK(object_kind IN ('creative_work','accumulation')),
    command_key     TEXT NOT NULL,
    request_digest  TEXT NOT NULL,
    object_id       TEXT NOT NULL,
    receipt_json    TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    PRIMARY KEY(agent_name, object_kind, command_key)
);
CREATE INDEX IF NOT EXISTS idx_k12_current_create_receipt_object
    ON k12_current_create_receipts(agent_name, object_kind, object_id);`); err != nil {
			return fmt.Errorf("创建当前 K12 create command receipt: %w", err)
		}
	}

	if worksExist {
		if err := addV37Columns(ctx, tx, "k12_creative_works", []v37Column{
			{"initial_feedback_generation_id", "TEXT NOT NULL DEFAULT ''"},
			{"latest_feedback_generation_id", "TEXT NOT NULL DEFAULT ''"},
			{"feedback_state", "TEXT NOT NULL DEFAULT '' CHECK(feedback_state IN ('','queued','running','succeeded','failed'))"},
			{"row_version", "INTEGER NOT NULL DEFAULT 1"},
			{"deleted_at", "INTEGER"},
			{"deleted_by", "TEXT NOT NULL DEFAULT ''"},
			{"delete_command_key", "TEXT NOT NULL DEFAULT ''"},
			{"delete_receipt_json", "TEXT NOT NULL DEFAULT ''"},
		}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS k12_work_feedback_generations (
    generation_id           TEXT PRIMARY KEY,
    work_id                 TEXT NOT NULL REFERENCES k12_creative_works(record_id),
    agent_name              TEXT NOT NULL REFERENCES agents(name),
    generation_no           INTEGER NOT NULL CHECK(generation_no > 0),
    command_key             TEXT NOT NULL,
    request_digest          TEXT NOT NULL,
    status                  TEXT NOT NULL DEFAULT 'queued'
                                CHECK(status IN ('queued','running','succeeded','failed')),
    feedback_type           TEXT NOT NULL DEFAULT '',
    source_snapshot_json    TEXT NOT NULL DEFAULT '{}',
    request_snapshot_json   TEXT NOT NULL DEFAULT '{}',
    route_snapshot_json     TEXT NOT NULL DEFAULT '{}',
    invocation_snapshot_json TEXT NOT NULL DEFAULT '{}',
    feedback_json           TEXT NOT NULL DEFAULT '',
    projection_markdown     TEXT NOT NULL DEFAULT '',
    failure_reason          TEXT NOT NULL DEFAULT '',
    attempt                 INTEGER NOT NULL DEFAULT 0,
    created_at              INTEGER NOT NULL,
    updated_at              INTEGER NOT NULL,
    UNIQUE(work_id, generation_no),
    UNIQUE(work_id, command_key)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_work_feedback_one_active
    ON k12_work_feedback_generations(work_id)
    WHERE status IN ('queued','running');
CREATE INDEX IF NOT EXISTS idx_k12_work_feedback_owner_work
    ON k12_work_feedback_generations(agent_name, work_id, generation_no);
CREATE INDEX IF NOT EXISTS idx_k12_creative_works_current
    ON k12_creative_works(agent_name, created_at)
    WHERE deleted_at IS NULL;
CREATE TRIGGER IF NOT EXISTS k12_work_feedback_generation_identity_immutable
BEFORE UPDATE OF generation_id, work_id, agent_name, generation_no, command_key,
                 request_digest, feedback_type, source_snapshot_json,
                 request_snapshot_json, route_snapshot_json
ON k12_work_feedback_generations
BEGIN
    SELECT RAISE(ABORT, 'work feedback generation identity is immutable');
END;`); err != nil {
			return fmt.Errorf("创建作品点评 generation: %w", err)
		}
		if err := backfillV37LegacyWorkFeedback(ctx, tx); err != nil {
			return err
		}
	}

	if accumulationsExist {
		if err := addV37Columns(ctx, tx, "k12_accumulations", []v37Column{
			{"derived_source_ref", "TEXT"},
			{"subject_provenance_json", "TEXT NOT NULL DEFAULT ''"},
			{"entry_type_provenance_json", "TEXT NOT NULL DEFAULT ''"},
			{"source_provenance_json", "TEXT NOT NULL DEFAULT ''"},
			{"row_version", "INTEGER NOT NULL DEFAULT 1"},
			{"deleted_at", "INTEGER"},
			{"deleted_by", "TEXT NOT NULL DEFAULT ''"},
			{"delete_command_key", "TEXT NOT NULL DEFAULT ''"},
			{"delete_receipt_json", "TEXT NOT NULL DEFAULT ''"},
		}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS k12_accumulation_dictation_generations (
    generation_id            TEXT PRIMARY KEY,
    accumulation_id          TEXT NOT NULL REFERENCES k12_accumulations(record_id),
    agent_name               TEXT NOT NULL REFERENCES agents(name),
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
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_accum_dictation_practice_item
    ON k12_accumulation_dictation_generations(practice_item_id)
    WHERE practice_item_id != '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_accum_dictation_one_active
    ON k12_accumulation_dictation_generations(accumulation_id)
    WHERE status IN ('queued','generating','validating');
CREATE INDEX IF NOT EXISTS idx_k12_accum_dictation_owner
    ON k12_accumulation_dictation_generations(agent_name, accumulation_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_k12_accumulations_current
    ON k12_accumulations(agent_name, created_at)
    WHERE deleted_at IS NULL;
CREATE TRIGGER IF NOT EXISTS k12_accum_dictation_identity_immutable
BEFORE UPDATE OF generation_id, accumulation_id, agent_name, command_key,
                 request_digest, source_snapshot_json, route_snapshot_json
ON k12_accumulation_dictation_generations
BEGIN
    SELECT RAISE(ABORT, 'accumulation dictation generation identity is immutable');
END;`); err != nil {
			return fmt.Errorf("创建积累默写 generation: %w", err)
		}
		// Empty legacy source_ref means “unknown”, not a real empty-string
		// source. Non-empty legacy values remain in their original column and
		// are exposed only by the compatibility reader.
		if _, err := tx.ExecContext(ctx, `UPDATE k12_accumulations
			SET derived_source_ref = NULL
			WHERE trim(source_ref) = ''`); err != nil {
			return fmt.Errorf("规范积累空来源兼容值: %w", err)
		}
	}

	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

type v37Column struct {
	name string
	def  string
}

func addV37Columns(
	ctx context.Context,
	tx *sql.Tx,
	table string,
	columns []v37Column,
) error {
	for _, column := range columns {
		has, err := txColumnExists(ctx, tx, table, column.name)
		if err != nil {
			return fmt.Errorf("检查 %s.%s: %w", table, column.name, err)
		}
		if has {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN %s %s`, table, column.name, column.def,
		)); err != nil {
			return fmt.Errorf("新增 %s.%s: %w", table, column.name, err)
		}
	}
	return nil
}

func backfillV37LegacyWorkFeedback(ctx context.Context, tx *sql.Tx) error {
	legacyExists, err := txTableExists(ctx, tx, "k12_work_feedback")
	if err != nil {
		return fmt.Errorf("检查 k12_work_feedback: %w", err)
	}
	if !legacyExists {
		return nil
	}
	workTypeExists, err := txColumnExists(ctx, tx, "k12_creative_works", "work_type")
	if err != nil {
		return fmt.Errorf("检查 k12_creative_works.work_type: %w", err)
	}
	workTypeExpr := `''`
	if workTypeExists {
		workTypeExpr = `w.work_type`
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
SELECT f.work_record_id, w.agent_name, f.version_index, f.feedback_markdown,
       f.feedback_source, f.feedback_skill, %s, w.created_at, w.updated_at
FROM k12_work_feedback f
JOIN k12_creative_works w ON w.record_id=f.work_record_id
ORDER BY f.work_record_id, f.version_index`, workTypeExpr))
	if err != nil {
		return fmt.Errorf("读取 legacy 作品点评: %w", err)
	}
	type legacyFeedback struct {
		workID, agentName, markdown, source, skill, workType string
		versionIndex, createdAt, updatedAt                   int64
	}
	var legacy []legacyFeedback
	for rows.Next() {
		var item legacyFeedback
		if err := rows.Scan(
			&item.workID, &item.agentName, &item.versionIndex, &item.markdown,
			&item.source, &item.skill, &item.workType, &item.createdAt, &item.updatedAt,
		); err != nil {
			rows.Close()
			return fmt.Errorf("扫描 legacy 作品点评: %w", err)
		}
		legacy = append(legacy, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	currentWork := ""
	generationNo := 0
	for _, item := range legacy {
		if item.workID != currentWork {
			currentWork = item.workID
			generationNo = 0
		}
		generationNo++
		generationID := fmt.Sprintf("legacy:%s:%d", item.workID, item.versionIndex)
		sum := sha256.Sum256([]byte(item.markdown))
		digest := "sha256:" + hex.EncodeToString(sum[:])
		sourceSnapshot := fmt.Sprintf(
			`{"migration":"v37","legacy_version_index":%d,"feedback_source":%q,"feedback_skill":%q}`,
			item.versionIndex, item.source, item.skill,
		)
		if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO k12_work_feedback_generations (
    generation_id, work_id, agent_name, generation_no, command_key,
    request_digest, status, feedback_type, source_snapshot_json,
    request_snapshot_json, route_snapshot_json, invocation_snapshot_json,
    feedback_json, projection_markdown, failure_reason, attempt,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, 'succeeded', ?, ?, '{}', '{}', '{}', '', ?, '', 1, ?, ?)`,
			generationID, item.workID, item.agentName, generationNo,
			fmt.Sprintf("legacy:%d", item.versionIndex), digest, item.workType,
			sourceSnapshot, item.markdown, item.createdAt, item.updatedAt,
		); err != nil {
			return fmt.Errorf("回填 legacy 作品点评 %s/%d: %w", item.workID, item.versionIndex, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE k12_creative_works
SET initial_feedback_generation_id = COALESCE((
        SELECT generation_id FROM k12_work_feedback_generations g
        WHERE g.work_id=k12_creative_works.record_id
        ORDER BY generation_no ASC LIMIT 1
    ), initial_feedback_generation_id),
    latest_feedback_generation_id = COALESCE((
        SELECT generation_id FROM k12_work_feedback_generations g
        WHERE g.work_id=k12_creative_works.record_id AND g.status='succeeded'
        ORDER BY generation_no DESC LIMIT 1
    ), latest_feedback_generation_id),
    feedback_state = CASE WHEN EXISTS (
        SELECT 1 FROM k12_work_feedback_generations g
        WHERE g.work_id=k12_creative_works.record_id AND g.status='succeeded'
    ) THEN 'succeeded' ELSE feedback_state END
WHERE EXISTS (
    SELECT 1 FROM k12_work_feedback_generations g
    WHERE g.work_id=k12_creative_works.record_id
)`); err != nil {
		return fmt.Errorf("回填作品点评 generation 指针: %w", err)
	}
	return nil
}
