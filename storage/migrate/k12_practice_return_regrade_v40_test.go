package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12PracticeReturnRegradeV40AddsDurableAutomaticRegradeProjection(t *testing.T) {
	db, err := sql.Open("sqlite", "file:k12-practice-return-v40?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE k12_practice_set_items (
			set_record_id TEXT NOT NULL,
			item_id TEXT NOT NULL,
			result_correct INTEGER
		);
		CREATE TABLE k12_practice_return_assets (
			set_record_id TEXT NOT NULL,
			return_id TEXT NOT NULL,
			PRIMARY KEY(set_record_id, return_id)
		);
	`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12PracticeReturnRegradeV40}); err != nil {
		t.Fatal(err)
	}
	for table, columns := range map[string][]string{
		"k12_practice_set_items": {
			"result_evidence",
		},
		"k12_practice_return_assets": {
			"regrade_job_id", "regrade_status", "route_snapshot_json",
			"annotated_asset_id", "result_markdown", "unresolved_item_ids_json",
			"regrade_updated_at",
		},
	} {
		for _, column := range columns {
			has, err := columnExists(context.Background(), db, table, column)
			if err != nil || !has {
				t.Fatalf("%s.%s exists=%v err=%v", table, column, has, err)
			}
		}
	}
}

func TestK12PracticeReturnRegradeV40PrecedesInsightSnapshotV41(t *testing.T) {
	if K12PracticeReturnRegradeV40.Version+1 != K12InsightSnapshotV41.Version {
		t.Fatalf("v40=%d v41=%d", K12PracticeReturnRegradeV40.Version, K12InsightSnapshotV41.Version)
	}
}
