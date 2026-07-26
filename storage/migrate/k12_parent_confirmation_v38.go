package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ParentConfirmationV38 separates a parent's subjective confirmation from
// evidence-backed mastery. Existing rows deliberately remain unconfirmed.
var K12ParentConfirmationV38 = Migration{
	Version:     38,
	Description: "v0.5.0 家长确认事实与系统掌握证据分离",
	AtomicFunc:  migrateK12ParentConfirmationV38,
}

func migrateK12ParentConfirmationV38(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启家长确认迁移事务: %w", err)
	}
	defer tx.Rollback()
	exists, err := txTableExists(ctx, tx, "k12_mistakes")
	if err != nil {
		return fmt.Errorf("检查 k12_mistakes: %w", err)
	}
	if exists {
		has, err := txColumnExists(ctx, tx, "k12_mistakes", "parent_confirmed_at")
		if err != nil {
			return fmt.Errorf("检查 k12_mistakes.parent_confirmed_at: %w", err)
		}
		if !has {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE k12_mistakes
				ADD COLUMN parent_confirmed_at INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("新增 k12_mistakes.parent_confirmed_at: %w", err)
			}
		}
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
