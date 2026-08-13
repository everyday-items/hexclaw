package usecase

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestK12ProjectingCurrentReceiptLineageFinalizesPastOlderTerminalAttempt(t *testing.T) {
	for _, status := range []k12.ModelInvocationStatus{
		k12.ModelInvocationFailed,
		k12.ModelInvocationOutcomeUnknown,
	} {
		t.Run(string(status), func(t *testing.T) {
			o, jobID, run, job := newCurrentReceiptLineageFixture(t, string(status))
			old := seedCurrentReceiptLineageInvocation(t, o, job, run.questions[0], status)

			item, err := o.assessDurablePhotoItem(
				context.Background(), o.deps, job, run.req, PhotoModeGrade, run.questions[0],
			)
			if err != nil {
				t.Fatalf("commit current assessment after older %s: %v", status, err)
			}
			assertCurrentReceiptReferencesLaterSucceededVerify(t, o, jobID, old)
			before := listCurrentReceiptLineageInvocations(t, o, jobID)

			forceBUG20260726031Projecting(t, o, jobID, PhotoGradeResult{
				Items: []PhotoGradeItem{item},
			})
			view, err := o.runProject(context.Background(), run, jobID)
			if err != nil {
				t.Fatalf("older %s must be covered by the later current receipt: %v", status, err)
			}
			if view.Record.Status != k12.GradingStageCompleted {
				t.Fatalf("final stage=%s, want completed", view.Record.Status)
			}
			after := listCurrentReceiptLineageInvocations(t, o, jobID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("projecting mutated or resent item operations\nbefore=%+v\nafter=%+v", before, after)
			}
			artifact := loadBUG20260726031FinalArtifact(t, o, jobID)
			if artifact.TotalCount != 1 || artifact.PublishedCount != 1 || artifact.SkippedCount != 0 {
				t.Fatalf("unexpected final artifact coverage: %+v", artifact)
			}
		})
	}
}

func TestK12ProjectingCurrentReceiptLineageStillBlocksUnsettledAttempt(t *testing.T) {
	for _, status := range []k12.ModelInvocationStatus{
		k12.ModelInvocationPrepared,
		k12.ModelInvocationSent,
	} {
		t.Run(string(status), func(t *testing.T) {
			o, jobID, run, job := newCurrentReceiptLineageFixture(t, string(status))
			old := seedCurrentReceiptLineageInvocation(t, o, job, run.questions[0], status)

			item, err := o.assessDurablePhotoItem(
				context.Background(), o.deps, job, run.req, PhotoModeGrade, run.questions[0],
			)
			if err != nil {
				t.Fatalf("commit current assessment after older %s: %v", status, err)
			}
			assertCurrentReceiptReferencesLaterSucceededVerify(t, o, jobID, old)
			before := listCurrentReceiptLineageInvocations(t, o, jobID)

			forceBUG20260726031Projecting(t, o, jobID, PhotoGradeResult{
				Items: []PhotoGradeItem{item},
			})
			view, err := o.runProject(context.Background(), run, jobID)
			if !errors.Is(err, ErrGradingFinalizationIncomplete) {
				t.Fatalf("unsettled %s err=%v, want finalization incomplete", status, err)
			}
			if view.Record.Status == k12.GradingStageCompleted {
				t.Fatalf("unsettled %s incorrectly completed the job", status)
			}
			after := listCurrentReceiptLineageInvocations(t, o, jobID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("blocked projecting mutated item operations\nbefore=%+v\nafter=%+v", before, after)
			}
			var artifacts int
			if err := o.deps.Records.DB().QueryRowContext(t.Context(), `
				SELECT COUNT(*) FROM k12_grading_final_artifacts
				WHERE agent_name='mingming' AND job_id=?`, jobID,
			).Scan(&artifacts); err != nil {
				t.Fatal(err)
			}
			if artifacts != 0 {
				t.Fatalf("unsettled %s wrote %d final artifacts", status, artifacts)
			}
		})
	}
}

