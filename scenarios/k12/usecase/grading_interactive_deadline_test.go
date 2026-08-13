package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestGradingIndependentCallContextClampsToParentDeadlineWithoutParentCancellation(t *testing.T) {
	parentDeadline := time.Now().Add(time.Minute)
	parent, cancelParent := context.WithDeadline(context.Background(), parentDeadline)
	child, cancelChild := gradingIndependentCallContext(parent, int((5*time.Minute)/time.Millisecond))
	defer cancelChild()

	gotDeadline, ok := child.Deadline()
	if !ok || !gotDeadline.Equal(parentDeadline) {
		t.Fatalf("child deadline=%v ok=%v, want parent deadline=%v", gotDeadline, ok, parentDeadline)
	}

	cancelParent()
	if err := child.Err(); err != nil {
		t.Fatalf("explicit parent cancellation propagated through independent call context: %v", err)
	}
}

func TestGradingRecoveryInteractiveDeadlineFailureStaysParked(t *testing.T) {
	const wantFailureKind = "interactive_deadline_exceeded"
	ctx := context.Background()
	dir := t.TempDir()
	rec := &countingRecognizer{
		failures: 1,
		questions: []RecognizedQuestion{{
			Question: "1+1=", Subject: "数学", StudentAnswer: "3",
		}},
	}
	d := recoveryDeps(t, rec,
		&photoAnchorerFake{boxes: map[int]BBox{0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05}}},
		&photoAnnotatorFake{},
	)

	o1 := newRecoverableOrchestrator(t, d, dir)
	view, _, err := o1.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "interactive-deadline",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := view.Record.RecordID
	if _, err = o1.RunGradingJob(ctx, jobID); err == nil {
		t.Fatal("first recognition call must fail to create a retryable checkpoint")
	}
	failed, err := d.GetGradingJob(ctx, "mingming", jobID)
	if err != nil {
		t.Fatalf("GetGradingJob: %v", err)
	}
	if failed.Record.Status != k12.GradingStageFailedRetryable || !failed.Fields.Retryable {
		t.Fatalf("seed stage=%s retryable=%v, want failed_retryable true",
			failed.Record.Status, failed.Fields.Retryable)
	}
	failed.Fields.FailureKind = wantFailureKind
	raw, err := json.Marshal(failed.Fields)
	if err != nil {
		t.Fatalf("marshal interactive deadline fields: %v", err)
	}
	if err = d.Records.UpdateStatusFields(ctx, jobID, failed.Record.Status,
		failed.Record.DueAt, string(raw), failed.Record.Version); err != nil {
		t.Fatalf("persist interactive deadline failure: %v", err)
	}

	o2 := newRecoverableOrchestrator(t, d, dir)
	if _, err = o2.RecoverGradingJobs(ctx, []string{"mingming"}); err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	idleCtx, cancelIdle := context.WithTimeout(ctx, time.Second)
	defer cancelIdle()
	if err = o2.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}

	got, err := d.GetGradingJob(ctx, "mingming", jobID)
	if err != nil {
		t.Fatalf("GetGradingJob after recovery: %v", err)
	}
	if got.Record.Status != k12.GradingStageFailedRetryable ||
		got.Fields.FailureKind != wantFailureKind ||
		!got.Fields.Retryable {
		t.Fatalf("interactive deadline recovery mutated parked job: stage=%s fields=%+v",
			got.Record.Status, got.Fields)
	}
	if rec.calls != 1 {
		t.Fatalf("interactive deadline recovery resent provider call: calls=%d, want 1", rec.calls)
	}
}

func gradingParentFrozenBudget() k12.GradingBudgetSnapshot {
	return k12.GradingBudgetSnapshot{
		PolicyVersion:          1,
		RecognitionPlanVersion: k12.RecognitionPlanVersionV1,
		StageSeconds: k12.GradingStageBudgets{
			Queued: 60, Normalizing: 60, Recognizing: 120,
			Locating: 60, Rendering: 60, Projecting: 60,
		},
		AssessingBuckets: []k12.GradingAssessingBudgetBucket{
			{MaxProblems: 1, Seconds: 90},
			{MaxProblems: 8, Seconds: 180},
			{MaxProblems: 16, Seconds: 300},
			{MaxProblems: 32, Seconds: 540},
		},
		ItemConcurrency: 2,
	}
}

