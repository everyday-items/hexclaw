package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12GradingItemInputFenceV98 为题目调用账本补充来源输入版本与摘要。
var K12GradingItemInputFenceV98 = Migration{
	Version:     98,
	Description: "K12 grading item invocation input revision and digest fence",
	Func:        migrateK12GradingItemInputFenceV98,
}

func migrateK12GradingItemInputFenceV98(ctx context.Context, db *sql.DB) error {
	hasTable, err := tableExists(ctx, db, "k12_grading_item_invocations")
	if err != nil {
		return fmt.Errorf("check K12 grading item invocation table: %w", err)
	}
	if !hasTable {
		return nil
	}
	for _, column := range []struct {
		name string
		ddl  string
	}{
		{name: "input_revision", ddl: `ALTER TABLE k12_grading_item_invocations
			ADD COLUMN input_revision INTEGER NOT NULL DEFAULT 0`},
		{name: "input_digest", ddl: `ALTER TABLE k12_grading_item_invocations
			ADD COLUMN input_digest TEXT NOT NULL DEFAULT ''`},
	} {
		exists, err := columnExists(ctx, db, "k12_grading_item_invocations", column.name)
		if err != nil {
			return fmt.Errorf("check K12 grading item invocation %s: %w", column.name, err)
		}
		if exists {
			continue
		}
		if _, err := db.ExecContext(ctx, column.ddl); err != nil {
			return fmt.Errorf("add K12 grading item invocation %s: %w", column.name, err)
		}
	}
	return nil
}
