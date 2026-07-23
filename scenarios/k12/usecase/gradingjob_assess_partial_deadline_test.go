package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// partialAssessDeadlineGrader reproduces the real dense-sheet failure shape:
// one worker finishes and another external call is still in flight when the
// persisted assessing deadline expires.
type partialAssessDeadlineGrader struct {
	fastDone    chan struct{}
	slowStarted chan struct{}
}

type cancelBlockingAssessGrader struct {
	started chan struct{}
	done    chan struct{}
}

type stringOnlyDeadlineAssessGrader struct {
	started chan struct{}
}

type partialStringOnlyDeadlineAssessGrader struct {
	fastDone    chan struct{}
	slowStarted chan struct{}
}

type successAfterDeadlineAssessGrader struct {
	started chan struct{}
}

func (g *cancelBlockingAssessGrader) Grade(ctx context.Context, _, _, _ string) (GradeOutcome, error) {
	close(g.started)
	<-ctx.Done()
	close(g.done)
	return GradeOutcome{}, ctx.Err()
}

func (g *stringOnlyDeadlineAssessGrader) Grade(ctx context.Context, _, _, _ string) (GradeOutcome, error) {
	close(g.started)
	<-ctx.Done()
	return GradeOutcome{}, errors.New("模型响应超时，请稍后重试或切换到更快的模型。")
}

func (g *partialStringOnlyDeadlineAssessGrader) Grade(ctx context.Context, problem, _, _ string) (GradeOutcome, error) {
	if problem == "fast" {
		close(g.fastDone)
		return GradeOutcome{Verdict: VerdictAgree}, nil
	}
	close(g.slowStarted)
	<-ctx.Done()
	return GradeOutcome{}, errors.New("模型响应超时，请稍后重试或切换到更快的模型。")
}

func (g *successAfterDeadlineAssessGrader) Grade(ctx context.Context, _, _, _ string) (GradeOutcome, error) {
	close(g.started)
	<-ctx.Done()
	return GradeOutcome{Verdict: VerdictAgree}, nil
}

func (g *partialAssessDeadlineGrader) Grade(ctx context.Context, problem, _, _ string) (GradeOutcome, error) {
	if problem == "fast" {
		close(g.fastDone)
		return GradeOutcome{Verdict: VerdictAgree}, nil
	}
	close(g.slowStarted)
	<-ctx.Done()
	return GradeOutcome{}, ctx.Err()
}

func TestGradingOrchestratorAssessDeadlineWithPartialItemsDoesNotComplete(t *testing.T) {
	grader := &partialAssessDeadlineGrader{
		fastDone:    make(chan struct{}),
		slowStarted: make(chan struct{}),
	}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{
		{Question: "fast", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent},
		{Question: "slow", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent},
	}}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	o.deps.Grader = grader
	o.deps.Now = func() int64 { return time.Now().Unix() }

	jobID := startOrchestratorJob(t, o, "msg-assess-partial-deadline").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob recognizing: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})

	// ConfirmGradingJob derives the assessing deadline from Deps.Now. Move only
	// that application clock so the real wall-clock deadline expires within one
	// second while both per-item workers are active.
	o.deps.Now = func() int64 {
		return time.Now().Unix() - k12.GradingStageBudgetSeconds(k12.GradingStageAssessing) + 1
	}
	view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("partial assessing deadline err=%v, want context deadline exceeded", err)
	}
	select {
	case <-grader.fastDone:
	default:
		t.Fatal("regression setup did not complete the fast item before stage expiry")
	}
	select {
	case <-grader.slowStarted:
	default:
		t.Fatal("regression setup did not start the slow item before stage expiry")
	}
	if view.Record.Status != k12.GradingStageOutcomeUnknown || view.Fields.Retryable {
		t.Fatalf("partial deadline must not complete or expose ordinary retry: stage=%s fields=%+v",
			view.Record.Status, view.Fields)
	}
	if _, ok := o.PhotoResult(jobID); ok {
		t.Fatal("outcome_unknown assessing stage must not publish a partial result as final")
	}
	invocations, listErr := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if listErr != nil {
		t.Fatalf("list model invocations: %v", listErr)
	}
	var assessInvocation *k12.ModelInvocation
	for i := range invocations {
		if invocations[i].Stage == k12.GradingStageAssessing {
			assessInvocation = &invocations[i]
			break
		}
	}
	if assessInvocation == nil || assessInvocation.Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf("assessing invocation must remain reconcilable, got %#v", assessInvocation)
	}
}

