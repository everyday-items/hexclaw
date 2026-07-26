package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ProblemSourceNumberV42 preserves the original paper's hierarchical
// numbering as immutable Problem evidence. Legacy rows remain explicitly
// unnumbered rather than receiving synthetic ordinal-based labels.
var K12ProblemSourceNumberV42 = Migration{
	Version:     42,
	Description: "v0.5.0 原卷层级题号端到端冻结",
	AtomicFunc:  migrateK12ProblemSourceNumberV42,
}

func migrateK12ProblemSourceNumberV42(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启原卷题号迁移事务: %w", err)
	}
	defer tx.Rollback()

	exists, err := txTableExists(ctx, tx, "k12_problems")
	if err != nil {
		return fmt.Errorf("检查 k12_problems: %w", err)
	}
	if exists {
		for _, column := range []struct {
			name string
			ddl  string
		}{
			{"source_number_path_json", "TEXT NOT NULL DEFAULT '[]'"},
			{"display_label", "TEXT NOT NULL DEFAULT ''"},
		} {
			has, err := txColumnExists(ctx, tx, "k12_problems", column.name)
			if err != nil {
				return fmt.Errorf("检查 k12_problems.%s: %w", column.name, err)
			}
			if has {
				continue
			}
			if _, err := tx.ExecContext(
				ctx,
				fmt.Sprintf(
					`ALTER TABLE k12_problems ADD COLUMN %s %s`,
					column.name,
					column.ddl,
				),
			); err != nil {
				return fmt.Errorf("新增 k12_problems.%s: %w", column.name, err)
			}
		}
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
