package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ImageTasksV32CreatesDispatchIntakeAndInvocationLedger(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All[:31]); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12ImageTasksV32}); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"k12_image_task_dispatches",
		"k12_homework_submissions",
		"k12_creative_work_intakes",
		"k12_image_task_invocations",
	} {
		var got string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).
			Scan(&got); err != nil {
			t.Fatalf("%s missing: %v", table, err)
		}
	}
	for _, column := range []string{
		"display_name", "work_title", "task_requirement",
		"title_task_provenance_json", "source_intake_id",
	} {
		has, err := columnExists(context.Background(), db, "k12_creative_works", column)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("k12_creative_works.%s missing", column)
		}
	}
}

func TestK12ImageTasksV32RejectsHalfPromotedIntake(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents
		(name,display_name,description,model,provider,system_prompt,metadata,created_at,updated_at)
		VALUES('mingming','','','','','','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_creative_work_intakes
		(intake_id,dispatch_id,agent_name,learner_id,work_type,source_asset_refs_json,
		 source_digest,route_policy_snapshot_json,status,idempotency_key,request_digest,
		 attempt_generation,created_at,updated_at,version)
		VALUES('i1','missing-dispatch','mingming','l1','art','["asset://mingming/a.png"]',
		 'sha256:a','{}','promoted','key','sha256:req',1,1,1,0)`); err == nil {
		t.Fatal("promoted intake without dispatch/work unexpectedly accepted")
	}
}
