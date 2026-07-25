package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ManualCreativeIntakeV34 adds the durable discriminators and receipt
// required to migrate the existing manual image upload path onto ImageTask.
// Historical automatic rows are backfilled as model_classified/automatic.
var K12ManualCreativeIntakeV34 = Migration{
	Version:     34,
	Description: "v0.5.0 手工作品图片 parent_selected/explicit_commit 接入与提交回执",
	AtomicFunc:  migrateK12ManualCreativeIntakeV34,
}

func migrateK12ManualCreativeIntakeV34(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启手工作品接入迁移事务: %w", err)
	}
	defer tx.Rollback()

	for _, column := range []struct {
		table string
		name  string
		def   string
	}{
		{"k12_image_task_dispatches", "routing_provenance",
			"TEXT NOT NULL DEFAULT 'model_classified' CHECK(routing_provenance IN ('model_classified','parent_selected'))"},
		{"k12_image_task_dispatches", "creative_entry_json",
			"TEXT NOT NULL DEFAULT ''"},
		{"k12_image_task_dispatches", "operation_route_request_json",
			"TEXT NOT NULL DEFAULT ''"},
		{"k12_creative_work_intakes", "entry_kind",
			"TEXT NOT NULL DEFAULT 'auto' CHECK(entry_kind IN ('auto','new_work','revision'))"},
		{"k12_creative_work_intakes", "promotion_policy",
			"TEXT NOT NULL DEFAULT 'automatic' CHECK(promotion_policy IN ('automatic','explicit_commit'))"},
		{"k12_creative_work_intakes", "target_work_id",
			"TEXT NOT NULL DEFAULT ''"},
		{"k12_creative_work_intakes", "base_version_id",
			"TEXT NOT NULL DEFAULT ''"},
		{"k12_creative_work_intakes", "promoted_version_id",
			"TEXT NOT NULL DEFAULT ''"},
		{"k12_creative_work_intakes", "commit_receipt_json",
			"TEXT NOT NULL DEFAULT ''"},
	} {
		has, checkErr := txColumnExists(ctx, tx, column.table, column.name)
		if checkErr != nil {
			return fmt.Errorf("检查 %s.%s: %w", column.table, column.name, checkErr)
		}
		if has {
			continue
		}
		if _, alterErr := tx.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN %s %s`, column.table, column.name, column.def,
		)); alterErr != nil {
			return fmt.Errorf("新增 %s.%s: %w", column.table, column.name, alterErr)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
        SET promoted_version_id='v1'
        WHERE status='promoted' AND promoted_work_id!='' AND promoted_version_id=''`); err != nil {
		return fmt.Errorf("回填历史自动晋升版本引用: %w", err)
	}

	// New identity discriminators are immutable. The V32 triggers continue to
	// protect their original columns.
	if _, err := tx.ExecContext(ctx, `
CREATE TRIGGER IF NOT EXISTS k12_image_dispatch_manual_identity_immutable
BEFORE UPDATE OF routing_provenance, creative_entry_json, operation_route_request_json
ON k12_image_task_dispatches
BEGIN
    SELECT RAISE(ABORT, 'image task manual routing identity is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_creative_intake_manual_identity_immutable
BEFORE UPDATE OF entry_kind, promotion_policy, target_work_id, base_version_id
ON k12_creative_work_intakes
BEGIN
    SELECT RAISE(ABORT, 'creative work manual intake identity is immutable');
END;`); err != nil {
		return fmt.Errorf("创建手工作品不可变约束: %w", err)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
