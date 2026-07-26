package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12SinglePracticeGenerationV39AddsDurableJoinFieldsAndGuards(t *testing.T) {
	db, err := sql.Open("sqlite", "file:k12-single-practice-v39?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE k12_practice_set_items (
			set_record_id TEXT NOT NULL,
			item_index INTEGER NOT NULL,
			generation_job_id TEXT NOT NULL DEFAULT '',
			added_via TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE k12_practice_generation_jobs (
			generation_job_id TEXT PRIMARY KEY,
			agent_name TEXT NOT NULL,
			scope TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
	`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12SinglePracticeGenerationV39}); err != nil {
		t.Fatal(err)
	}
	for table, columns := range map[string][]string{
		"k12_practice_set_items": {
			"source_mistake_summary", "generation_status",
		},
		"k12_practice_generation_jobs": {
			"source_mistake_id", "source_mistake_summary", "request_snapshot_json",
			"route_snapshot_json", "attempt", "generation_output_json",
			"generation_output_attempt", "validation_output_json",
			"validation_output_attempt", "retired_at", "retired_reason",
		},
	} {
		for _, column := range columns {
			has, err := columnExists(context.Background(), db, table, column)
			if err != nil || !has {
				t.Fatalf("%s.%s exists=%v err=%v", table, column, has, err)
			}
		}
	}
	hasLedger, err := tableExists(
		context.Background(), db, "k12_practice_generation_invocations",
	)
	if err != nil || !hasLedger {
		t.Fatalf("practice generation invocation ledger exists=%v err=%v", hasLedger, err)
	}
	if _, err := db.Exec(`INSERT INTO k12_practice_generation_jobs
		(generation_job_id,agent_name,scope,status,created_at,source_mistake_id)
		VALUES ('job-1','mingming','single','queued',1,'mistake-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_practice_generation_jobs
		(generation_job_id,agent_name,scope,status,created_at,source_mistake_id)
		VALUES ('job-2','mingming','single','generating',2,'mistake-1')`); err == nil {
		t.Fatal("same source accepted a second active single generation")
	}
}

func TestK12SinglePracticeGenerationV39IsRegisteredAtItsNumber(t *testing.T) {
	if len(All) < K12SinglePracticeGenerationV39.Version ||
		All[K12SinglePracticeGenerationV39.Version-1].Version != K12SinglePracticeGenerationV39.Version {
		t.Fatalf("migration %d is not registered at its numbered position", K12SinglePracticeGenerationV39.Version)
	}
}
