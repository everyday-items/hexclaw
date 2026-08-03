package k12storage_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestHasPendingCurrentProblemSourceRecognitionActionTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		action string
		want   bool
	}{
		{name: "select_region blocks", action: "select_region", want: true},
		{name: "retake blocks", action: "retake", want: true},
		{name: "correct_text does not block", action: "correct_text", want: false},
		{name: "resume does not block", action: "resume", want: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, db := setup(t)
			seedProblemSourceRecognitionFixtureForAction(
				t,
				store,
				db,
				recognitionWork,
				test.action,
			)

			got, err := store.HasPendingCurrentProblemSourceRecognition(
				context.Background(),
				"mingming",
				recognitionJob,
			)
			if err != nil || got != test.want {
				t.Fatalf("pending=%v want=%v err=%v", got, test.want, err)
			}
		})
	}
}

func TestHasPendingCurrentProblemSourceRecognitionResultAndHeadCases(t *testing.T) {
	t.Parallel()

	t.Run("same work V73 result exists", func(t *testing.T) {
		t.Parallel()
		store, db := setup(t)
		seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		insertGateRecognitionResult(t, db)

		got, err := store.HasPendingCurrentProblemSourceRecognition(
			context.Background(),
			"mingming",
			recognitionJob,
		)
		if err != nil || got {
			t.Fatalf("pending=%v want=false err=%v", got, err)
		}
	})

	t.Run("old work input head is superseded", func(t *testing.T) {
		t.Parallel()
		store, db := setup(t)
		seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		supersedeGateInputHeads(t, db)

		got, err := store.HasPendingCurrentProblemSourceRecognition(
			context.Background(),
			"mingming",
			recognitionJob,
		)
		if err != nil || got {
			t.Fatalf("pending=%v want=false err=%v", got, err)
		}
	})
}

func TestHasPendingCurrentProblemSourceRecognitionScopeIsolation(t *testing.T) {
	t.Parallel()
	store, db := setup(t)
	seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
	if _, err := db.Exec(`
		INSERT INTO k12_grading_jobs (
			record_id,agent_name,status,submission_id,source_kind,idempotency_key,
			dedupe_key,created_at,updated_at
		) VALUES (
			'recognition-other-job','mingming','active','recognition-other-submission',
			'desktop','recognition-other-job-key','recognition-other-job-dedupe',100,100
		)`); err != nil {
		t.Fatalf("seed independent grading job: %v", err)
	}

	tests := []struct {
		name      string
		agentName string
		jobID     string
	}{
		{
			name:      "agent owner differs",
			agentName: "other-agent",
			jobID:     recognitionJob,
		},
		{
			name:      "job and submission differ",
			agentName: "mingming",
			jobID:     "recognition-other-job",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := store.HasPendingCurrentProblemSourceRecognition(
				context.Background(),
				test.agentName,
				test.jobID,
			)
			if err != nil || got {
				t.Fatalf("pending=%v want=false err=%v", got, err)
			}
		})
	}

	got, err := store.HasPendingCurrentProblemSourceRecognition(
		context.Background(),
		"mingming",
		recognitionJob,
	)
	if err != nil || !got {
		t.Fatalf("canonical scope pending=%v want=true err=%v", got, err)
	}
}

func insertGateRecognitionResult(t *testing.T, db *sql.DB) {
	t.Helper()
	result, err := db.Exec(`
		INSERT INTO k12_problem_source_recognition_results (
			work_id,command_receipt_id,owner_scope,agent_name,submission_id,
			dispatch_id,job_id,path_problem_id,parent_invocation_id,
			parent_request_digest,parent_invocation_attempt,action,structure_version,
			source_input_revision,result_input_revision,result_digest,mapping_state,
			structure_digest,affected_problem_ids_json,created_at
		)
		SELECT work.work_id,work.command_receipt_id,work.owner_scope,work.agent_name,
		       job.submission_id,work.dispatch_id,work.job_id,work.problem_id,
		       invocation.invocation_id,invocation.request_digest,invocation.attempt,
		       work.action,work.structure_version,work.input_revision,
		       work.input_revision+1,?,'stable_exact_set',snapshot.structure_digest,
		       work.affected_problem_ids_json,101
		FROM k12_problem_source_reprocess_jobs work
		JOIN k12_grading_jobs job
		  ON job.agent_name=work.agent_name AND job.record_id=work.job_id
		JOIN k12_problem_structure_snapshots snapshot
		  ON snapshot.agent_name=work.agent_name
		 AND snapshot.submission_id=job.submission_id
		 AND snapshot.structure_version=work.structure_version
		JOIN k12_model_invocations invocation
		  ON invocation.agent_name=work.agent_name
		 AND invocation.job_id=work.job_id
		 AND invocation.stage='recognizing'
		WHERE work.work_id=?`,
		strings.Repeat("f", 64),
		recognitionWork,
	)
	if err != nil {
		t.Fatalf("insert same-work V73 result: %v", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		t.Fatalf("insert same-work V73 rows=%d err=%v", rows, err)
	}
}

func supersedeGateInputHeads(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		UPDATE k12_problem_input_revisions
		SET current_disposition='superseded',updated_at=101
		WHERE agent_name='mingming' AND submission_id=?
		  AND structure_version=1 AND input_revision=2
		  AND current_disposition='current'`, recognitionSubmission); err != nil {
		t.Fatalf("supersede old source input heads: %v", err)
	}
	result, err := tx.Exec(`
		INSERT INTO k12_problem_input_revisions (
			agent_name,submission_id,structure_version,problem_id,input_revision,
			page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
			question_canonical_markdown,answer_canonical_markdown,input_digest,
			current_disposition,origin_command_receipt_id,origin_kind,created_at,updated_at
		)
		SELECT agent_name,submission_id,structure_version,problem_id,3,
		       page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
		       question_canonical_markdown,answer_canonical_markdown,
		       'sha256:later-' || problem_id,'current',origin_command_receipt_id,
		       'command',101,101
		FROM k12_problem_input_revisions
		WHERE agent_name='mingming' AND submission_id=?
		  AND structure_version=1 AND input_revision=2`, recognitionSubmission)
	if err != nil {
		t.Fatalf("append later current input heads: %v", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 2 {
		t.Fatalf("append later current input heads rows=%d err=%v", rows, err)
	}
	if _, err := tx.Exec(`
		UPDATE k12_problem_structure_members
		SET input_revision=3
		WHERE agent_name='mingming' AND submission_id=?
		  AND structure_version=1 AND problem_id IN (?,?)`,
		recognitionSubmission,
		recognitionChildOne,
		recognitionChildTwo,
	); err != nil {
		t.Fatalf("advance structure member input heads: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
