package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// K12GeneralGuidanceFinalArtifactV80 允许没有可信教材知识点的 text webhook
// 固化题目级普通指导，同时保持完整辅导要点与跳题产物的既有约束。
var K12GeneralGuidanceFinalArtifactV80 = Migration{
	Version:     80,
	Description: "K12 text webhook non-textbook general guidance final artifact",
	AtomicFunc:  migrateK12GeneralGuidanceFinalArtifactV80,
}

const k12GeneralGuidanceFinalArtifactV80DDL = `
CREATE TABLE k12_grading_final_artifacts_v80 (
    artifact_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    coverage_status TEXT NOT NULL
        CHECK(coverage_status IN ('complete','with_skips','general_guidance')),
    total_count INTEGER NOT NULL CHECK(total_count >= 1),
    published_count INTEGER NOT NULL CHECK(published_count >= 0),
    skipped_count INTEGER NOT NULL CHECK(skipped_count >= 0),
    ordered_current_digests_json TEXT NOT NULL
        CHECK(
            json_valid(ordered_current_digests_json) AND
            json_type(ordered_current_digests_json) = 'array'
        ),
    canonical_markdown TEXT NOT NULL CHECK(length(trim(canonical_markdown)) > 0),
    artifact_digest TEXT NOT NULL CHECK(length(artifact_digest) = 64),
    summary_invocation_id TEXT NOT NULL,
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    finalization_generation INTEGER NOT NULL DEFAULT 0
        CHECK(finalization_generation >= 0),
    UNIQUE(agent_name,job_id),
    UNIQUE(agent_name,job_id,structure_version,artifact_digest),
    FOREIGN KEY(agent_name,job_id)
        REFERENCES k12_grading_jobs(agent_name,record_id) ON DELETE CASCADE,
    CHECK(published_count + skipped_count = total_count),
    CHECK(
        (coverage_status = 'complete' AND
            published_count = total_count AND skipped_count = 0 AND
            length(trim(summary_invocation_id)) > 0)
        OR
        (coverage_status = 'with_skips' AND skipped_count > 0 AND
            length(trim(summary_invocation_id)) = 0)
        OR
        (coverage_status = 'general_guidance' AND
            published_count = total_count AND skipped_count = 0 AND
            length(trim(summary_invocation_id)) = 0)
    )
);`

const k12GeneralGuidanceFinalArtifactV80PostDDL = `
ALTER TABLE k12_grading_final_artifacts_v80
    RENAME TO k12_grading_final_artifacts;
CREATE INDEX idx_k12_grading_final_artifacts_digest
    ON k12_grading_final_artifacts(
        agent_name,job_id,structure_version,artifact_digest
    );`

func migrateK12GeneralGuidanceFinalArtifactV80(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 general guidance V80 migration: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil &&
			!errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf(
				"roll back K12 general guidance V80 migration: %w", rollbackErr,
			))
		}
	}()

	exists, err := txTableExists(ctx, tx, "k12_grading_final_artifacts")
	if err != nil {
		return fmt.Errorf("check K12 final artifact table for V80: %w", err)
	}
	if !exists {
		if err := recordVersion(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS k12_grading_final_artifacts_v80`); err != nil {
		return fmt.Errorf("drop stale K12 V80 final artifact table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, k12GeneralGuidanceFinalArtifactV80DDL); err != nil {
		return fmt.Errorf("create K12 V80 final artifact table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_grading_final_artifacts_v80 (
        artifact_id,agent_name,job_id,structure_version,coverage_status,
        total_count,published_count,skipped_count,ordered_current_digests_json,
        canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at,
        finalization_generation
    )
    SELECT artifact_id,agent_name,job_id,structure_version,coverage_status,
        total_count,published_count,skipped_count,ordered_current_digests_json,
        canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at,
        finalization_generation
    FROM k12_grading_final_artifacts`); err != nil {
		return fmt.Errorf("copy K12 V80 final artifacts: %w", err)
	}
	for _, statement := range []string{
		`DROP TABLE k12_grading_final_artifacts`,
		k12GeneralGuidanceFinalArtifactV80PostDDL,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("activate K12 V80 final artifact schema: %w", err)
		}
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return fmt.Errorf("check K12 V80 final artifact foreign keys: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("K12 V80 final artifact foreign-key check found %d conflicts", violations)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 general guidance V80 migration: %w", err)
	}
	return nil
}
