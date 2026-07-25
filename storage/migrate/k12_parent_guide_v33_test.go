package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openV30ParentGuideFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
PRAGMA foreign_keys=ON;
CREATE TABLE agents(name TEXT PRIMARY KEY);
CREATE TABLE k12_grading_jobs(
    record_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    UNIQUE(agent_name,record_id)
);
CREATE TABLE k12_problems(
    problem_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    UNIQUE(agent_name,problem_id)
);
CREATE TABLE k12_attempts(
    attempt_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    confirmed_version INTEGER NOT NULL,
    input_digest TEXT NOT NULL,
    UNIQUE(agent_name,attempt_id,problem_id)
);
` + K12GradingResumeV30DDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO agents(name) VALUES('mingming');
INSERT INTO k12_grading_jobs(record_id,agent_name) VALUES('job-v30','mingming');
INSERT INTO k12_problems(problem_id,agent_name,submission_id)
VALUES('problem-v30','mingming','submission-v30');
INSERT INTO k12_attempts(
    attempt_id,agent_name,submission_id,problem_id,confirmed_version,input_digest
) VALUES('attempt-v30','mingming','submission-v30','problem-v30',1,'sha256:input');
INSERT INTO k12_grading_item_invocations(
    item_invocation_id,agent_name,job_id,problem_id,attempt_id,
    operation,operation_attempt,request_digest,provider,model,route_snapshot_json,status,
    result_digest,result_json,failure_class,failure_code,created_at,updated_at
) VALUES
    ('solve-v30','mingming','job-v30','problem-v30','attempt-v30',
     'solve',1,'sha256:solve-request','provider','model','{"provider":"provider","model":"model","route":"provider/model"}',
     'succeeded','sha256:solve-result','{"solution":"2"}','','',100,101),
    ('grade-v30','mingming','job-v30','problem-v30','attempt-v30',
     'grade',1,'sha256:grade-request','provider','model','{"provider":"provider","model":"model","route":"provider/model"}',
     'succeeded','sha256:grade-result','{"verdict":"disagree"}','','',100,101);
INSERT INTO k12_grading_assessment_items(
    agent_name,job_id,problem_id,attempt_id,confirmed_version,input_digest,status,
    result_json,result_digest,solve_invocation_id,grade_invocation_id,
    projection_record_id,projection_created,projection_status,created_at,updated_at
) VALUES(
    'mingming','job-v30','problem-v30','attempt-v30',1,'sha256:input','wrong',
    '{"status":"wrong","grade":{"outcome":{"wrong_step":"旧错步","error_cause":"旧错因"}}}',
    'sha256:assessment','solve-v30','grade-v30','',0,'committed',100,101
);`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestK12ParentGuideV33PreservesV30ReceiptsAndAddsSeparateOperation(t *testing.T) {
	db := openV30ParentGuideFixture(t)
	if err := Run(context.Background(), db, []Migration{K12ParentGuideV33}); err != nil {
		t.Fatalf("upgrade V30 fixture: %v", err)
	}

	var resultJSON string
	var parentGuideID sql.NullString
	if err := db.QueryRow(`SELECT result_json,parent_guide_invocation_id
		FROM k12_grading_assessment_items
		WHERE job_id='job-v30' AND problem_id='problem-v30'`).
		Scan(&resultJSON, &parentGuideID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultJSON, `"wrong_step":"旧错步"`) ||
		!strings.Contains(resultJSON, `"error_cause":"旧错因"`) ||
		parentGuideID.Valid {
		t.Fatalf("historical assessment evidence changed: result=%s parent=%v", resultJSON, parentGuideID)
	}

	if _, err := db.Exec(`INSERT INTO k12_grading_item_invocations(
		item_invocation_id,agent_name,job_id,problem_id,attempt_id,
		operation,operation_attempt,request_digest,provider,model,route_snapshot_json,status,
		result_digest,result_json,failure_class,failure_code,created_at,updated_at
	) VALUES(
		'parent-v33','mingming','job-v30','problem-v30','attempt-v30',
		'parent_guide',1,'sha256:parent-request','provider','model',
		'{"provider":"provider","model":"model","route":"provider/model"}',
		'succeeded','sha256:parent-result','{"answer":"2"}','','',102,103
	)`); err != nil {
		t.Fatalf("parent_guide operation unavailable after V33: %v", err)
	}
	if _, err := db.Exec(`UPDATE k12_grading_assessment_items
		SET parent_guide_invocation_id='parent-v33'
		WHERE job_id='job-v30' AND problem_id='problem-v30'`); err != nil {
		t.Fatalf("parent guide durable reference unavailable after V33: %v", err)
	}
	var violations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("V33 left %d foreign-key violations", violations)
	}
}

func TestK12ParentGuideV33NoOpsWithoutV30Tables(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, []Migration{K12ParentGuideV33}); err != nil {
		t.Fatalf("optional V33 migration: %v", err)
	}
	has, err := tableExists(context.Background(), db, "k12_grading_item_invocations")
	if err != nil || has {
		t.Fatalf("V33 invented optional K12 schema: has=%v err=%v", has, err)
	}
}

func TestK12ParentGuideV33ReentryDoesNotDropExistingParentGuideEvidence(t *testing.T) {
	db := openV30ParentGuideFixture(t)
	ctx := context.Background()
	if err := Run(ctx, db, []Migration{K12ParentGuideV33}); err != nil {
		t.Fatalf("first V33: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO k12_grading_item_invocations(
		item_invocation_id,agent_name,job_id,problem_id,attempt_id,
		operation,operation_attempt,request_digest,provider,model,route_snapshot_json,status,
		result_digest,result_json,failure_class,failure_code,created_at,updated_at
	) VALUES(
		'parent-reentry','mingming','job-v30','problem-v30','attempt-v30',
		'parent_guide',1,'sha256:parent-reentry-request','provider','model',
		'{"provider":"provider","model":"model","route":"provider/model"}',
		'succeeded','sha256:parent-reentry-result','{"answer":"2"}','','',102,103
	);
	UPDATE k12_grading_assessment_items
	SET parent_guide_invocation_id='parent-reentry'
	WHERE job_id='job-v30' AND problem_id='problem-v30';
	DELETE FROM schema_migrations WHERE version=33;`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, []Migration{K12ParentGuideV33}); err != nil {
		t.Fatalf("V33 reentry: %v", err)
	}
	var parentGuideID string
	if err := db.QueryRow(`SELECT parent_guide_invocation_id
		FROM k12_grading_assessment_items
		WHERE job_id='job-v30' AND problem_id='problem-v30'`).Scan(&parentGuideID); err != nil {
		t.Fatal(err)
	}
	if parentGuideID != "parent-reentry" {
		t.Fatalf("V33 reentry lost parent guide evidence: %q", parentGuideID)
	}
}