func TestStartPhotoGradingPersistsParentAutomaticAttemptAndClampsQueuedDeadline(t *testing.T) {
	const now = int64(1_000)
	d := recoveryDeps(t, &countingRecognizer{}, nil, &photoAnnotatorFake{})
	d.Now = func() int64 { return now }
	o := newRecoverableOrchestrator(t, d, t.TempDir())

	view, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo:                     orchestratorPhotoRequest(),
		SourceKind:                "image_task",
		SourceKey:                 "dispatch-parent-budget:1",
		BudgetSnapshot:            gradingParentFrozenBudget(),
		ParentAutomaticAttemptID:  "dispatch-parent-budget:1",
		ParentAutomaticDeadlineAt: now + 30,
	})
	if err != nil || !created {
		t.Fatalf("StartPhotoGradingJob: created=%v err=%v", created, err)
	}
	if view.Fields.ParentAutomaticAttemptID != "dispatch-parent-budget:1" ||
		view.Fields.ParentAutomaticDeadlineAt != now+30 ||
		view.Fields.ParentAutomaticRemainingSeconds != 30 {
		t.Fatalf("parent automatic facts not persisted: %+v", view.Fields)
	}
	if view.Fields.Deadline != now+30 {
		t.Fatalf("queued deadline=%d, want parent cap=%d", view.Fields.Deadline, now+30)
	}
}

func TestGradingParentAutomaticDeadlineClampsEveryAutomaticStage(t *testing.T) {
	const now = int64(2_000)
	d := recoveryDeps(t, &countingRecognizer{}, nil, &photoAnnotatorFake{})
	d.Now = func() int64 { return now }
	for _, stage := range []string{
		k12.GradingStageQueued,
		k12.GradingStageNormalizing,
		k12.GradingStageRecognizing,
		k12.GradingStageLocating,
		k12.GradingStageAssessing,
		k12.GradingStageRendering,
		k12.GradingStageProjecting,
	} {
		t.Run(stage, func(t *testing.T) {
			fields := k12.GradingJobFields{
				SubmissionID:                    "sub-parent-stage-cap",
				ParentAutomaticAttemptID:        "dispatch-parent-stage-cap:1",
				ParentAutomaticDeadlineAt:       now + 7,
				ParentAutomaticRemainingSeconds: 7,
			}
			if err := d.setGradingDeadline(context.Background(), "mingming", &fields, stage); err != nil {
				t.Fatalf("setGradingDeadline(%s): %v", stage, err)
			}
			if fields.Deadline != now+7 {
				t.Fatalf("stage=%s deadline=%d, want parent cap=%d", stage, fields.Deadline, now+7)
			}
		})
	}
}

func TestGradingParentAutomaticBudgetPausesForConfirmationAndResumesRemaining(t *testing.T) {
	now := int64(3_000)
	d := recoveryDeps(t, &countingRecognizer{}, nil, &photoAnnotatorFake{})
	d.Now = func() int64 { return now }
	d.GradingBudgetSnapshot = k12.GradingBudgetSnapshot{}
	view, created, err := d.CreateGradingJob(context.Background(), "mingming", "session-parent-pause", CreateGradingJobInput{
		SubmissionID:              "sub-parent-pause",
		SourceKind:                "desktop",
		SourceKey:                 "parent-pause",
		ModelSnapshot:             k12.GradingModelSnapshot{Provider: "openrouter", Model: "test-vlm"},
		ParentAutomaticAttemptID:  "dispatch-parent-pause:1",
		ParentAutomaticDeadlineAt: now + 100,
	})
	if err != nil || !created {
		t.Fatalf("CreateGradingJob: created=%v err=%v", created, err)
	}
	jobID := view.Record.RecordID
	now += 10
	if view, err = d.AdvanceGradingStage(context.Background(), "mingming", jobID,
		AdvanceGradingInput{Outcome: GradingOutcomeOK}); err != nil {
		t.Fatal(err)
	}
	now += 10
	if view, err = d.AdvanceGradingStage(context.Background(), "mingming", jobID,
		AdvanceGradingInput{Outcome: GradingOutcomeOK}); err != nil {
		t.Fatal(err)
	}
	now += 10
	if view, err = d.AdvanceGradingStage(context.Background(), "mingming", jobID,
		AdvanceGradingInput{Outcome: GradingOutcomeOK}); err != nil {
		t.Fatal(err)
	}
	if view.Record.Status != k12.GradingStageAwaitingConfirmation ||
		view.Fields.ParentAutomaticDeadlineAt != 0 ||
		view.Fields.ParentAutomaticRemainingSeconds != 70 {
		t.Fatalf("confirmation did not freeze 70 seconds: stage=%s fields=%+v",
			view.Record.Status, view.Fields)
	}

	now += 1_000
	if view, err = d.AdvanceGradingStage(context.Background(), "mingming", jobID,
		AdvanceGradingInput{
			Outcome: GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated,
		}); err != nil {
		t.Fatal(err)
	}
	if view, err = d.ConfirmGradingJob(context.Background(), "mingming", jobID, nil); err != nil {
		t.Fatal(err)
	}
	if view.Record.Status != k12.GradingStageAssessing ||
		view.Fields.ParentAutomaticDeadlineAt != now+70 ||
		view.Fields.ParentAutomaticRemainingSeconds != 70 ||
		view.Fields.Deadline != now+70 {
		t.Fatalf("confirmation did not resume frozen parent budget: stage=%s fields=%+v",
			view.Record.Status, view.Fields)
	}
}

