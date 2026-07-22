package usecase

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type recognizingDeadlineProbe struct {
	seen        chan recognizingDeadlineObservation
	proceed     chan struct{}
	done        chan struct{}
	releaseOnce sync.Once
}

type recognizingDeadlineObservation struct {
	deadline time.Time
	ok       bool
}

func (r *recognizingDeadlineProbe) Recognize(ctx context.Context, _ []byte) ([]RecognizedQuestion, error) {
	deadline, ok := ctx.Deadline()
	r.seen <- recognizingDeadlineObservation{deadline: deadline, ok: ok}
	<-r.proceed
	<-ctx.Done()
	close(r.done)
	return nil, ctx.Err()
}

func (r *recognizingDeadlineProbe) release() {
	r.releaseOnce.Do(func() { close(r.proceed) })
}

type recognizingDeadlineCapture struct {
	seen chan recognizingDeadlineObservation
}

func (r *recognizingDeadlineCapture) Recognize(ctx context.Context, _ []byte) ([]RecognizedQuestion, error) {
	deadline, ok := ctx.Deadline()
	r.seen <- recognizingDeadlineObservation{deadline: deadline, ok: ok}
	return []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}, nil
}

type assessingDeadlineProbe struct {
	seen        chan recognizingDeadlineObservation
	proceed     chan struct{}
	releaseOnce sync.Once
}

func (g *assessingDeadlineProbe) Grade(ctx context.Context, _, _, _ string) (GradeOutcome, error) {
	deadline, ok := ctx.Deadline()
	g.seen <- recognizingDeadlineObservation{deadline: deadline, ok: ok}
	select {
	case <-g.proceed:
		return GradeOutcome{Verdict: VerdictAgree}, nil
	case <-ctx.Done():
		return GradeOutcome{}, ctx.Err()
	}
}

func (g *assessingDeadlineProbe) release() {
	g.releaseOnce.Do(func() { close(g.proceed) })
}

// The persisted recognizing deadline is the provider-call budget, not merely
// audit metadata. Expiry must cancel the in-flight call, durably park the sent
// invocation as outcome_unknown, and let the async worker drain.
func TestGradingOrchestratorRecognizeUsesPersistedDeadlineAndDrainsOnExpiry(t *testing.T) {
	recognizer := &recognizingDeadlineProbe{
		seen:    make(chan recognizingDeadlineObservation, 1),
		proceed: make(chan struct{}),
		done:    make(chan struct{}),
	}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	t.Cleanup(recognizer.release)

	// A recognizing stage has a 120-second budget. Offset the application clock
	// so the persisted absolute deadline expires within the next wall-clock second.
	fakeNow := time.Now().Unix() - k12.GradingStageBudgetSeconds(k12.GradingStageRecognizing) + 1
	o.deps.Now = func() int64 { return fakeNow }
	jobID := startOrchestratorJob(t, o, "msg-recognize-stage-deadline").Record.RecordID
	if accepted := o.StartAsync(jobID); !accepted {
		t.Fatal("running orchestrator must accept asynchronous grading")
	}

	var observed recognizingDeadlineObservation
	select {
	case observed = <-recognizer.seen:
	case <-time.After(time.Second):
		t.Fatal("recognizer provider call did not start")
	}
	if !observed.ok {
		t.Fatal("recognizer provider context must carry the persisted stage deadline")
	}

	running, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := observed.deadline.Unix(), running.Fields.Deadline; got != want {
		t.Fatalf("recognizer deadline=%d, want persisted job deadline=%d", got, want)
	}
	recognizer.release()

	idleCtx, cancelIdle := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelIdle()
	if err := o.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("deadline-expired recognizer left an async worker running: %v", err)
	}
	select {
	case <-recognizer.done:
	default:
		t.Fatal("async worker drained before the canceled recognizer returned")
	}

	parked, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Record.Status != k12.GradingStageOutcomeUnknown || parked.Fields.Retryable {
		t.Fatalf("expired sent recognition must park as non-retryable outcome_unknown: stage=%s fields=%+v",
			parked.Record.Status, parked.Fields)
	}
	invocations, err := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if err != nil || len(invocations) != 1 {
		t.Fatalf("list expired recognition invocation: count=%d err=%v", len(invocations), err)
	}
	if invocations[0].Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf("expired recognition invocation status=%s, want outcome_unknown", invocations[0].Status)
	}
}

func TestGradingOrchestratorRecognizePreservesEarlierParentDeadline(t *testing.T) {
	recognizer := &recognizingDeadlineCapture{seen: make(chan recognizingDeadlineObservation, 1)}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	o.deps.Now = func() int64 { return time.Now().Unix() }
	jobID := startOrchestratorJob(t, o, "msg-recognize-parent-deadline").Record.RecordID

	parentDeadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), parentDeadline)
	defer cancel()
	if _, err := o.RunGradingJob(ctx, jobID); err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	observed := <-recognizer.seen
	if !observed.ok || !observed.deadline.Equal(parentDeadline) {
		t.Fatalf("recognizer deadline=%v ok=%v, want earlier parent deadline=%v",
			observed.deadline, observed.ok, parentDeadline)
	}
}

func TestGradingOrchestratorAssessUsesPersistedDeadline(t *testing.T) {
	grader := &assessingDeadlineProbe{
		seen: make(chan recognizingDeadlineObservation, 1), proceed: make(chan struct{}),
	}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	t.Cleanup(grader.release)
	o.deps.Grader = grader
	o.deps.Now = func() int64 { return time.Now().Unix() }
	jobID := startOrchestratorJob(t, o, "msg-assess-stage-deadline").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob recognizing: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})
	if _, ok, err := o.ConfirmPhotoGradingJob(context.Background(), jobID, ConfirmPhotoGradingInput{}); err != nil || !ok {
		t.Fatalf("ConfirmPhotoGradingJob: ok=%v err=%v", ok, err)
	}

	var observed recognizingDeadlineObservation
	select {
	case observed = <-grader.seen:
	case <-time.After(time.Second):
		t.Fatal("assessing grader call did not start")
	}
	running, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if running.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("blocked grader stage=%s, want assessing", running.Record.Status)
	}
	if !observed.ok || observed.deadline.Unix() != running.Fields.Deadline {
		t.Fatalf("grader deadline=%v ok=%v, want persisted assessing deadline=%d",
			observed.deadline, observed.ok, running.Fields.Deadline)
	}
	grader.release()
	idleCtx, cancelIdle := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelIdle()
	if err := o.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("assessing worker did not drain: %v", err)
	}
}