func TestGradingOrchestratorAssessDeadlineWithStringOnlyAdapterIsOutcomeUnknown(t *testing.T) {
	grader := &stringOnlyDeadlineAssessGrader{started: make(chan struct{})}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	o.deps.Grader = grader
	o.deps.Now = func() int64 { return time.Now().Unix() }

	jobID := startOrchestratorJob(t, o, "msg-assess-string-only-deadline").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob recognizing: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})

	o.deps.Now = func() int64 {
		return time.Now().Unix() - k12.GradingStageBudgetSeconds(k12.GradingStageAssessing) + 1
	}
	view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("regression requires an opaque adapter error, got %T %v", err, err)
	}
	select {
	case <-grader.started:
	default:
		t.Fatal("regression setup did not enter the adapter call")
	}
	if view.Record.Status != k12.GradingStageOutcomeUnknown || view.Fields.Retryable {
		t.Fatalf("expired provider context must dominate opaque adapter text: stage=%s fields=%+v err=%v",
			view.Record.Status, view.Fields, err)
	}
	invocations, listErr := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if listErr != nil {
		t.Fatalf("list model invocations: %v", listErr)
	}
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageAssessing && invocation.Status != k12.ModelInvocationOutcomeUnknown {
			t.Fatalf("expired assessing invocation status=%s, want outcome_unknown", invocation.Status)
		}
	}
}

func TestGradingOrchestratorAssessPartialSuccessWithStringOnlyDeadlineIsOutcomeUnknown(t *testing.T) {
	grader := &partialStringOnlyDeadlineAssessGrader{
		fastDone:    make(chan struct{}),
		slowStarted: make(chan struct{}),
	}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{
		{Question: "fast", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent},
		{Question: "slow", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent},
	}}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	o.deps.Grader = grader
	o.deps.Now = func() int64 { return time.Now().Unix() }

	jobID := startOrchestratorJob(t, o, "msg-assess-partial-string-only-deadline").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob recognizing: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})

	o.deps.Now = func() int64 {
		return time.Now().Unix() - k12.GradingStageBudgetSeconds(k12.GradingStageAssessing) + 1
	}
	view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired provider context err=%T %v, want context deadline exceeded", err, err)
	}
	select {
	case <-grader.fastDone:
	default:
		t.Fatal("regression setup did not complete the fast item")
	}
	select {
	case <-grader.slowStarted:
	default:
		t.Fatal("regression setup did not enter the slow adapter call")
	}
	if view.Record.Status != k12.GradingStageOutcomeUnknown || view.Fields.Retryable {
		t.Fatalf("partial success cannot hide an expired provider context: stage=%s fields=%+v err=%v",
			view.Record.Status, view.Fields, err)
	}
	if _, ok := o.PhotoResult(jobID); ok {
		t.Fatal("outcome_unknown assessing stage must not publish the partial result")
	}
	invocations, listErr := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if listErr != nil {
		t.Fatalf("list model invocations: %v", listErr)
	}
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageAssessing && invocation.Status != k12.ModelInvocationOutcomeUnknown {
			t.Fatalf("expired assessing invocation status=%s, want outcome_unknown", invocation.Status)
		}
	}
}

func TestGradingOrchestratorAssessCompleteResultAtDeadlineDoesNotBecomeUnknown(t *testing.T) {
	grader := &successAfterDeadlineAssessGrader{started: make(chan struct{})}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	o.deps.Grader = grader
	o.deps.Now = func() int64 { return time.Now().Unix() }

	jobID := startOrchestratorJob(t, o, "msg-assess-complete-at-deadline").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob recognizing: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})

	o.deps.Now = func() int64 {
		return time.Now().Unix() - k12.GradingStageBudgetSeconds(k12.GradingStageAssessing) + 1
	}
	view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil {
		t.Fatalf("complete result returned after context expiry must remain usable: %v", err)
	}
	select {
	case <-grader.started:
	default:
		t.Fatal("regression setup did not enter the adapter call")
	}
	if view.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("complete result must not become false unknown: stage=%s fields=%+v", view.Record.Status, view.Fields)
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || len(result.Items) != 1 || result.Items[0].Status != PhotoCorrect {
		t.Fatalf("completed result missing: ok=%v result=%#v", ok, result)
	}
	invocations, listErr := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if listErr != nil {
		t.Fatalf("list model invocations: %v", listErr)
	}
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageAssessing && invocation.Status != k12.ModelInvocationSucceeded {
			t.Fatalf("complete assessing invocation status=%s, want succeeded", invocation.Status)
		}
	}
}

func TestGradingOrchestratorFrozenAssessCompleteResultAtDeadlinePersistsSuccess(t *testing.T) {
	grader := &successAfterDeadlineAssessGrader{started: make(chan struct{})}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	o.deps.Grader = grader
	o.deps.Now = func() int64 { return time.Now().Unix() }

	jobID := startOrchestratorJob(t, o, "msg-frozen-assess-complete-at-deadline").Record.RecordID
	freezeItemResumeBudget(t, o, jobID)
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob recognizing: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})

	o.deps.Now = func() int64 {
		return time.Now().Unix() - 90 + 1
	}
	view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil {
		t.Fatalf("complete frozen item returned after context expiry must remain usable: %v", err)
	}
	select {
	case <-grader.started:
	default:
		t.Fatal("regression setup did not enter the adapter call")
	}
	if view.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("complete frozen item must not become false unknown: stage=%s fields=%+v", view.Record.Status, view.Fields)
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || len(result.Items) != 1 || result.Items[0].Status != PhotoCorrect {
		t.Fatalf("completed frozen result missing: ok=%v result=%#v", ok, result)
	}
	invocations, listErr := o.deps.Records.ListGradingItemInvocations(context.Background(), "mingming", jobID)
	if listErr != nil {
		t.Fatalf("list item invocations: %v", listErr)
	}
	if len(invocations) != 2 {
		t.Fatalf("solve and grade invocations=%d, want 2", len(invocations))
	}
	for _, invocation := range invocations {
		if invocation.Status != k12.ModelInvocationSucceeded {
			t.Fatalf("complete item invocation status=%s, want succeeded", invocation.Status)
		}
	}
}

