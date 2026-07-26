package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12InsightSnapshotV41 adds immutable grade-term attribution to every source
// collection consumed by the learning-insight snapshot. Existing rows
// deliberately remain empty: current profile metadata is not evidence of the
// term in which a legacy object was created.
var K12InsightSnapshotV41 = Migration{
	Version:     41,
	Description: "v0.5.0 学情同一快照与不可变学期归属",
	AtomicFunc:  migrateK12InsightSnapshotV41,
}

func migrateK12InsightSnapshotV41(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启学情快照迁移事务: %w", err)
	}
	defer tx.Rollback()

	for _, table := range []string{"k12_mistakes", "k12_accumulations", "k12_practice_sets"} {
		exists, err := txTableExists(ctx, tx, table)
		if err != nil {
			return fmt.Errorf("检查 %s: %w", table, err)
		}
		if !exists {
			continue
		}
		has, err := txColumnExists(ctx, tx, table, "grade_term")
		if err != nil {
			return fmt.Errorf("检查 %s.grade_term: %w", table, err)
		}
		if !has {
			if _, err := tx.ExecContext(
				ctx,
				fmt.Sprintf(`ALTER TABLE %s ADD COLUMN grade_term TEXT NOT NULL DEFAULT ''`, table),
			); err != nil {
				return fmt.Errorf("新增 %s.grade_term: %w", table, err)
			}
		}
		if _, err := tx.ExecContext(
			ctx,
			fmt.Sprintf(
				`CREATE INDEX IF NOT EXISTS idx_%s_grade_term_scope ON %s(agent_name, grade_term, status)`,
				table,
				table,
			),
		); err != nil {
			return fmt.Errorf("创建 %s 学期索引: %w", table, err)
		}
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
