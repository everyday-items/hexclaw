package apihttp_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// PROG-022: a changed page exact-set creates one new authoritative structure
// version. Old input heads, skip receipts and dependency-group state remain
// auditable but cannot participate in the new current projection.
func TestBUG_20260726_031_StructureChangeCreatesIsolatedAuthoritativeVersion(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	ctx := context.Background()

	skipV1, skipBody := postProblemSourceAction(
		t,
		seed.fixture.handler,
		seed.dispatchID,
		seed.problemID,
		"structure-v1-skip-r1",
		validSkipSourceActionBody,
	)
	if skipV1.Code != http.StatusOK {
		t.Fatalf("seed v1 skip: status=%d body=%#v", skipV1.Code, skipBody)
	}
	resumeV1, resumeBody := postProblemSourceAction(
		t,
		seed.fixture.handler,
		seed.dispatchID,
		seed.problemID,
		"structure-v1-resume-r1",
		`{"action":"resume","structure_version":1,"expected_input_revision":1,"payload":{}}`,
	)
	if resumeV1.Code != http.StatusOK {
		t.Fatalf("seed v1 resume: status=%d body=%#v", resumeV1.Code, resumeBody)
	}
	skipV1R2, skipR2Body := postProblemSourceAction(
		t,
		seed.fixture.handler,
		seed.dispatchID,
		seed.problemID,
		"structure-v1-skip-r2",
		`{"action":"skip","structure_version":1,"expected_input_revision":2,"payload":{}}`,
	)
	if skipV1R2.Code != http.StatusOK {
		t.Fatalf("seed v1 revision 2 skip: status=%d body=%#v", skipV1R2.Code, skipR2Body)
	}

	for _, table := range []string{
		"k12_problem_structure_snapshots",
		"k12_problem_structure_members",
		"k12_problem_structure_mappings",
		"k12_problem_dependency_groups",
	} {
		var count int
		if err := seed.fixture.db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
			WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("PROG-022 authoritative structure table %q missing", table)
		}
	}

	var oldDependencyGroupID string
	if err := seed.fixture.db.QueryRow(`
		SELECT dependency_group_id
		FROM k12_problem_structure_members
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND structure_version=1 AND problem_id=?`,
		seed.problemID,
	).Scan(&oldDependencyGroupID); err != nil {
		t.Fatalf("read v1 dependency group: %v", err)
	}
	if _, err := seed.fixture.db.Exec(`
		UPDATE k12_problem_dependency_groups
		SET state='completed',state_revision=7,updated_at=created_at+1
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND structure_version=1 AND dependency_group_id=?`,
		oldDependencyGroupID,
	); err != nil {
		t.Fatalf("seed completed v1 dependency state: %v", err)
	}

	current, err := seed.fixture.coordinator.Records.GetProblemAttemptSnapshot(
		ctx,
		"mingming",
		"submission-source-action",
	)
	if err != nil {
		t.Fatalf("read v1 recognized structure: %v", err)
	}
	changed := k12.ProblemAttemptSnapshot{
		Problems: append([]k12.Problem(nil), current.Problems...),
		Attempts: append([]k12.Attempt(nil), current.Attempts...),
	}
	changed.Problems = append(changed.Problems, k12.Problem{
		ProblemID:            "problem-source-action-added",
		AgentName:            "mingming",
		SubmissionID:         "submission-source-action",
		PageAssetID:          current.Problems[0].PageAssetID,
		Ordinal:              1,
		ProblemKind:          k12.ProblemKindStandalone,
		Subject:              "数学",
		StemRaw:              "2+2=",
		StemMarkdown:         "2+2=",
		ConfirmationRequired: true,
		ConfirmationReasons: []string{
			"source_unclear",
		},
		CanonicalVersion: 1,
	})
	changed.Attempts = append(changed.Attempts, k12.Attempt{
		AttemptID:        "attempt-source-action-added",
		AgentName:        "mingming",
		SubmissionID:     "submission-source-action",
		ProblemID:        "problem-source-action-added",
		AnswerState:      "unclear",
		ConfirmedVersion: 1,
		InputDigest:      "sha256:source-action-added-input",
	})
	if err := seed.fixture.coordinator.Records.PutProblemAttemptSnapshot(ctx, changed); err != nil {
		t.Fatalf("persist changed exact-set: %v", err)
	}
	if err := seed.fixture.coordinator.Records.PutProblemAttemptSnapshot(ctx, changed); err != nil {
		t.Fatalf("idempotently replay changed exact-set: %v", err)
	}

	var versions, currentVersion int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*),
		       MAX(CASE WHEN current_disposition='current' THEN structure_version ELSE 0 END)
		FROM k12_problem_structure_snapshots
		WHERE agent_name='mingming' AND submission_id='submission-source-action'`,
	).Scan(&versions, &currentVersion); err != nil {
		t.Fatal(err)
	}
	if versions != 2 || currentVersion != 2 {
		t.Fatalf("structure snapshots=(count=%d current=%d), want (2,2)",
			versions, currentVersion)
	}

	var currentMembers int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*)
		FROM k12_problem_structure_members
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND structure_version=2`,
	).Scan(&currentMembers); err != nil {
		t.Fatal(err)
	}
	if currentMembers != 2 {
		t.Fatalf("v2 current exact-set members=%d, want 2", currentMembers)
	}
	var stableMappings, newMappings int
	if err := seed.fixture.db.QueryRow(`
		SELECT
		  SUM(CASE WHEN mapping_kind='stable'
		                 AND old_problem_id=? AND new_problem_id=? THEN 1 ELSE 0 END),
		  SUM(CASE WHEN mapping_kind='new'
		                 AND old_problem_id='' AND new_problem_id='problem-source-action-added'
		           THEN 1 ELSE 0 END)
		FROM k12_problem_structure_mappings
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND from_structure_version=1 AND to_structure_version=2`,
		seed.problemID,
		seed.problemID,
	).Scan(&stableMappings, &newMappings); err != nil {
		t.Fatal(err)
	}
	if stableMappings != 1 || newMappings != 1 {
		t.Fatalf("explicit mappings stable=%d new=%d, want 1/1",
			stableMappings, newMappings)
	}

	var oldCurrentSkips, oldSupersededSkips int
	if err := seed.fixture.db.QueryRow(`
		SELECT
		  SUM(CASE WHEN current_disposition='current' THEN 1 ELSE 0 END),
		  SUM(CASE WHEN current_disposition='superseded' THEN 1 ELSE 0 END)
		FROM k12_problem_skip_receipts
		WHERE agent_name='mingming' AND job_id=? AND problem_id=?`,
		seed.jobID,
		seed.problemID,
	).Scan(&oldCurrentSkips, &oldSupersededSkips); err != nil {
		t.Fatal(err)
	}
	if oldCurrentSkips != 0 || oldSupersededSkips != 2 {
		t.Fatalf("old skips current=%d superseded=%d, want 0/2",
			oldCurrentSkips, oldSupersededSkips)
	}

	var v1Completed, v2Pending int
	if err := seed.fixture.db.QueryRow(`
		SELECT
		  SUM(CASE WHEN structure_version=1 AND state='completed' THEN 1 ELSE 0 END),
		  SUM(CASE WHEN structure_version=2 AND state='pending' THEN 1 ELSE 0 END)
		FROM k12_problem_dependency_groups
		WHERE agent_name='mingming' AND submission_id='submission-source-action'`,
	).Scan(&v1Completed, &v2Pending); err != nil {
		t.Fatal(err)
	}
	if v1Completed != 1 || v2Pending != 2 {
		t.Fatalf("dependency groups old-completed=%d new-pending=%d, want 1/2",
			v1Completed, v2Pending)
	}

	stale, staleBody := postProblemSourceAction(
		t,
		seed.fixture.handler,
		seed.dispatchID,
		seed.problemID,
		"structure-stale-v1",
		`{"action":"skip","structure_version":1,"expected_input_revision":1,"payload":{}}`,
	)
	if stale.Code != http.StatusConflict {
		t.Fatalf("old expected structure = %d, want 409; body=%#v",
			stale.Code, staleBody)
	}

	_, _, err = seed.fixture.coordinator.Records.CommitGradingAssessmentItem(
		ctx,
		k12.GradingAssessmentItem{
			AgentName:        "mingming",
			JobID:            seed.jobID,
			ProblemID:        seed.problemID,
			AttemptID:        "attempt-source-action",
			ConfirmedVersion: 1,
			InputRevision:    1,
			StructureVersion: 1,
			InputDigest:      "sha256:source-action-input",
			Status:           k12.GradingAssessmentUnanswered,
			ResultJSON:       `{"status":"unanswered"}`,
			ResultDigest:     "sha256:late-old-structure",
			ProjectionStatus: k12.GradingProjectionCommitted,
		},
		k12storage.GradingAssessmentEffects{},
	)
	if !errors.Is(err, k12storage.ErrGradingAssessmentItemConflict) {
		t.Fatalf("late v1 assessment after authoritative v2: err=%v, want structure conflict", err)
	}
	var currentOldAssessments int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*)
		FROM k12_grading_assessment_items
		WHERE agent_name='mingming' AND job_id=? AND problem_id=?
		  AND structure_version=1 AND current_disposition='current'`,
		seed.jobID,
		seed.problemID,
	).Scan(&currentOldAssessments); err != nil {
		t.Fatal(err)
	}
	if currentOldAssessments != 0 {
		t.Fatalf("late v1 assessment polluted v2 current projection: current v1 rows=%d",
			currentOldAssessments)
	}

	var v2InputRevision int
	if err := seed.fixture.db.QueryRow(`
		SELECT input_revision
		FROM k12_problem_structure_members
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND structure_version=2 AND problem_id=?`,
		seed.problemID,
	).Scan(&v2InputRevision); err != nil {
		t.Fatal(err)
	}
	if v2InputRevision <= 2 {
		t.Fatalf("v2 stable problem input revision=%d, want a new monotonic head above v1 revision 2",
			v2InputRevision)
	}
	var v2LedgerRevision, v2AttemptRevision int
	if err := seed.fixture.db.QueryRow(`
		SELECT input_revision
		FROM k12_problem_input_revisions
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND structure_version=2 AND problem_id=?
		  AND current_disposition='current'`,
		seed.problemID,
	).Scan(&v2LedgerRevision); err != nil {
		t.Fatalf("read v2 immutable input head: %v", err)
	}
	if err := seed.fixture.db.QueryRow(`
		SELECT confirmed_version
		FROM k12_attempts
		WHERE agent_name='mingming' AND attempt_id='attempt-source-action'`,
	).Scan(&v2AttemptRevision); err != nil {
		t.Fatalf("read v2 Attempt revision: %v", err)
	}
	if v2LedgerRevision != v2InputRevision || v2AttemptRevision != v2InputRevision {
		t.Fatalf(
			"v2 revision barrier drift: structure=%d input_head=%d attempt=%d",
			v2InputRevision,
			v2LedgerRevision,
			v2AttemptRevision,
		)
	}
	fresh, freshBody := postProblemSourceAction(
		t,
		seed.fixture.handler,
		seed.dispatchID,
		seed.problemID,
		"structure-current-v2",
		fmt.Sprintf(
			`{"action":"skip","structure_version":2,"expected_input_revision":%d,"payload":{}}`,
			v2InputRevision,
		),
	)
	if fresh.Code != http.StatusOK {
		t.Fatalf("new structure must start without old revision pollution: status=%d body=%#v",
			fresh.Code, freshBody)
	}
	resumed, resumedBody := postProblemSourceAction(
		t,
		seed.fixture.handler,
		seed.dispatchID,
		seed.problemID,
		"structure-current-v2-resume",
		fmt.Sprintf(
			`{"action":"resume","structure_version":2,"expected_input_revision":%d,"payload":{}}`,
			v2InputRevision,
		),
	)
	if resumed.Code != http.StatusOK {
		t.Fatalf(
			"new structure resume must advance the aligned input head: status=%d body=%#v",
			resumed.Code,
			resumedBody,
		)
	}
	if err := seed.fixture.coordinator.Records.PutProblemAttemptSnapshot(
		ctx,
		changed,
	); !errors.Is(err, k12storage.ErrProblemAttemptConflict) {
		t.Fatalf("stale OCR snapshot after command-origin head err=%v, want immutable conflict", err)
	}
	var commandHeadRevision int
	var commandHeadOrigin string
	if err := seed.fixture.db.QueryRow(`
		SELECT input_revision,origin_kind
		FROM k12_problem_input_revisions
		WHERE agent_name='mingming' AND submission_id='submission-source-action'
		  AND structure_version=2 AND problem_id=?
		  AND current_disposition='current'`,
		seed.problemID,
	).Scan(&commandHeadRevision, &commandHeadOrigin); err != nil {
		t.Fatalf("read command-origin v2 input head: %v", err)
	}
	if commandHeadRevision != v2InputRevision+1 || commandHeadOrigin != "command" {
		t.Fatalf(
			"stale OCR replay changed command head: revision=%d origin=%q",
			commandHeadRevision,
			commandHeadOrigin,
		)
	}
}