func TestBUG20260802ImageTaskAssessingReservationUsesFrozenProblemBucket(t *testing.T) {
	const now = int64(7_000)
	budget := k12.GradingBudgetSnapshot{
		PolicyVersion:          1,
		RecognitionPlanVersion: k12.RecognitionPlanVersionV1,
		StageSeconds: k12.GradingStageBudgets{
			Queued: 60, Normalizing: 60, Recognizing: 60,
			Locating: 60, Rendering: 60, Projecting: 60,
		},
		AssessingBuckets: []k12.GradingAssessingBudgetBucket{
			{MaxProblems: 1, Seconds: 90},
			{MaxProblems: 8, Seconds: 180},
			{MaxProblems: 16, Seconds: 600},
			{MaxProblems: 32, Seconds: 900},
		},
		ItemConcurrency: 1,
	}
	for _, test := range []struct {
		name                string
		problems            int
		seconds             int64
		confirmBeforeAnchor bool
	}{
		{name: "one", problems: 1, seconds: 90},
		{name: "eight", problems: 8, seconds: 180},
		{name: "sixteen", problems: 16, seconds: 600},
		{name: "thirty_two", problems: 32, seconds: 900},
		{name: "sixteen_confirmed_before_anchor", problems: 16, seconds: 600, confirmBeforeAnchor: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, _ := newPipeline(t,
				fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
				fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
			)
			d.Now = func() int64 { return now }
			d.GradingBudgetSnapshot = budget
			ctx := context.Background()
			view, created, err := d.CreateGradingJob(ctx, "mingming", "session", CreateGradingJobInput{
				SubmissionID:                "budget-parent-" + test.name,
				SourceKind:                  "image_task",
				SourceKey:                   "dispatch-budget-parent-" + test.name,
				ModelSnapshot:               orchestratorSnapshot(),
				ParentAutomaticAttemptID:    "dispatch-budget-parent-" + test.name + ":7000",
				ParentAutomaticDeadlineAt:   now + k12.ImageTaskAutomaticBudgetSeconds,
				MaterializesProblemAttempts: true,
			})
			if err != nil || !created {
				t.Fatalf("create: created=%v err=%v", created, err)
			}
			for _, stage := range []string{
				k12.GradingStageNormalizing,
				k12.GradingStageRecognizing,
				k12.GradingStageAwaitingConfirmation,
			} {
				view, err = d.AdvanceGradingStage(ctx, "mingming", view.Record.RecordID,
					AdvanceGradingInput{Outcome: GradingOutcomeOK, ArtifactDigest: stage})
				if err != nil || view.Record.Status != stage {
					t.Fatalf("advance to %s: got=%s err=%v", stage, view.Record.Status, err)
				}
			}
			questions := make([]RecognizedQuestion, test.problems)
			for i := range questions {
				questions[i] = RecognizedQuestion{
					Question: fmt.Sprintf("q-%d", i+1), Subject: "数学",
					AnswerState: AnswerStatePresent, StudentAnswer: "1",
				}
			}
			questions, err = NormalizeRecognizedProblems(view.Fields.SubmissionID, questions)
			if err != nil {
				t.Fatal(err)
			}
			for i := range questions {
				questions[i].ConfirmedVersion = 1
			}
			questions = FreezeRecognizedQuestionInputDigests(questions, "五年级下")
			typed, err := RecognizedQuestionsProblemAttemptSnapshot(
				"mingming", view.Fields.SubmissionID, questions, now,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Records.PutProblemAttemptSnapshot(ctx, typed); err != nil {
				t.Fatal(err)
			}
			anchor := func() {
				t.Helper()
				var anchorErr error
				view, anchorErr = d.AdvanceGradingStage(ctx, "mingming", view.Record.RecordID,
					AdvanceGradingInput{Outcome: GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated, ArtifactDigest: "located"},
				)
				if anchorErr != nil {
					t.Fatal(anchorErr)
				}
			}
			confirm := func() {
				t.Helper()
				var confirmErr error
				view, confirmErr = d.ConfirmGradingJob(ctx, "mingming", view.Record.RecordID, nil)
				if confirmErr != nil {
					t.Fatal(confirmErr)
				}
			}
			if test.confirmBeforeAnchor {
				confirm()
				if view.Record.Status != k12.GradingStageAwaitingConfirmation {
					t.Fatalf("confirmation before anchor stage=%s, want awaiting_confirmation", view.Record.Status)
				}
				anchor()
			} else {
				anchor()
				confirm()
			}
			if view.Record.Status != k12.GradingStageAssessing {
				t.Fatalf("confirmation/anchor join stage=%s", view.Record.Status)
			}
			view, err = d.GetGradingJob(ctx, "mingming", view.Record.RecordID)
			if err != nil {
				t.Fatalf("reload durable job: %v", err)
			}
			wantDeadline := now + test.seconds
			if view.Fields.ParentAutomaticRemainingSeconds != test.seconds ||
				view.Fields.ParentAutomaticDeadlineAt != wantDeadline ||
				view.Fields.Deadline != wantDeadline {
				t.Fatalf("assessing window=%d/%d deadline=%d, want remaining/deadline=%d/%d",
					view.Fields.ParentAutomaticRemainingSeconds,
					view.Fields.ParentAutomaticDeadlineAt,
					view.Fields.Deadline,
					test.seconds,
					wantDeadline,
				)
			}
			stageCtx, cancelStage := gradingStageContext(ctx, view.Fields.Deadline)
			callCtx, cancelCall := gradingIndependentCallContext(stageCtx, int((20*time.Minute)/time.Millisecond))
			gotDeadline, ok := callCtx.Deadline()
			cancelCall()
			cancelStage()
			if !ok || !gotDeadline.Equal(time.Unix(wantDeadline, 0)) {
				t.Fatalf("physical call deadline=%v ok=%v, want=%v", gotDeadline, ok, time.Unix(wantDeadline, 0))
			}
		})
	}
}

