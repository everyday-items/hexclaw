package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestGradingJobBlankWorksheetCommitsAndReplaysPerItemParentGuide(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "q1", Subject: "数学", AnswerState: AnswerStateBlank,
			KnowledgePoints: []string{"加法"},
		},
		{
			Question: "q2", Subject: "数学", AnswerState: AnswerStateBlank,
			KnowledgePoints: []string{"减法"},
		},
	}, solver, grader)
	generator := &parentTeachingGuideSpy{}
	o.deps.ParentTeachingGuide = generator

	jobID := runItemResumeJobToAssessing(t, o, "blank-parent-guide-receipt")
	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil || completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("blank worksheet completion: stage=%s err=%v", completed.Record.Status, err)
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || len(result.Items) != 2 {
		t.Fatalf("blank worksheet result missing: ok=%v result=%#v", ok, result)
	}
	if calls := generator.snapshot(); len(calls) != 0 {
		t.Fatalf("numeric-exec parent guides must not call Provider: %#v", calls)
	}
	invocations, err := o.deps.Records.ListGradingItemInvocations(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	operations := make(map[string][]k12.GradingItemOperation)
	for _, invocation := range invocations {
		operations[invocation.ProblemID] = append(operations[invocation.ProblemID], invocation.Operation)
		if invocation.RouteSnapshot != completed.Fields.ModelSnapshot {
			t.Fatalf("blank item route drifted from frozen job route: item=%+v job=%+v",
				invocation.RouteSnapshot, completed.Fields.ModelSnapshot)
		}
	}

	for index, item := range result.Items {
		assertExactGradingItemOperations(t, operations[item.Recognized.ProblemID],
			k12.GradingItemOperationSolve,
			k12.GradingItemOperationParentGuide,
		)
		if item.ParentGuide == nil ||
			item.ParentGuide.Answer != "2" ||
			!reflect.DeepEqual(item.ParentGuide.FullSolutionSteps, []string{"2"}) {
			t.Fatalf("item %d parent guide missing/canonical answer drifted: %#v", index, item)
		}
		receipt, err := o.deps.Records.GetGradingAssessmentItem(
			context.Background(), "mingming", jobID, item.Recognized.ProblemID,
		)
		if err != nil {
			t.Fatalf("item %d receipt: %v", index, err)
		}
		if receipt.ParentGuideInvocationID == "" {
			t.Fatalf("item %d receipt has no durable parent-guide invocation: %+v", index, receipt)
		}
		parentInvocation, err := o.deps.Records.GetGradingItemInvocation(
			context.Background(),
			"mingming",
			receipt.ParentGuideInvocationID,
		)
		if err != nil ||
			parentInvocation.Operation != k12.GradingItemOperationParentGuide ||
			parentInvocation.Status != k12.ModelInvocationSucceeded {
			t.Fatalf("item %d parent guide invocation=%+v err=%v", index, parentInvocation, err)
		}
		replayed, err := replayGradingAssessmentItem(item.Recognized, receipt)
		if err != nil {
			t.Fatalf("item %d replay: %v", index, err)
		}
		if replayed.ParentGuide == nil ||
			replayed.ParentGuide.Answer != item.ParentGuide.Answer ||
			!reflect.DeepEqual(replayed.ParentGuide.FullSolutionSteps, item.ParentGuide.FullSolutionSteps) ||
			replayed.ParentGuide.GradeLevelMethod != item.ParentGuide.GradeLevelMethod {
			t.Fatalf("item %d durable receipt lost guide: live=%#v replay=%#v", index, item, replayed)
		}
	}
}

func TestGradingAssessmentReceiptDigestIgnoresDerivedResultKind(t *testing.T) {
	legacy := PhotoGradeItem{
		Recognized: RecognizedQuestion{ProblemID: "problem-1", AttemptID: "attempt-1"},
		Status:     PhotoCorrect,
	}
	current := legacy
	current.ResultKind = PhotoItemAssessment

	legacyJSON, err := json.Marshal(gradingAssessmentCanonicalResult(legacy))
	if err != nil {
		t.Fatal(err)
	}
	currentJSON, err := json.Marshal(gradingAssessmentCanonicalResult(current))
	if err != nil {
		t.Fatal(err)
	}
	if string(currentJSON) != string(legacyJSON) {
		t.Fatalf("derived result kind changed immutable receipt digest:\nlegacy=%s\ncurrent=%s", legacyJSON, currentJSON)
	}
}

func TestFrozenAssessIdentityConflictBeforeSendIsTerminalWithoutItemProvider(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q1", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "assess-identity-conflict-before-send")
	_, job := confirmItemResumeJobWithoutRun(t, o, jobID)

	if _, _, err := o.deps.Records.PrepareModelInvocation(context.Background(), k12.ModelInvocation{
		InvocationID: "modelinv-assess-conflicting-route", AgentName: "mingming",
		JobID: jobID, Stage: k12.GradingStageAssessing,
		RequestDigest: "sha256:conflicting-request",
		RouteSnapshot: k12.GradingModelSnapshot{
			Provider: "old-provider", Model: "old-model", Route: "old-provider/old-model",
		},
		Attempt: job.Fields.AttemptCount + 1, CreatedAt: o.deps.now(), UpdatedAt: o.deps.now(),
	}); err != nil {
		t.Fatal(err)
	}

	failed, err := o.RunGradingJob(context.Background(), jobID)
	if !errors.Is(err, k12storage.ErrModelInvocationConflict) {
		t.Fatalf("run error=%v, want immutable assessing invocation conflict", err)
	}
	if failed.Record.Status != k12.GradingStageFailedTerminal ||
		failed.Fields.Retryable ||
		failed.Fields.FailureKind != "invocation_identity_conflict" {
		t.Fatalf("pre-send assessing conflict must fail terminal, got record=%+v fields=%+v",
			failed.Record, failed.Fields)
	}
	if solver.callCount("q1") != 0 || grader.callCount("q1") != 0 {
		t.Fatalf("item provider ran after pre-send conflict: solver=%d grader=%d",
			solver.callCount("q1"), grader.callCount("q1"))
	}
}
