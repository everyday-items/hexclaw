package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// K12LearningArchiveV82 为作品补充创建学期，并允许通用不可变 Artifact
// 承载学习档案 canonical Markdown。历史作品的 grade_term 保持空值。
var K12LearningArchiveV82 = Migration{
	Version:     82,
	Description: "K12 learning archive canonical artifact and creative grade term",
	AtomicFunc:  migrateK12LearningArchiveV82,
}

const k12LearningArchiveArtifactsV82DDL = `
DROP TABLE IF EXISTS k12_print_artifacts_v82;
CREATE TABLE k12_print_artifacts_v82 (
    artifact_id        TEXT    PRIMARY KEY,
    agent_name         TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    source_kind        TEXT    NOT NULL CHECK(source_kind IN
        ('tutoring_tips','creative_observation_card','practice_question',
         'practice_answer','grading_final_artifact','weekly_practice_snapshot',
         'learning_archive')),
    source_ref         TEXT    NOT NULL CHECK(length(trim(source_ref)) BETWEEN 1 AND 512),
    title              TEXT    NOT NULL CHECK(length(trim(title)) BETWEEN 1 AND 256),
    canonical_markdown TEXT    NOT NULL CHECK(length(trim(canonical_markdown)) BETWEEN 1 AND 4194304),
    source_digest      TEXT    NOT NULL CHECK(length(source_digest) = 64),
    created_at         INTEGER NOT NULL CHECK(created_at > 0),
    UNIQUE(agent_name, source_kind, source_ref, source_digest)
);
INSERT INTO k12_print_artifacts_v82 (
    artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,
    source_digest,created_at
)
SELECT artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,
       source_digest,created_at
FROM k12_print_artifacts;
DROP TRIGGER IF EXISTS trg_k12_print_artifacts_immutable;
DROP TABLE k12_print_artifacts;
ALTER TABLE k12_print_artifacts_v82 RENAME TO k12_print_artifacts;
CREATE INDEX IF NOT EXISTS idx_k12_print_artifacts_source
    ON k12_print_artifacts(agent_name, source_kind, source_ref, created_at);
CREATE TRIGGER IF NOT EXISTS trg_k12_print_artifacts_immutable
BEFORE UPDATE ON k12_print_artifacts
BEGIN
    SELECT RAISE(ABORT, 'k12 print artifact is immutable');
END;
`

func migrateK12LearningArchiveV82(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get V82 migration connection: %w", err)
	}
	defer conn.Close()

	var foreignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read V82 foreign_keys: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable V82 foreign_keys: %w", err)
	}
	defer func() {
		if foreignKeys != 0 {
			if _, restoreErr := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); retErr == nil && restoreErr != nil {
				retErr = fmt.Errorf("restore V82 foreign_keys: %w", restoreErr)
			}
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin V82 migration: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil &&
			!errors.Is(rollbackErr, sql.ErrTxDone) && retErr == nil {
			retErr = fmt.Errorf("roll back V82 migration: %w", rollbackErr)
		}
	}()

	hasGradeTerm, err := v82ColumnExists(ctx, tx, "k12_creative_works", "grade_term")
	if err != nil {
		return fmt.Errorf("inspect creative grade term for V82: %w", err)
	}
	if !hasGradeTerm {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE k12_creative_works
			ADD COLUMN grade_term TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add creative grade term for V82: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, k12LearningArchiveArtifactsV82DDL); err != nil {
		return fmt.Errorf("extend learning archive artifact source kind for V82: %w", err)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit V82 migration: %w", err)
	}
	return nil
}

func v82ColumnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
