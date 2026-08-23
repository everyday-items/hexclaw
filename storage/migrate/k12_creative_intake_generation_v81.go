package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12CreativeIntakeGenerationV81 为当前作品接入保存首轮点评 generation 引用；
// 历史 promoted_version_id 继续保留，只用于旧数据读取兼容。
var K12CreativeIntakeGenerationV81 = Migration{
	Version:     81,
	Description: "K12 current creative intake promoted generation identity",
	AtomicFunc:  migrateK12CreativeIntakeGenerationV81,
}

func migrateK12CreativeIntakeGenerationV81(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 creative intake generation V81 migration: %w", err)
	}
	defer tx.Rollback()

	hasColumn, err := txColumnExists(
		ctx, tx, "k12_creative_work_intakes", "promoted_generation_id",
	)
	if err != nil {
		return fmt.Errorf("check K12 creative intake generation column: %w", err)
	}
	if !hasColumn {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE k12_creative_work_intakes
			ADD COLUMN promoted_generation_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add K12 creative intake generation column: %w", err)
		}
	}

	// 仅能从独立作品的首轮点评指针回填；历史 revision 不冒充当前 generation。
	if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
		SET promoted_generation_id=COALESCE((
			SELECT initial_feedback_generation_id
			FROM k12_creative_works
			WHERE record_id=k12_creative_work_intakes.promoted_work_id
		), '')
		WHERE status='promoted'
		  AND entry_kind IN ('auto','new_work')
		  AND promoted_generation_id=''`); err != nil {
		return fmt.Errorf("backfill K12 creative intake generation identity: %w", err)
	}

	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 creative intake generation V81 migration: %w", err)
	}
	return nil
}