func TestGradingRecoveryExpiredParentBeforeSendPersistsInteractiveDeadlineWithoutProvider(t *testing.T) {
	now := int64(4_000)
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2",
	}}}
	d := recoveryDeps(t, rec, nil, &photoAnnotatorFake{})
	d.Now = func() int64 { return now }
	dir := t.TempDir()
	o1 := newRecoverableOrchestrator(t, d, dir)
	view, created, err := o1.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo:                     orchestratorPhotoRequest(),
		SourceKind:                "image_task",
		SourceKey:                 "dispatch-expired-before-send:1",
		BudgetSnapshot:            gradingParentFrozenBudget(),
		ParentAutomaticAttemptID:  "dispatch-expired-before-send:1",
		ParentAutomaticDeadlineAt: now + 5,
	})
	if err != nil || !created {
		t.Fatalf("StartPhotoGradingJob: created=%v err=%v", created, err)
	}
	now += 6
	o2 := newRecoverableOrchestrator(t, d, dir)
	if _, err = o2.RecoverGradingJobs(context.Background(), []string{"mingming"}); err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	idleCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = o2.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
	got, err := d.GetGradingJob(context.Background(), "mingming", view.Record.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Record.Status != k12.GradingStageFailedRetryable ||
		got.Fields.FailureKind != "interactive_deadline_exceeded" ||
		!got.Fields.Retryable {
		t.Fatalf("expired pre-send recovery state=%s fields=%+v", got.Record.Status, got.Fields)
	}
	if rec.calls != 0 {
		t.Fatalf("expired pre-send recovery called provider %d times", rec.calls)
	}
}

