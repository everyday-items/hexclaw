package usecase

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase/internal/testdb"
)

func TestWeeklyPracticePersistenceBaseline(t *testing.T) {
	db, err := testdb.Open(filepath.Join(t.TempDir(), "weekly.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	required := map[string][]string{
		"k12_curriculum_progress": {
			"agent_name", "subject", "revision", "textbook_binding_id",
		},
		"k12_weekly_practice_settings": {
			"agent_name", "revision", "timezone", "arithmetic_minutes",
		},
		"k12_weekly_practice_plans": {
			"plan_id", "agent_name", "revision", "iso_week_year",
			"iso_week_number", "timezone", "week_start", "week_end",
			"local_start_date", "local_end_date", "status",
		},
		"k12_weekly_practice_snapshots": {
			"snapshot_id", "plan_id", "plan_revision", "snapshot_digest",
		},
		"k12_weekly_practice_attempts": {
			"snapshot_id", "item_id", "idempotency_key", "request_digest",
		},
		"k12_weekly_practice_saves": {
			"plan_id", "plan_revision", "practice_set_id", "idempotency_key",
		},
	}

	for table, columns := range required {
		rows, err := db.QueryContext(
			context.Background(), "PRAGMA table_info("+table+")",
		)
		if err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		seen := map[string]bool{}
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, kind string
			var defaultValue any
			if err := rows.Scan(
				&cid, &name, &kind, &notNull, &defaultValue, &primaryKey,
			); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			seen[name] = true
		}
		rows.Close()
		for _, column := range columns {
			if !seen[column] {
				t.Errorf("%s missing canonical column %s", table, column)
			}
		}
	}
}
