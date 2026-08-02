package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12SourceSectionSystemOrderV68 preserves the printed section heading and the
// separately marked server ordinal used only when a child item has no printed
// number. Existing rows remain explicitly blank/zero; no historical source
// number is inferred during migration.
var K12SourceSectionSystemOrderV68 = Migration{
	Version:     68,
	Description: "DD-041 K12 原卷分区与服务端系统序号双事实冻结",
	AtomicFunc:  migrateK12SourceSectionSystemOrderV68,
}

func migrateK12SourceSectionSystemOrderV68(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 DD-041 分区序号迁移事务: %w", err)
	}
	defer tx.Rollback()

	for _, table := range []struct {
		name    string
		columns []struct{ name, ddl string }
	}{
		{
			name: "k12_problems",
			columns: []struct{ name, ddl string }{
				{"source_section_path_json", "TEXT NOT NULL DEFAULT '[]'"},
				{"source_section_label", "TEXT NOT NULL DEFAULT ''"},
				{"system_section_ordinal", "INTEGER NOT NULL DEFAULT 0 CHECK(system_section_ordinal >= 0)"},
				{"system_display_label", "TEXT NOT NULL DEFAULT ''"},
			},
		},
		{
			name: "k12_problem_structure_members",
			columns: []struct{ name, ddl string }{
				{"source_section_path_json", "TEXT NOT NULL DEFAULT '[]'"},
				{"source_section_label", "TEXT NOT NULL DEFAULT ''"},
				{"system_section_ordinal", "INTEGER NOT NULL DEFAULT 0 CHECK(system_section_ordinal >= 0)"},
				{"system_display_label", "TEXT NOT NULL DEFAULT ''"},
			},
		},
	} {
		exists, err := txTableExists(ctx, tx, table.name)
		if err != nil {
			return fmt.Errorf("检查 %s: %w", table.name, err)
		}
		if !exists {
			continue
		}
		for _, column := range table.columns {
			has, err := txColumnExists(ctx, tx, table.name, column.name)
			if err != nil {
				return fmt.Errorf("检查 %s.%s: %w", table.name, column.name, err)
			}
			if has {
				continue
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(
				`ALTER TABLE %s ADD COLUMN %s %s`, table.name, column.name, column.ddl,
			)); err != nil {
				return fmt.Errorf("新增 %s.%s: %w", table.name, column.name, err)
			}
		}
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