func TestGradingExpiredParentPreservesSentInvocationAsOutcomeUnknown(t *testing.T) {
	now := int64(5_000)
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2",
	}}}
	d := recoveryDeps(t, rec, nil, &photoAnnotatorFake{})
	d.Now = func() int64 { return now }
	o := newRecoverableOrchestrator(t, d, t.TempDir())
	view, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo:                     orchestratorPhotoRequest(),
		SourceKind:                "image_task",
		SourceKey:                 "dispatch-sent-before-expiry:1",
		BudgetSnapshot:            gradingParentFrozenBudget(),
		ParentAutomaticAttemptID:  "dispatch-sent-before-expiry:1",
		ParentAutomaticDeadlineAt: now + 5,
	})
	if err != nil || !created {
		t.Fatalf("StartPhotoGradingJob: created=%v err=%v", created, err)
	}
	jobID := view.Record.RecordID
	if view, err = d.AdvanceGradingStage(context.Background(), "mingming", jobID,
		AdvanceGradingInput{Outcome: GradingOutcomeOK}); err != nil {
		t.Fatal(err)
	}
	if view, err = d.AdvanceGradingStage(context.Background(), "mingming", jobID,
		AdvanceGradingInput{Outcome: GradingOutcomeOK}); err != nil {
		t.Fatal(err)
	}
	run := o.lookup(jobID)
	requestDigest := recognizingInvocationDigest(
		run.req.Image,
		view.Fields.ModelSnapshot,
		k12.ModelRequestPolicySnapshot{},
	)
	invocation, _, err := d.Records.PrepareModelInvocation(context.Background(), k12.ModelInvocation{
		InvocationID: "modelinv-sent-parent-expiry", AgentName: "mingming",
		JobID: jobID, Stage: k12.GradingStageRecognizing, RequestDigest: requestDigest,
		RouteSnapshot: view.Fields.ModelSnapshot, Attempt: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Records.MarkModelInvocationSent(context.Background(), "mingming",
		invocation.InvocationID, ""); err != nil {
		t.Fatal(err)
	}
	now += 6
	got, err := o.RunGradingJob(context.Background(), jobID)
	if err == nil {
		t.Fatal("sent invocation without provider query must remain outcome_unknown")
	}
	if got.Record.Status != k12.GradingStageOutcomeUnknown ||
		got.Fields.FailureKind == "interactive_deadline_exceeded" {
		t.Fatalf("sent invocation overwritten by parent expiry: stage=%s fields=%+v",
			got.Record.Status, got.Fields)
	}
	if rec.calls != 0 {
		t.Fatalf("sent invocation was resent %d times", rec.calls)
	}
}

func TestRetryGradingJobWithParentAutomaticWindowRequeuesOnlyInteractiveDeadline(t *testing.T) {
	now := int64(6_000)
	d := recoveryDeps(t, &countingRecognizer{}, nil, &photoAnnotatorFake{})
	d.Now = func() int64 { return now }
	d.GradingBudgetSnapshot = k12.GradingBudgetSnapshot{}
	view, created, err := d.CreateGradingJob(context.Background(), "mingming", "session-parent-retry", CreateGradingJobInput{
		SubmissionID:              "sub-parent-retry",
		SourceKind:                "image_task",
		SourceKey:                 "dispatch-parent-retry:1",
		ModelSnapshot:             k12.GradingModelSnapshot{Provider: "openrouter", Model: "test-vlm"},
		ParentAutomaticAttemptID:  "dispatch-parent-retry:1",
		ParentAutomaticDeadlineAt: now + 5,
	})
	if err != nil || !created {
		t.Fatalf("CreateGradingJob: created=%v err=%v", created, err)
	}
	jobID := view.Record.RecordID
	if view, err = d.AdvanceGradingStage(context.Background(), "mingming", jobID,
		AdvanceGradingInput{Outcome: GradingOutcomeOK}); err != nil {
		t.Fatal(err)
	}
	now += 6
	if view, err = d.AdvanceGradingStage(context.Background(), "mingming", jobID,
		AdvanceGradingInput{
			Outcome: GradingOutcomeFailed, FailureKind: "interactive_deadline_exceeded", Retryable: true,
		}); err != nil {
		t.Fatal(err)
	}
	if view.Record.Status != k12.GradingStageFailedRetryable {
		t.Fatalf("seed stage=%s, want failed_retryable", view.Record.Status)
	}

	const newAttempt = "dispatch-parent-retry:2"
	view, err = d.RetryGradingJobWithParentAutomaticWindow(
		context.Background(), "mingming", jobID, newAttempt, now+300,
	)
	if err != nil {
		t.Fatalf("RetryGradingJobWithParentAutomaticWindow: %v", err)
	}
	if view.Record.Status != k12.GradingStageQueued ||
		view.Fields.ParentAutomaticAttemptID != newAttempt ||
		view.Fields.ParentAutomaticDeadlineAt != now+300 ||
		view.Fields.ParentAutomaticRemainingSeconds != 300 ||
		view.Fields.Deadline != now+60 {
		t.Fatalf("fresh parent retry facts incorrect: stage=%s fields=%+v",
			view.Record.Status, view.Fields)
	}
}
