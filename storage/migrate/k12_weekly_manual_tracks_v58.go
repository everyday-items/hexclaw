package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

var K12WeeklyManualTracksV58 = Migration{
	Version:     58,
	Description: "BUG-20260727-005: 周练手动教材同步档位与服务端推荐策略",
	Func:        migrateK12WeeklyManualTracksV58,
}

func migrateK12WeeklyManualTracksV58(ctx context.Context, db *sql.DB) error {
	has, err := columnExists(
		ctx, db, "k12_weekly_practice_settings", "textbook_consolidation_tier")
	if err != nil {
		return fmt.Errorf("检查周练教材同步档位: %w", err)
	}
	if has {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE k12_weekly_practice_settings
        ADD COLUMN textbook_consolidation_tier TEXT NOT NULL DEFAULT 'standard'
        CHECK(textbook_consolidation_tier IN ('less','standard','more'))`); err != nil {
		return fmt.Errorf("新增周练教材同步档位: %w", err)
	}
	return nil
}
