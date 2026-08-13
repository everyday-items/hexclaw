package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// K12CurriculumProgressRevisionV78 为可空的当前进度投影新增独立生命周期时钟。
// 当前投影不存在时不写入 head；已有投影则按其当前 revision 原样回填。
var K12CurriculumProgressRevisionV78 = Migration{
	Version:     78,
	Description: "K12 nullable curriculum progress lifecycle revision",
	AtomicFunc:  migrateK12CurriculumProgressRevisionV78,
}

const k12CurriculumProgressRevisionV78DDL = `
CREATE TABLE IF NOT EXISTS k12_curriculum_progress_revisions (
    agent_name TEXT NOT NULL,
    subject TEXT NOT NULL CHECK(subject = 'math'),
    revision INTEGER NOT NULL CHECK(revision >= 1),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(agent_name,subject),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);`

func migrateK12CurriculumProgressRevisionV78(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 curriculum progress revision V78 migration: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("roll back V78 migration transaction: %w", rollbackErr))
		}
	}()

	if _, createErr := tx.ExecContext(ctx, k12CurriculumProgressRevisionV78DDL); createErr != nil {
		return fmt.Errorf("create K12 curriculum progress revision table: %w", createErr)
	}
	progressExists, err := txTableExists(ctx, tx, "k12_curriculum_progress")
	if err != nil {
		return fmt.Errorf("check V78 curriculum progress table: %w", err)
	}
	if progressExists {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO k12_curriculum_progress_revisions(
				agent_name,subject,revision,updated_at
			)
			SELECT agent_name,subject,revision,updated_at
			FROM k12_curriculum_progress
			WHERE subject='math'`); err != nil {
			return fmt.Errorf("backfill K12 curriculum progress revision heads: %w", err)
		}
	}

	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 curriculum progress revision V78 migration: %w", err)
	}
	return nil
}