func newCurrentReceiptLineageFixture(
	t *testing.T,
	suffix string,
) (*GradingOrchestrator, string, *gradingRun, GradingJobView) {
	t.Helper()
	solver := &grading20260726PhysicalSolver{
		calls: map[k12.GradingItemOperation]int{},
	}
	grader := &grading20260726PhysicalGrader{}
	o := newParallelAnchorOrchestrator(
		t,
		&countingRecognizer{questions: []RecognizedQuestion{{
			Question: "1+1=", Subject: "数学", StudentAnswer: "3",
			AnswerState: AnswerStatePresent, SourceNumberPath: []string{"1"},
			DisplayLabel: "1.", KnowledgePoints: []string{"加法"},
		}}},
		nil,
		WithGradingRunDir(t.TempDir()),
	)
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	o.deps.ParentTeachingGuide = &grading20260726RestartGuide{}
	o.deps.TutoringTipsReview = &bug20260726031TipsSpy{}
	o.deps.Profiles = &bug20260726031ProfileStore{
		profile: k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级下"},
	}
	jobID := runItemResumeJobToAssessing(t, o, "current-receipt-lineage-"+suffix)
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	return o, jobID, run, job
}

func seedCurrentReceiptLineageInvocation(
	t *testing.T,
	o *GradingOrchestrator,
	job GradingJobView,
	question RecognizedQuestion,
	status k12.ModelInvocationStatus,
) k12.GradingItemInvocation {
	t.Helper()
	now := o.deps.now()
	invocation, created, err := o.deps.Records.PrepareGradingItemInvocation(
		context.Background(),
		k12.GradingItemInvocation{
			InvocationID:     "current-receipt-lineage-" + string(status),
			AgentName:        job.Record.AgentName,
			JobID:            job.Record.RecordID,
			ProblemID:        question.ProblemID,
			AttemptID:        question.AttemptID,
			Operation:        k12.GradingItemOperationSolveVerify,
			OperationAttempt: 1,
			RequestDigest:    "sha256:older-request-" + string(status),
			RouteSnapshot:    job.Fields.ModelSnapshot,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
	)
	if err != nil || !created {
		t.Fatalf("prepare older %s invocation created=%v err=%v", status, created, err)
	}
	if status == k12.ModelInvocationPrepared {
		return invocation
	}
	invocation, err = o.deps.Records.MarkGradingItemInvocationSent(
		context.Background(), job.Record.AgentName, invocation.InvocationID,
	)
	if err != nil {
		t.Fatalf("mark older %s sent: %v", status, err)
	}
	switch status {
	case k12.ModelInvocationSent:
		return invocation
	case k12.ModelInvocationFailed:
		invocation, err = o.deps.Records.MarkGradingItemInvocationFailed(
			context.Background(), job.Record.AgentName, invocation.InvocationID,
			"provider_response", "http_503",
		)
	case k12.ModelInvocationOutcomeUnknown:
		invocation, err = o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
			context.Background(), job.Record.AgentName, invocation.InvocationID,
			"provider_transport", "outcome_unknown",
		)
	default:
		t.Fatalf("unsupported seeded status %s", status)
	}
	if err != nil {
		t.Fatalf("mark older invocation %s: %v", status, err)
	}
	return invocation
}

func assertCurrentReceiptReferencesLaterSucceededVerify(
	t *testing.T,
	o *GradingOrchestrator,
	jobID string,
	old k12.GradingItemInvocation,
) {
	t.Helper()
	assessments, err := o.deps.Records.ListGradingAssessmentItems(
		context.Background(), old.AgentName, jobID,
	)
	if err != nil || len(assessments) != 1 {
		t.Fatalf("current assessments=%+v err=%v", assessments, err)
	}
	invocations := listCurrentReceiptLineageInvocations(t, o, jobID)
	for _, invocation := range invocations {
		if invocation.InvocationID != assessments[0].SolveInvocationID {
			continue
		}
		if invocation.Operation != k12.GradingItemOperationSolveVerify ||
			invocation.Status != k12.ModelInvocationSucceeded ||
			invocation.OperationAttempt <= old.OperationAttempt {
			t.Fatalf("current solve reference is not a later succeeded verify: %+v", invocation)
		}
		return
	}
	t.Fatalf("current assessment solve reference %q is absent", assessments[0].SolveInvocationID)
}

func listCurrentReceiptLineageInvocations(
	t *testing.T,
	o *GradingOrchestrator,
	jobID string,
) []k12.GradingItemInvocation {
	t.Helper()
	invocations, err := o.deps.Records.ListGradingItemInvocations(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return invocations
}
