package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestProblemGroundingProjectionRejectsMixedStatusMissingSolveOperation(t *testing.T) {
	o, jobID, grounding, solver, grader := completedProblemGroundingFixture(t)
	items, err := o.deps.Records.ListGradingAssessmentItems(
		context.Background(), "mingming", jobID,
	)
	if err != nil || len(items) != 2 {
		t.Fatalf("list completed mixed-status fixture: items=%d err=%v", len(items), err)
	}
	if _, err := o.deps.Records.DB().ExecContext(
		context.Background(),
		`UPDATE k12_grading_assessment_items
		 SET status=?,solve_invocation_id=NULL,grade_invocation_id=NULL
		 WHERE agent_name=? AND job_id=? AND problem_id=?`,
		k12.GradingAssessmentOutOfScope, "mingming", jobID, items[0].ProblemID,
	); err != nil {
		t.Fatalf("seed mixed-status missing-solve receipt: %v", err)
	}
	freezesBefore, legacyBefore, queriesBefore := grounding.snapshot()
	solveBefore, gradeBefore := solver.callCount(), grader.callCount()
	if _, err := o.ImageTaskHomeworkProjection(
		context.Background(), "mingming", jobID,
	); err == nil {
		t.Fatal("completed public projection accepted out_of_scope without solve")
	}
	freezesAfter, legacyAfter, queriesAfter := grounding.snapshot()
	if freezesAfter != freezesBefore || legacyAfter != legacyBefore ||
		len(queriesAfter) != len(queriesBefore) ||
		solver.callCount() != solveBefore || grader.callCount() != gradeBefore {
		t.Fatal("fail-closed projection performed external work")
	}
}
