package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type photoGradeErrorGrader struct {
	err error
}

func (g photoGradeErrorGrader) Grade(context.Context, string, string, string) (GradeOutcome, error) {
	return GradeOutcome{}, g.err
}

type photoGradeSelectiveErrorGrader struct {
	errProblem string
	err        error
}

func (g photoGradeSelectiveErrorGrader) Grade(_ context.Context, problem, _, _ string) (GradeOutcome, error) {
	if problem == g.errProblem {
		return GradeOutcome{}, g.err
	}
	return GradeOutcome{Verdict: VerdictAgree}, nil
}

type photoGradeStopAfterUnknownGrader struct {
	mu                 sync.Mutex
	calls              []string
	inFlightStarted    chan struct{}
	allowUnknownReturn chan struct{}
	releaseInFlight    chan struct{}
	unexpectedStarted  chan struct{}
	unexpectedOnce     sync.Once
}

func (g *photoGradeStopAfterUnknownGrader) Grade(_ context.Context, problem, _, _ string) (GradeOutcome, error) {
	g.mu.Lock()
	g.calls = append(g.calls, problem)
	g.mu.Unlock()

	switch problem {
	case "unknown":
		<-g.inFlightStarted
		<-g.allowUnknownReturn
		return GradeOutcome{}, context.DeadlineExceeded
	case "in-flight":
		close(g.inFlightStarted)
		<-g.releaseInFlight
		return GradeOutcome{Verdict: VerdictAgree}, nil
	default:
		g.unexpectedOnce.Do(func() { close(g.unexpectedStarted) })
		return GradeOutcome{Verdict: VerdictAgree}, nil
	}
}

func (g *photoGradeStopAfterUnknownGrader) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

func TestGradeHomeworkPhoto_InvocationUnknownItemReturnsTypedAggregateError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "deadline exceeded", err: context.DeadlineExceeded},
		{name: "cancelled", err: context.Canceled},
		{name: "requires reconciliation", err: ErrModelInvocationRequiresReconciliation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newPipeline(t,
				fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
				photoGradeErrorGrader{err: tc.err}, nil,
			)
			d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
				Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
			}}}

			result, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
				AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("aggregate err=%v, want typed %v", err, tc.err)
			}
			if len(result.Items) != 1 || result.Items[0].Status != PhotoFailed {
				t.Fatalf("partial diagnostic result must retain the failed item: %#v", result.Items)
			}
		})
	}
}

func TestGradeHomeworkPhoto_StopsDispatchingAfterFirstUnknownAndWaitsForInFlight(t *testing.T) {
	grader := &photoGradeStopAfterUnknownGrader{
		inFlightStarted:    make(chan struct{}),
		allowUnknownReturn: make(chan struct{}),
		releaseInFlight:    make(chan struct{}),
		unexpectedStarted:  make(chan struct{}),
	}
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		grader, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "unknown", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent},
		{Question: "in-flight", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent},
		{Question: "must-not-start-1", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent},
		{Question: "must-not-start-2", Subject: "数学", StudentAnswer: "4", AnswerState: AnswerStatePresent},
	}}

	type gradeResponse struct {
		result PhotoGradeResult
		err    error
	}
	done := make(chan gradeResponse, 1)
	go func() {
		result, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
			AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
		})
		done <- gradeResponse{result: result, err: err}
	}()

	select {
	case <-grader.inFlightStarted:
	case <-time.After(time.Second):
		t.Fatal("second worker did not enter its in-flight call")
	}
	close(grader.allowUnknownReturn)
	select {
	case <-grader.unexpectedStarted:
		// The pre-fix worker pool immediately dispatches a third item here. Release
		// the already-started peer so the failing regression can drain cleanly.
		close(grader.releaseInFlight)
	case <-time.After(100 * time.Millisecond):
		// No new call was dispatched after the unknown outcome. The already-started
		// peer is still allowed to converge before GradeHomeworkPhoto returns.
		close(grader.releaseInFlight)
	}

	var response gradeResponse
	select {
	case response = <-done:
	case <-time.After(time.Second):
		t.Fatal("GradeHomeworkPhoto did not wait for the in-flight call to converge")
	}
	if !errors.Is(response.err, context.DeadlineExceeded) {
		t.Fatalf("aggregate err=%v, want deadline exceeded", response.err)
	}
	if got := grader.callCount(); got != 2 {
		t.Fatalf("grader calls=%d, want only the two already-started calls", got)
	}
	if len(response.result.Items) != 4 || response.result.Items[0].Status != PhotoFailed || response.result.Items[1].Status != PhotoCorrect {
		t.Fatalf("diagnostic result lost converged in-flight items: %#v", response.result.Items)
	}
}

func TestGradeHomeworkPhoto_OrdinaryFailureWithZeroCompletedItemsReturnsAggregateError(t *testing.T) {
	ordinaryErr := errors.New("provider returned 503")
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		photoGradeErrorGrader{err: ordinaryErr}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}}

	result, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
	})
	if !errors.Is(err, ordinaryErr) {
		t.Fatalf("zero completed items err=%v, want original provider error", err)
	}
	if len(result.Items) != 1 || result.Items[0].Status != PhotoFailed {
		t.Fatalf("ordinary failure must remain an explicit failed item: %#v", result.Items)
	}
	if result.Markdown == "" {
		t.Fatal("failed aggregate must retain diagnostic markdown")
	}
}

func TestGradeHomeworkPhoto_UnansweredItemDoesNotHideZeroSuccessfulAssessments(t *testing.T) {
	ordinaryErr := errors.New("provider returned 503")
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		photoGradeErrorGrader{err: ordinaryErr}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent},
		{Question: "2+2=", Subject: "数学", AnswerState: AnswerStateBlank},
	}}

	result, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
	})
	if !errors.Is(err, ordinaryErr) {
		t.Fatalf("unanswered metadata must not turn zero successful assessments into success: %v", err)
	}
	if len(result.Items) != 2 || result.Items[0].Status != PhotoFailed || result.Items[1].Status != PhotoUnanswered {
		t.Fatalf("diagnostic statuses=%#v, want failed then unanswered", result.Items)
	}
}

func TestGradeHomeworkPhoto_OrdinaryFailureAfterOneSuccessfulItemReturnsAggregateError(t *testing.T) {
	ordinaryErr := errors.New("provider returned 503")
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		photoGradeSelectiveErrorGrader{errProblem: "fails", err: ordinaryErr}, nil,
	)
	d.Recognizer = photoRecognizerFake{questions: []RecognizedQuestion{
		{Question: "succeeds", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent},
		{Question: "fails", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent},
	}}

	result, err := d.GradeHomeworkPhoto(context.Background(), PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", Image: []byte("jpeg"),
	})
	if !errors.Is(err, ordinaryErr) {
		t.Fatalf("one failed assessment must keep the page incomplete: err=%v", err)
	}
	if len(result.Items) != 2 || result.Items[0].Status != PhotoCorrect || result.Items[1].Status != PhotoFailed {
		t.Fatalf("partial statuses=%#v, want correct then failed", result.Items)
	}
}