func TestGradingOrchestratorAssessTypedUnknownItemDoesNotComplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "deadline exceeded", err: context.DeadlineExceeded},
		{name: "cancelled", err: context.Canceled},
		{name: "requires reconciliation", err: ErrModelInvocationRequiresReconciliation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
				Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
			}}}
			o := newParallelAnchorOrchestrator(t, recognizer, nil)
			o.deps.Grader = photoGradeErrorGrader{err: tc.err}
			o.deps.Now = func() int64 { return time.Now().Unix() }

			jobID := startOrchestratorJob(t, o, "msg-assess-typed-unknown-"+tc.name).Record.RecordID
			if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
				t.Fatalf("RunGradingJob recognizing: %v", err)
			}
			waitGradingView(t, o, jobID, func(v GradingJobView) bool {
				return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
					v.Fields.AnchorState == k12.GradingAnchorDegraded
			})

			view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
			if !errors.Is(err, tc.err) {
				t.Fatalf("assessing err=%v, want typed %v", err, tc.err)
			}
			if view.Record.Status != k12.GradingStageOutcomeUnknown || view.Fields.Retryable {
				t.Fatalf("typed unknown item must not complete or expose ordinary retry: stage=%s fields=%+v",
					view.Record.Status, view.Fields)
			}
			if _, ok := o.PhotoResult(jobID); ok {
				t.Fatal("outcome_unknown assessing stage must not publish a partial result as final")
			}
		})
	}
}

func TestGradingOrchestratorAssessOrdinaryFailureWithZeroCompletedItemsBecomesFailed(t *testing.T) {
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	o.deps.Grader = photoGradeErrorGrader{err: errors.New("provider returned 503")}

	jobID := startOrchestratorJob(t, o, "msg-assess-ordinary-item-failure").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob recognizing: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})

	view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err == nil || view.Record.Status != k12.GradingStageFailedRetryable || !view.Fields.Retryable {
		t.Fatalf("zero completed items must become retryable failed: stage=%s fields=%+v err=%v",
			view.Record.Status, view.Fields, err)
	}
	if result, ok := o.PhotoResult(jobID); ok {
		t.Fatalf("failed aggregate must not publish a final photo result: %#v", result)
	}
}

func TestCancelPhotoGradingJobDuringAssessNeverMarksInvocationSucceededOrJobCompleted(t *testing.T) {
	grader := &cancelBlockingAssessGrader{started: make(chan struct{}), done: make(chan struct{})}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	o.deps.Grader = grader

	jobID := startOrchestratorJob(t, o, "msg-cancel-during-assess").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob recognizing: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})
	if _, handled, err := o.ConfirmPhotoGradingJob(context.Background(), jobID, ConfirmPhotoGradingInput{}); err != nil || !handled {
		t.Fatalf("ConfirmPhotoGradingJob: handled=%v err=%v", handled, err)
	}
	select {
	case <-grader.started:
	case <-time.After(time.Second):
		t.Fatal("assessing grader did not start")
	}

	cancelled, handled, err := o.CancelPhotoGradingJob(context.Background(), "mingming", jobID)
	if err != nil || !handled || cancelled.Record.Status != k12.GradingStageCancelled {
		t.Fatalf("CancelPhotoGradingJob: handled=%v stage=%s err=%v", handled, cancelled.Record.Status, err)
	}
	select {
	case <-grader.done:
	case <-time.After(time.Second):
		t.Fatal("assessing cancellation did not reach the grader")
	}
	idleCtx, cancelIdle := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelIdle()
	if err := o.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("cancelled assessing worker did not drain: %v", err)
	}
	final, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil || final.Record.Status != k12.GradingStageCancelled {
		t.Fatalf("cancelled Job must remain durable cancelled, stage=%s err=%v", final.Record.Status, err)
	}
	if _, ok := o.PhotoResult(jobID); ok {
		t.Fatal("cancelled assessing Job must not publish a partial result")
	}
	invocations, err := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatalf("list model invocations: %v", err)
	}
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageAssessing && invocation.Status == k12.ModelInvocationSucceeded {
			t.Fatalf("cancelled assessing invocation was falsely marked succeeded: %#v", invocation)
		}
	}
}
