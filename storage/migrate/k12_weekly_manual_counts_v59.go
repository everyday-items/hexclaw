package migrate

import (
	"context"
	"database/sql"
)

var K12WeeklyManualCountsV59 = Migration{
	Version:     59,
	Description: "persist successful K12 weekly manual question counts",
	Func:        migrateK12WeeklyManualCountsV59,
}

func migrateK12WeeklyManualCountsV59(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS k12_weekly_manual_practice_preferences (
    agent_name TEXT NOT NULL,
    plan_section TEXT NOT NULL,
    item_count INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (agent_name, plan_section),
    CHECK (plan_section IN ('textbook_consolidation', 'arithmetic_warmup')),
    CHECK (item_count >= 1)
);`)
	return err
}
