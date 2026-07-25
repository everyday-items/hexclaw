package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12GradingResumeV30NoOpsWhenK12SchemaIsAbsent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := migrateK12GradingResumeV30(context.Background(), db); err != nil {
		t.Fatalf("optional K12 migration must no-op without the K12 root schema: %v", err)
	}
	for _, table := range []string{"k12_grading_item_invocations", "k12_grading_assessment_items"} {
		has, err := tableExists(context.Background(), db, table)
		if err != nil {
			t.Fatal(err)
		}
		if has {
			t.Fatalf("%s must not be created without its K12 parent schema", table)
		}
	}
}

// REG-DD-038: the per-item grading ledger is a release-governed additive
// migration. Existing GradingJob rows must survive while the frozen budget
// column and both evidence tables are added.
func TestK12GradingResumeV30IsRegisteredAndAdditive(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := Run(ctx, db, All[:29]); err != nil {
		t.Fatalf("install through V29: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_grading_jobs
		(record_id,agent_name,schema_version,status,dedupe_key,tags_json,source_session_id,
		 version,created_at,updated_at,submission_id,source_kind,idempotency_key,
		 confirmation_state,anchor_state,model_snapshot_json)
		VALUES('job-old','mingming',1,'queued','dedupe-old','[]','session-old',
		 0,100,100,'submission-old','desktop','desktop|old|v0','pending','pending','{}')`); err != nil {
		t.Fatalf("seed pre-V30 grading job: %v", err)
	}
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("upgrade through V31: %v", err)
	}

	var maxVersion int
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&maxVersion); err != nil {
		t.Fatal(err)
	}
	wantLatest := All[len(All)-1].Version
	if maxVersion != wantLatest {
		t.Fatalf("latest migration=%d, want %d", maxVersion, wantLatest)
	}
	for _, table := range []string{"k12_grading_item_invocations", "k12_grading_assessment_items"} {
		has, err := tableExists(ctx, db, table)
		if err != nil || !has {
			t.Fatalf("%s missing after V30: has=%v err=%v", table, has, err)
		}
	}
	for _, column := range []string{"projection_record_id", "projection_created"} {
		has, err := columnExists(ctx, db, "k12_grading_assessment_items", column)
		if err != nil || !has {
			t.Fatalf("assessment receipt column %s missing: has=%v err=%v", column, has, err)
		}
	}
	hasBudget, err := columnExists(ctx, db, "k12_grading_jobs", "budget_snapshot_json")
	if err != nil || !hasBudget {
		t.Fatalf("budget_snapshot_json missing: has=%v err=%v", hasBudget, err)
	}
	var snapshot string
	if err := db.QueryRowContext(ctx,
		`SELECT budget_snapshot_json FROM k12_grading_jobs WHERE record_id='job-old'`).Scan(&snapshot); err != nil {
		t.Fatalf("old grading job lost: %v", err)
	}
	if snapshot != "" {
		t.Fatalf("old job must remain legacy/unfrozen, snapshot=%q", snapshot)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_problems
		(problem_id,agent_name,submission_id,page_asset_id,ordinal,problem_kind,
		 stem_raw,stem_markdown,canonical_version,created_at,updated_at)
		VALUES('problem-v0','mingming','submission-old','page-1',0,'standalone','q','q',1,100,100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_attempts
		(attempt_id,agent_name,submission_id,problem_id,answer_state,answer_raw,answer_markdown,
		 confirmed_version,input_digest,created_at,updated_at)
		VALUES('attempt-v1','mingming','submission-old','problem-v0','present','a','a',1,'sha256:input',100,100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_grading_assessment_items
		(agent_name,job_id,problem_id,attempt_id,confirmed_version,input_digest,status,
		 result_json,result_digest,projection_status,created_at,updated_at)
		VALUES('mingming','job-old','problem-v0','attempt-v1',0,'sha256:input','unanswered',
		 '{"status":"unanswered"}','sha256:result','committed',100,100)`); err == nil {
		t.Fatal("V30 must reject an assessment receipt for unconfirmed version 0")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_grading_assessment_items
		(agent_name,job_id,problem_id,attempt_id,confirmed_version,input_digest,status,
		 result_json,result_digest,projection_created,projection_status,created_at,updated_at)
		VALUES('mingming','job-old','problem-v0','attempt-v1',1,'sha256:input','unanswered',
		 '{"status":"unanswered"}','sha256:result',1,'committed',100,100)`); err == nil {
		t.Fatal("V30 must reject projection_created without a projected record id")
	}
}
