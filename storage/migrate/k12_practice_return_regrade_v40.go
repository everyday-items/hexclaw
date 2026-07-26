package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12PracticeReturnRegradeV40 adds the durable binding between an immutable
// practice return photo and the one automatic GradingJob that owns its result.
// Processing projections are mutable under the PracticeSet aggregate CAS; the
// original return_id/asset_id/item_ids/returned_at exact-set remains immutable.
var K12PracticeReturnRegradeV40 = Migration{
	Version:     40,
	Description: "v0.5.0 练习卷回传自动复批与证据等级",
	AtomicFunc:  migrateK12PracticeReturnRegradeV40,
}

func migrateK12PracticeReturnRegradeV40(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启练习回传自动复批迁移事务: %w", err)
	}
	defer tx.Rollback()

	for table, columns := range map[string][]struct {
		name string
		def  string
	}{
		"k12_practice_set_items": {
			{"result_evidence", "TEXT NOT NULL DEFAULT ''"},
		},
		"k12_practice_return_assets": {
			{"regrade_job_id", "TEXT NOT NULL DEFAULT ''"},
			{"regrade_status", "TEXT NOT NULL DEFAULT 'queued'"},
			{"route_snapshot_json", "TEXT NOT NULL DEFAULT '{}'"},
			{"annotated_asset_id", "TEXT NOT NULL DEFAULT ''"},
			{"result_markdown", "TEXT NOT NULL DEFAULT ''"},
			{"unresolved_item_ids_json", "TEXT NOT NULL DEFAULT '[]'"},
			{"regrade_updated_at", "INTEGER NOT NULL DEFAULT 0"},
		},
	} {
		exists, err := txTableExists(ctx, tx, table)
		if err != nil {
			return fmt.Errorf("检查 %s: %w", table, err)
		}
		if !exists {
			continue
		}
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
	}
	if exists, err := txTableExists(ctx, tx, "k12_practice_return_assets"); err != nil {
		return fmt.Errorf("检查 k12_practice_return_assets: %w", err)
	} else if exists {
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS
			idx_k12_practice_return_regrade_status
			ON k12_practice_return_assets(regrade_status, regrade_updated_at)
			WHERE regrade_status IN ('queued','running','outcome_unknown')`); err != nil {
			return fmt.Errorf("创建练习回传恢复索引: %w", err)
		}
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
