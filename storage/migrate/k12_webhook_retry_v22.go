package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// migrateK12WebhookRetryV22 adds only the durable facts needed to decide
// whether a failed Receipt may be redispatched. Existing rows default to
// retry_safe=0, so an upgrade never invents evidence that a prior side effect
// is safe to repeat. Column probes make interrupted/half-applied upgrades
// re-entrant.
func migrateK12WebhookRetryV22(ctx context.Context, db *sql.DB) error {
	columns := []struct {
		name string
		ddl  string
	}{
		{
			name: "retry_safe",
			ddl:  `ALTER TABLE k12_webhook_receipts ADD COLUMN retry_safe INTEGER NOT NULL DEFAULT 0 CHECK(retry_safe IN (0,1))`,
		},
		{
			name: "attempt_count",
			ddl:  `ALTER TABLE k12_webhook_receipts ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0)`,
		},
	}
	for _, column := range columns {
		has, err := columnExists(ctx, db, "k12_webhook_receipts", column.name)
		if err != nil {
			return fmt.Errorf("inspect K12 webhook Receipt column %s: %w", column.name, err)
		}
		if has {
			continue
		}
		if _, err := db.ExecContext(ctx, column.ddl); err != nil {
			return fmt.Errorf("add K12 webhook Receipt column %s: %w", column.name, err)
		}
	}
	return nil
}
