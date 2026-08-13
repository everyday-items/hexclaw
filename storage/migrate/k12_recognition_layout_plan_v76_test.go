package migrate

import (
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12RecognitionLayoutPlanV76MigratesLegacyAndConstrainsV2Units(t *testing.T) {
	var registered *Migration
	for index := range All {
		if All[index].Version == 76 {
			registered = &All[index]
			break
		}
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	ctx := t.Context()
	seedK12RecognitionLayoutPlanV76LegacyFixture(t, db)

	// 这是迁移前的真实不变量：V65/V73 的封闭枚举会拒绝首个规范 V2 批次单元。
	// V76 必须通过重建两张账本扩展持久化契约，而不能将约束弱化为任意字符串。
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_model_physical_invocations(
			physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
			physical_unit,request_digest,route_snapshot_json,
			request_policy_snapshot_json,status,attempt,created_at,updated_at
		) VALUES(
			'pre-v76-batch','parent-v1','agent-v76','job-v76','recognizing',
			'layout_batch_0001','request-pre','{}','{}','prepared',1,100,100
		)`); err == nil {
		t.Fatal("legacy V65 CHECK unexpectedly accepted layout_batch_0001")
	}
	if registered == nil {
		t.Fatal("migration v76 is not registered in migrate.All")
	}
	if registered.AtomicFunc == nil || registered.Func != nil || registered.SQL != "" {
		t.Fatalf("migration v76 must be one AtomicFunc: %+v", *registered)
	}
	if err := applyMigration(ctx, db, *registered); err != nil {
		t.Fatalf("apply V76 to legacy recognition ledgers: %v", err)
	}

	for _, table := range []string{
		"k12_model_physical_invocations",
		"k12_problem_source_recognition_physical_results",
	} {
		for _, column := range []string{
			"recognition_plan_version",
			"plan_digest",
			"candidate_exact_set_digest",
		} {
			has, columnErr := columnExists(ctx, db, table, column)
			if columnErr != nil || !has {
				t.Fatalf("V76 column %s.%s: has=%v err=%v", table, column, has, columnErr)
			}
		}
	}

	var (
		planVersion, planDigest, candidateDigest string
		resultContent                            sql.NullString
	)
	if err := db.QueryRowContext(t.Context(), `
		SELECT recognition_plan_version,plan_digest,
		       candidate_exact_set_digest,result_content
		FROM k12_model_physical_invocations
		WHERE physical_invocation_id='physical-v1-whole'
	`).Scan(&planVersion, &planDigest, &candidateDigest, &resultContent); err != nil {
		t.Fatal(err)
	}
	if planVersion != "v1" || planDigest != "" || candidateDigest != "" ||
		!resultContent.Valid || resultContent.String != `{"private":"legacy-result"}` {
		t.Fatalf(
			"legacy physical evidence drift: version=%q plan=%q candidates=%q content=%v",
			planVersion,
			planDigest,
			candidateDigest,
			resultContent,
		)
	}
	if err := db.QueryRowContext(t.Context(), `
		SELECT recognition_plan_version,plan_digest,candidate_exact_set_digest
		FROM k12_problem_source_recognition_physical_results
		WHERE work_id='work-v1' AND physical_invocation_id='physical-v1-whole'
	`).Scan(&planVersion, &planDigest, &candidateDigest); err != nil {
		t.Fatal(err)
	}
	if planVersion != "v1" || planDigest != "" || candidateDigest != "" {
		t.Fatalf("legacy source receipt drift: version=%q plan=%q candidates=%q",
			planVersion, planDigest, candidateDigest)
	}

	if _, err := db.ExecContext(t.Context(), `
		UPDATE k12_problem_source_recognition_physical_results
		SET result_digest='mutated'
		WHERE work_id='work-v1' AND physical_invocation_id='physical-v1-whole'
	`); err == nil {
		t.Fatal("V73 physical-result immutability was lost during V76 rebuild")
	}

	seedK12RecognitionLayoutPlanV76V2Parent(t, db)
	for index, unit := range []string{
		"layout_batch_0001",
		"layout_batch_9999",
		"layout_repair_0001",
		"layout_repair_9999",
	} {
		insertV76PhysicalInvocation(t, db, fmt.Sprintf("physical-v2-%d", index), unit, true)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_problem_source_recognition_results(work_id,parent_invocation_id)
		VALUES('work-v2','parent-v2');
		INSERT INTO k12_problem_source_recognition_physical_results(
			work_id,ordinal,parent_invocation_id,physical_invocation_id,
			physical_unit,result_digest,created_at,recognition_plan_version,
			plan_digest,candidate_exact_set_digest
		) VALUES(
			'work-v2',0,'parent-v2','physical-v2-0','layout_batch_0001',
			'result-v2-layout_batch_0001',100,'v2',
			'authorized-plan-v2','candidate-exact-v2'
		)
	`); err != nil {
		t.Fatalf("V76 source-result ledger rejected canonical V2 batch evidence: %v", err)
	}
	for index, unit := range []string{
		"layout_batch_0000",
		"layout_batch_1",
		"layout_batch_10000",
		"layout_batch_abcd",
		"layout_repair_0000",
		"layout_repair_1",
		"layout_repair_10000",
		"layout_repair_abcd",
	} {
		insertV76PhysicalInvocation(t, db, fmt.Sprintf("physical-invalid-%d", index), unit, false)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_model_physical_invocations(
			physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
			physical_unit,request_digest,route_snapshot_json,
			request_policy_snapshot_json,status,attempt,result_digest,result_content,
			created_at,updated_at,recognition_plan_version,plan_digest,
			candidate_exact_set_digest
		) VALUES(
			'v1-cannot-use-layout','parent-v2','agent-v76','job-v76','recognizing',
			'layout_batch_0002','request-v1-layout','{}','{}','succeeded',1,
			'result-v1-layout','{}',100,100,'v1','',''
		)`); err == nil {
		t.Fatal("V76 accepted a layout unit under recognition_plan_version=v1")
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_model_physical_invocations(
			physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
			physical_unit,request_digest,route_snapshot_json,
			request_policy_snapshot_json,status,attempt,result_digest,result_content,
			created_at,updated_at,recognition_plan_version,plan_digest,
			candidate_exact_set_digest
		) VALUES(
			'v2-cannot-use-segment','parent-v2','agent-v76','job-v76','recognizing',
			'segment_1','request-v2-segment','{}','{}','succeeded',1,
			'result-v2-segment','{}',100,100,'v2','header-v2','exact-v2'
		)`); err == nil {
		t.Fatal("V76 accepted a legacy fallback unit under recognition_plan_version=v2")
	}

	for _, table := range []string{
		"k12_recognition_layout_plans",
		"k12_recognition_layout_candidates",
		"k12_recognition_layout_batches",
		"k12_recognition_layout_batch_members",
		"k12_recognition_layout_batch_settlements",
		"k12_recognition_layout_candidate_results",
		"k12_recognition_layout_repair_authorizations",
		"k12_recognition_layout_repair_settlements",
	} {
		exists, tableErr := tableExists(ctx, db, table)
		if tableErr != nil || !exists {
			t.Fatalf("V76 table %s: exists=%v err=%v", table, exists, tableErr)
		}
	}

	seedK12RecognitionLayoutPlanV76DeferredDeadline(t, db)
	if _, err := db.ExecContext(t.Context(), `
		UPDATE k12_recognition_layout_plans
		SET manifest_result_digest='manifest-result-deferred',
		    authorized_plan_digest='authorized-plan-deferred',
		    candidate_exact_set_digest='candidate-exact-deferred',
		    authorized_plan_json='{}',
		    selected_bucket_max_problems=8,
		    stage_deadline_at=340000,
		    status='authorized',updated_at=101
		WHERE plan_id='plan-v2-deferred'
	`); err != nil {
		t.Fatalf("V76 could not freeze the selected bucket deadline after manifest success: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		UPDATE k12_recognition_layout_plans
		SET selected_bucket_max_problems=16,stage_deadline_at=500000,updated_at=102
		WHERE plan_id='plan-v2-deferred'
	`); err == nil {
		t.Fatal("V76 allowed the selected recognition bucket/deadline to change twice")
	}
	var selectedBucket int
	var selectedDeadline int64
	if err := db.QueryRowContext(t.Context(), `
		SELECT selected_bucket_max_problems,stage_deadline_at
		FROM k12_recognition_layout_plans WHERE plan_id='plan-v2-deferred'
	`).Scan(&selectedBucket, &selectedDeadline); err != nil {
		t.Fatal(err)
	}
	if selectedBucket != 8 || selectedDeadline != 340000 {
		t.Fatalf("selected recognition bucket/deadline=%d/%d want=8/340000", selectedBucket, selectedDeadline)
	}

	seedK12RecognitionLayoutPlanV76Authorization(t, db)
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_recognition_layout_candidates(
			plan_id,candidate_id,ordinal,bbox_x,bbox_y,bbox_width,bbox_height,
			crop_digest,candidate_json,created_at
		) VALUES('plan-v2','candidate-bad-ordinal',33,1,1,10,10,'crop-bad','{}',100)
	`); err == nil {
		t.Fatal("V76 accepted candidate ordinal 33")
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_recognition_layout_candidates(
			plan_id,candidate_id,ordinal,bbox_x,bbox_y,bbox_width,bbox_height,
			crop_digest,candidate_json,created_at
		) VALUES('plan-v2','candidate-bad-bbox',3,1,1,0,10,'crop-bad','{}',100)
	`); err == nil {
		t.Fatal("V76 accepted a non-positive candidate bbox")
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_recognition_layout_batches(
			plan_id,batch_id,ordinal,physical_unit,member_count,batch_digest,
			input_digest,created_at
		) VALUES('plan-v2','batch-too-large',2,'layout_batch_0002',5,
			'batch-too-large','input-too-large',100)
	`); err == nil {
		t.Fatal("V76 accepted a primary batch with more than four candidates")
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_recognition_layout_batch_members(
			plan_id,batch_id,slot,candidate_id,created_at
		) VALUES('plan-v2','batch-v2-2',0,'candidate-v2-1',100)
	`); err == nil {
		t.Fatal("V76 allowed one primary candidate in two batches")
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_recognition_layout_repair_authorizations(
			plan_id,repair_authorization_id,repair_physical_unit,candidate_id,
			source_batch_id,source_batch_physical_invocation_id,
			source_batch_result_digest,repair_round,authorization_digest,created_at
		) VALUES(
			'plan-v2','repair-round-two','layout_repair_0002','candidate-v2-2',
			'batch-v2-1','physical-v2-0','result-v2-layout_batch_0001',
			2,'repair-round-two-digest',100
		)`); err == nil {
		t.Fatal("V76 accepted a second repair round")
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_recognition_layout_repair_authorizations(
			plan_id,repair_authorization_id,repair_physical_unit,candidate_id,
			source_batch_id,source_batch_physical_invocation_id,
			source_batch_result_digest,repair_round,authorization_digest,created_at
		) VALUES(
			'plan-v2','repair-wrong-source','layout_repair_0002','candidate-v2-2',
			'batch-v2-1','physical-v2-0','wrong-result-digest',
			1,'repair-wrong-source-digest',100
		)`); err == nil {
		t.Fatal("V76 accepted repair authorization detached from source batch result")
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_recognition_layout_repair_authorizations(
			plan_id,repair_authorization_id,repair_physical_unit,candidate_id,
			source_batch_id,source_batch_physical_invocation_id,
			source_batch_result_digest,repair_round,authorization_digest,created_at
		) VALUES(
			'plan-v2','repair-duplicate-candidate','layout_repair_0002','candidate-v2-1',
			'batch-v2-1','physical-v2-0','result-v2-layout_batch_0001',
			1,'repair-duplicate-digest',100
		)`); err == nil {
		t.Fatal("V76 authorized the same candidate for repair twice")
	}
	if _, err := db.ExecContext(t.Context(), `
		UPDATE k12_recognition_layout_candidates
		SET bbox_width=11
		WHERE plan_id='plan-v2' AND candidate_id='candidate-v2-1'
	`); err == nil {
		t.Fatal("V76 accepted mutation of an authorized candidate")
	}
	if _, err := db.ExecContext(t.Context(), `
		UPDATE k12_recognition_layout_plans
		SET effective_concurrency=3,updated_at=101
		WHERE plan_id='plan-v2'
	`); err == nil {
		t.Fatal("V76 accepted effective concurrency above adapter hard cap 2")
	}

	var violations int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("V76 left %d foreign-key violations", violations)
	}
	for _, object := range []string{
		"idx_k12_model_physical_invocations_job",
		"idx_k12_model_physical_invocations_status",
		"idx_k12_model_physical_invocation_parent_identity",
		"idx_k12_problem_source_recognition_physical_parent",
		"k12_problem_source_recognition_physical_result_immutable",
	} {
		var count int
		if err := db.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM sqlite_master
			WHERE name=? AND type IN ('index','trigger')
		`, object).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("V76 lost schema object %s: count=%d", object, count)
		}
	}
	var versionCount int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations WHERE version=76`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 {
		t.Fatalf("V76 migration ledger count=%d, want 1", versionCount)
	}
}

func seedK12RecognitionLayoutPlanV76DeferredDeadline(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_model_invocations(invocation_id,agent_name,job_id,stage)
		VALUES('parent-v2-deferred','agent-v76','job-v76','recognizing');
		INSERT INTO k12_model_physical_invocations(
			physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
			physical_unit,request_digest,route_snapshot_json,
			request_policy_snapshot_json,status,attempt,result_digest,result_content,
			created_at,updated_at,recognition_plan_version,plan_digest,
			candidate_exact_set_digest
		) VALUES(
			'physical-v2-deferred','parent-v2-deferred','agent-v76','job-v76','recognizing',
			'whole_page','request-v2-deferred','{"provider":"hexclaw-gpt"}',
			'{"policy":"v2"}','succeeded',1,'manifest-result-deferred','{"targets":[]}',
			100,100,'v2','header-v2-deferred',''
		);
		INSERT INTO k12_recognition_layout_plans(
			plan_id,parent_invocation_id,agent_name,job_id,stage,
			manifest_physical_invocation_id,page_digest,header_digest,
			manifest_result_digest,authorized_plan_digest,candidate_exact_set_digest,
			layout_header_json,authorized_plan_json,stage_started_at,stage_deadline_at,
			selected_bucket_max_problems,effective_concurrency,status,created_at,updated_at
		) VALUES(
			'plan-v2-deferred','parent-v2-deferred','agent-v76','job-v76','recognizing',
			'physical-v2-deferred','page-v2-deferred','header-v2-deferred',
			'manifest-result-deferred','','',
			'{"physical_call_cap_millis":120000,"budget_buckets":{"candidates_1_millis":120000,"candidates_8_millis":240000,"candidates_16_millis":360000,"candidates_32_millis":600000}}',
			'',100000,0,0,1,'manifest_succeeded',100,100
		);
	`); err != nil {
		t.Fatalf("seed deferred V2 recognition deadline: %v", err)
	}
}

func seedK12RecognitionLayoutPlanV76LegacyFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		PRAGMA foreign_keys=ON;
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			applied_at INTEGER NOT NULL
		);
		CREATE TABLE agents (name TEXT PRIMARY KEY);
		CREATE TABLE k12_grading_jobs (
			record_id TEXT PRIMARY KEY,
			agent_name TEXT NOT NULL,
			FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
		);
		CREATE TABLE k12_model_invocations (
			invocation_id TEXT PRIMARY KEY,
			agent_name TEXT NOT NULL,
			job_id TEXT NOT NULL,
			stage TEXT NOT NULL,
			FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE,
			FOREIGN KEY(job_id) REFERENCES k12_grading_jobs(record_id) ON DELETE CASCADE
		);
		INSERT INTO agents(name) VALUES('agent-v76');
		INSERT INTO k12_grading_jobs(record_id,agent_name)
		VALUES('job-v76','agent-v76');
		INSERT INTO k12_model_invocations(invocation_id,agent_name,job_id,stage)
		VALUES('parent-v1','agent-v76','job-v76','recognizing');
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), K12ModelPhysicalInvocationsV65DDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `
		CREATE UNIQUE INDEX idx_k12_model_physical_invocation_parent_identity
			ON k12_model_physical_invocations(physical_invocation_id,parent_invocation_id);
		CREATE TABLE k12_problem_source_recognition_results (
			work_id TEXT PRIMARY KEY,
			parent_invocation_id TEXT NOT NULL,
			UNIQUE(work_id,parent_invocation_id),
			FOREIGN KEY(parent_invocation_id)
				REFERENCES k12_model_invocations(invocation_id) ON DELETE CASCADE
		);
		CREATE TABLE k12_problem_source_recognition_physical_results (
			work_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
			parent_invocation_id TEXT NOT NULL CHECK(length(trim(parent_invocation_id)) > 0),
			physical_invocation_id TEXT NOT NULL CHECK(length(trim(physical_invocation_id)) > 0),
			physical_unit TEXT NOT NULL CHECK(physical_unit IN
				('whole_page','segment_1','segment_2','segment_3','segment_4','segment_5','printed_inventory')),
			result_digest TEXT NOT NULL CHECK(length(trim(result_digest)) > 0),
			created_at INTEGER NOT NULL CHECK(created_at > 0),
			PRIMARY KEY(work_id,physical_invocation_id),
			UNIQUE(work_id,ordinal),
			FOREIGN KEY(work_id,parent_invocation_id)
				REFERENCES k12_problem_source_recognition_results(work_id,parent_invocation_id)
				ON DELETE CASCADE,
			FOREIGN KEY(physical_invocation_id,parent_invocation_id)
				REFERENCES k12_model_physical_invocations(
					physical_invocation_id,parent_invocation_id
				) ON DELETE RESTRICT
		);
		CREATE INDEX idx_k12_problem_source_recognition_physical_parent
			ON k12_problem_source_recognition_physical_results(
				parent_invocation_id,physical_invocation_id,work_id
			);
		CREATE TRIGGER k12_problem_source_recognition_physical_result_immutable
		BEFORE UPDATE ON k12_problem_source_recognition_physical_results
		BEGIN
			SELECT RAISE(ABORT, 'problem source recognition physical result is immutable');
		END;
		INSERT INTO k12_model_physical_invocations(
			physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
			physical_unit,request_digest,route_snapshot_json,
			request_policy_snapshot_json,status,attempt,result_digest,result_content,
			external_request_id,failure_kind,created_at,updated_at
		) VALUES(
			'physical-v1-whole','parent-v1','agent-v76','job-v76','recognizing',
			'whole_page','request-v1','{"provider":"legacy"}','{"policy":"v1"}',
			'succeeded',1,'result-v1','{"private":"legacy-result"}',
			'external-v1','',100,101
		);
		INSERT INTO k12_recognition_fallback_authorizations(
			parent_invocation_id,agent_name,job_id,whole_physical_invocation_id,
			whole_result_digest,whole_result_content,created_at
		) VALUES(
			'parent-v1','agent-v76','job-v76','physical-v1-whole',
			'result-v1','{"private":"legacy-result"}',102
		);
		INSERT INTO k12_problem_source_recognition_results(work_id,parent_invocation_id)
		VALUES('work-v1','parent-v1');
		INSERT INTO k12_problem_source_recognition_physical_results(
			work_id,ordinal,parent_invocation_id,physical_invocation_id,
			physical_unit,result_digest,created_at
		) VALUES(
			'work-v1',0,'parent-v1','physical-v1-whole','whole_page','result-v1',103
		);
	`); err != nil {
		t.Fatal(err)
	}
}

func seedK12RecognitionLayoutPlanV76V2Parent(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_model_invocations(invocation_id,agent_name,job_id,stage)
		VALUES('parent-v2','agent-v76','job-v76','recognizing');
		INSERT INTO k12_model_physical_invocations(
			physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
			physical_unit,request_digest,route_snapshot_json,
			request_policy_snapshot_json,status,attempt,result_digest,result_content,
			created_at,updated_at,recognition_plan_version,plan_digest,
			candidate_exact_set_digest
		) VALUES(
			'physical-v2-manifest','parent-v2','agent-v76','job-v76','recognizing',
			'whole_page','request-v2-manifest','{"provider":"hexclaw-gpt"}',
			'{"policy":"v2"}','succeeded',1,'manifest-result-v2','{"targets":[]}',
			100,100,'v2','header-v2',''
		);
	`); err != nil {
		t.Fatal(err)
	}
}

func insertV76PhysicalInvocation(
	t *testing.T,
	db *sql.DB,
	physicalID string,
	unit string,
	wantAccepted bool,
) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_model_physical_invocations(
			physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
			physical_unit,request_digest,route_snapshot_json,
			request_policy_snapshot_json,status,attempt,result_digest,result_content,
			created_at,updated_at,recognition_plan_version,plan_digest,
			candidate_exact_set_digest
		) VALUES(?,?,?,?,? ,?,?,?,?,? ,?,?,?,?,? ,?,?,?)
	`,
		physicalID,
		"parent-v2",
		"agent-v76",
		"job-v76",
		"recognizing",
		unit,
		"request-v2-"+unit,
		`{"provider":"hexclaw-gpt"}`,
		`{"policy":"v2"}`,
		"succeeded",
		1,
		"result-v2-"+unit,
		`{"items":[]}`,
		100,
		100,
		"v2",
		"authorized-plan-v2",
		"candidate-exact-v2",
	)
	if wantAccepted && err != nil {
		t.Fatalf("canonical V2 physical unit %q rejected: %v", unit, err)
	}
	if !wantAccepted && err == nil {
		t.Fatalf("non-canonical V2 physical unit %q accepted", unit)
	}
}

func seedK12RecognitionLayoutPlanV76Authorization(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_recognition_layout_plans(
			plan_id,parent_invocation_id,agent_name,job_id,stage,
			manifest_physical_invocation_id,page_digest,header_digest,
			manifest_result_digest,authorized_plan_digest,candidate_exact_set_digest,
			layout_header_json,authorized_plan_json,stage_started_at,stage_deadline_at,
			selected_bucket_max_problems,effective_concurrency,status,created_at,updated_at
		) VALUES(
			'plan-v2','parent-v2','agent-v76','job-v76','recognizing',
			'physical-v2-manifest','page-v2','header-v2','manifest-result-v2',
			'authorized-plan-v2','candidate-exact-v2',
			'{"physical_call_timeout_ms":120000}',
			'{"candidate_ids":["candidate-v2-1","candidate-v2-2"]}',
			100000,500000,8,1,'authorized',100,100
		);
		INSERT INTO k12_recognition_layout_candidates(
			plan_id,candidate_id,ordinal,bbox_x,bbox_y,bbox_width,bbox_height,
			crop_digest,candidate_json,created_at
		) VALUES
			('plan-v2','candidate-v2-1',1,1,1,10,10,'crop-v2-1','{}',100),
			('plan-v2','candidate-v2-2',2,12,1,10,10,'crop-v2-2','{}',100);
		INSERT INTO k12_recognition_layout_batches(
			plan_id,batch_id,ordinal,physical_unit,member_count,batch_digest,
			input_digest,created_at
		) VALUES
			('plan-v2','batch-v2-1',1,'layout_batch_0001',2,
			 'batch-digest-v2-1','batch-input-v2-1',100),
			('plan-v2','batch-v2-2',2,'layout_batch_0002',1,
			 'batch-digest-v2-2','batch-input-v2-2',100);
		INSERT INTO k12_recognition_layout_batch_members(
			plan_id,batch_id,slot,candidate_id,created_at
		) VALUES
			('plan-v2','batch-v2-1',0,'candidate-v2-1',100),
			('plan-v2','batch-v2-1',1,'candidate-v2-2',100);
		INSERT INTO k12_recognition_layout_candidate_results(
			plan_id,candidate_id,parent_invocation_id,
			source_physical_invocation_id,source_physical_result_digest,
			result_kind,result_digest,result_json,created_at
		) VALUES(
			'plan-v2','candidate-v2-2','parent-v2','physical-v2-0',
			'result-v2-layout_batch_0001','question','candidate-result-v2-2','{}',100
		);
		INSERT INTO k12_recognition_layout_repair_authorizations(
			plan_id,repair_authorization_id,repair_physical_unit,candidate_id,
			source_batch_id,source_batch_physical_invocation_id,
			source_batch_result_digest,repair_round,authorization_digest,created_at
		) VALUES(
			'plan-v2','repair-v2-1','layout_repair_0001','candidate-v2-1',
			'batch-v2-1','physical-v2-0','result-v2-layout_batch_0001',
			1,'repair-authorization-v2-1',100
		);
	`); err != nil {
		t.Fatal(err)
	}
}
