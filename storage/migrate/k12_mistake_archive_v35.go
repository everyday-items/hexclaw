package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12MistakeArchiveV35 adds the durable snapshot used by the controlled
// archive/restore commands. A zero archived_from_due_at represents an original
// nil due_at; real review due timestamps are positive unix seconds.
var K12MistakeArchiveV35 = Migration{
	Version:     35,
	Description: "v0.5.0 错题手动软归档、Undo 与长期恢复调度快照",
	AtomicFunc:  migrateK12MistakeArchiveV35,
}

func migrateK12MistakeArchiveV35(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启错题归档迁移事务: %w", err)
	}
	defer tx.Rollback()
	exists, err := txTableExists(ctx, tx, "k12_mistakes")
	if err != nil {
		return fmt.Errorf("检查 k12_mistakes: %w", err)
	}
	// 允许只构造某一后期子系统表的迁移审计库顺序推进；正式库的 V9 一定建表。
	if !exists {
		if err := recordVersion(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	for _, column := range []struct {
		name string
		def  string
	}{
		{"archived_reason", "TEXT NOT NULL DEFAULT ''"},
		{"archived_at", "INTEGER NOT NULL DEFAULT 0"},
		{"archive_command_id", "TEXT NOT NULL DEFAULT ''"},
		{"archived_from_status", "TEXT NOT NULL DEFAULT ''"},
		{"archived_from_due_at", "INTEGER NOT NULL DEFAULT 0"},
		{"archived_from_spot_check_state", "TEXT NOT NULL DEFAULT ''"},
		{"last_archive_snapshot_json", "TEXT NOT NULL DEFAULT ''"},
	} {
		has, checkErr := txColumnExists(ctx, tx, "k12_mistakes", column.name)
		if checkErr != nil {
			return fmt.Errorf("检查 k12_mistakes.%s: %w", column.name, checkErr)
		}
		if has {
			continue
		}
		if _, alterErr := tx.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE k12_mistakes ADD COLUMN %s %s`, column.name, column.def,
		)); alterErr != nil {
			return fmt.Errorf("新增 k12_mistakes.%s: %w", column.name, alterErr)
		}
	}
	if _, err := tx.ExecContext(ctx, `
CREATE TRIGGER IF NOT EXISTS k12_mistake_archive_reason_active_insert
BEFORE INSERT ON k12_mistakes
WHEN NEW.status <> 'archived' AND NEW.archived_reason <> ''
BEGIN
    SELECT RAISE(ABORT, 'active mistake cannot carry archived_reason');
END;
CREATE TRIGGER IF NOT EXISTS k12_mistake_archive_reason_active_update
BEFORE UPDATE OF status, archived_reason ON k12_mistakes
WHEN NEW.status <> 'archived' AND NEW.archived_reason <> ''
BEGIN
    SELECT RAISE(ABORT, 'active mistake cannot carry archived_reason');
END;`); err != nil {
		return fmt.Errorf("创建错题归档字段生命周期约束: %w", err)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
