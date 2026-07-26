package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12SinglePracticeGenerationV39 adds the durable identity and projection
// fields required by the one-click mistake -> practice generation state
// machine. It is additive: existing custom-paper jobs and ready items retain
// their pre-V39 semantics.
var K12SinglePracticeGenerationV39 = Migration{
	Version:     39,
	Description: "v0.5.0 错题一键异步加入练习集",
	AtomicFunc:  migrateK12SinglePracticeGenerationV39,
}

func migrateK12SinglePracticeGenerationV39(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启错题异步练习迁移事务: %w", err)
	}
	defer tx.Rollback()

	if exists, err := txTableExists(ctx, tx, "k12_practice_set_items"); err != nil {
		return fmt.Errorf("检查 k12_practice_set_items: %w", err)
	} else if exists {
		for _, column := range []struct {
			name string
			def  string
		}{
			{"source_mistake_summary", "TEXT NOT NULL DEFAULT ''"},
			{"generation_status", "TEXT NOT NULL DEFAULT ''"},
		} {
			has, err := txColumnExists(ctx, tx, "k12_practice_set_items", column.name)
			if err != nil {
				return fmt.Errorf("检查 k12_practice_set_items.%s: %w", column.name, err)
			}
			if !has {
				if _, err := tx.ExecContext(ctx, fmt.Sprintf(
					`ALTER TABLE k12_practice_set_items ADD COLUMN %s %s`,
					column.name, column.def,
				)); err != nil {
					return fmt.Errorf("新增 k12_practice_set_items.%s: %w", column.name, err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS
			idx_k12_single_generation_item
			ON k12_practice_set_items(generation_job_id)
			WHERE generation_job_id != '' AND added_via = 'single_variant'`); err != nil {
			return fmt.Errorf("创建逐题 generation/item 唯一索引: %w", err)
		}
	}

	if exists, err := txTableExists(ctx, tx, "k12_practice_generation_jobs"); err != nil {
		return fmt.Errorf("检查 k12_practice_generation_jobs: %w", err)
	} else if exists {
		for _, column := range []struct {
			name string
			def  string
		}{
			{"source_mistake_id", "TEXT NOT NULL DEFAULT ''"},
			{"source_mistake_summary", "TEXT NOT NULL DEFAULT ''"},
			{"request_snapshot_json", "TEXT NOT NULL DEFAULT '{}'"},
			{"route_snapshot_json", "TEXT NOT NULL DEFAULT '{}'"},
			{"attempt", "INTEGER NOT NULL DEFAULT 0"},
			{"generation_output_json", "TEXT NOT NULL DEFAULT ''"},
			{"generation_output_attempt", "INTEGER NOT NULL DEFAULT 0"},
			{"validation_output_json", "TEXT NOT NULL DEFAULT ''"},
			{"validation_output_attempt", "INTEGER NOT NULL DEFAULT 0"},
			{"retired_at", "INTEGER NOT NULL DEFAULT 0"},
			{"retired_reason", "TEXT NOT NULL DEFAULT ''"},
		} {
			has, err := txColumnExists(ctx, tx, "k12_practice_generation_jobs", column.name)
			if err != nil {
				return fmt.Errorf("检查 k12_practice_generation_jobs.%s: %w", column.name, err)
			}
			if !has {
				if _, err := tx.ExecContext(ctx, fmt.Sprintf(
					`ALTER TABLE k12_practice_generation_jobs ADD COLUMN %s %s`,
					column.name, column.def,
				)); err != nil {
					return fmt.Errorf("新增 k12_practice_generation_jobs.%s: %w", column.name, err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS
			idx_k12_single_generation_active_source
			ON k12_practice_generation_jobs(agent_name, source_mistake_id)
			WHERE scope = 'single'
			  AND source_mistake_id != ''
			  AND retired_at = 0
			  AND status IN ('queued','generating','validating')`); err != nil {
			return fmt.Errorf("创建逐题 active source 唯一索引: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS
			idx_k12_single_generation_source_history
			ON k12_practice_generation_jobs(agent_name, source_mistake_id, created_at DESC)
			WHERE scope = 'single' AND source_mistake_id != ''`); err != nil {
			return fmt.Errorf("创建逐题 source 历史索引: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS k12_practice_generation_invocations (
				invocation_id TEXT PRIMARY KEY,
				agent_name TEXT NOT NULL,
				job_id TEXT NOT NULL,
				stage TEXT NOT NULL CHECK(stage IN ('practice_generate','practice_validate')),
				request_digest TEXT NOT NULL,
				provider TEXT NOT NULL,
				model TEXT NOT NULL,
				route_snapshot_json TEXT NOT NULL,
				provider_idempotency_key TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL CHECK(status IN (
					'prepared','sent','succeeded','failed','outcome_unknown','reconciled'
				)),
				attempt INTEGER NOT NULL CHECK(attempt >= 1),
				result_digest TEXT NOT NULL DEFAULT '',
				external_request_id TEXT NOT NULL DEFAULT '',
				failure_kind TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				UNIQUE(job_id, stage, attempt),
				FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE,
				FOREIGN KEY(job_id) REFERENCES k12_practice_generation_jobs(generation_job_id)
					ON DELETE CASCADE
			);
			CREATE INDEX IF NOT EXISTS idx_k12_practice_generation_invocations_job
				ON k12_practice_generation_invocations(agent_name, job_id, stage, attempt);
			CREATE INDEX IF NOT EXISTS idx_k12_practice_generation_invocations_status
				ON k12_practice_generation_invocations(status, updated_at);
		`); err != nil {
			return fmt.Errorf("创建逐题模型调用账本: %w", err)
		}
	}

	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
