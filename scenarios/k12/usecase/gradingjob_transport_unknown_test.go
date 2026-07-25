package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// REG-P0: once the durable invocation ledger is marked sent, an untyped EOF
// cannot prove that the provider rejected the request. Whole-page recognition
// must park for reconciliation instead of exposing an ordinary retry.
func TestGradingRecognizeAmbiguousTransportAfterSendParksOutcomeUnknown(t *testing.T) {
	transportErr := fmt.Errorf("vision transport lost after send: %w", io.ErrUnexpectedEOF)
	recognizer := &providerErrorRecognizer{err: transportErr}
	o := newOrchestrator(t, recognizer, nil, nil)
	started := startOrchestratorJob(t, o, "recognize-transport-unknown")
	jobID := started.Record.RecordID

	unknown, err := o.RunGradingJob(context.Background(), jobID)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("recognition error=%v, want wrapped unexpected EOF", err)
	}
	if unknown.Record.Status != k12.GradingStageOutcomeUnknown || unknown.Fields.Retryable {
		t.Fatalf("ambiguous sent recognition must park: status=%s fields=%+v",
			unknown.Record.Status, unknown.Fields)
	}
	assertStageInvocationStatus(t, o, jobID, k12.GradingStageRecognizing, k12.ModelInvocationOutcomeUnknown)

	if _, err := o.RetryAndRun(context.Background(), jobID); err == nil {
		t.Fatal("ordinary retry must reject an ambiguous sent recognition")
	}
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("query/recovery pass should remain parked: %v", err)
	}
	if recognizer.calls != 1 {
		t.Fatalf("ambiguous recognition was resent: calls=%d", recognizer.calls)
	}
}

type ambiguousAggregateGrader struct {
	err   error
	calls int
}

func (g *ambiguousAggregateGrader) Grade(context.Context, string, string, string) (GradeOutcome, error) {
	g.calls++
	return GradeOutcome{}, g.err
}

// REG-P0: the legacy aggregate assessment path has the same sent-before-call
// boundary as recognition. An untyped reset/EOF is outcome_unknown, not a
// safely retryable provider rejection.
func TestGradingAssessAmbiguousTransportAfterSendParksOutcomeUnknown(t *testing.T) {
	transportErr := fmt.Errorf("assessment transport lost after send: %w", io.ErrUnexpectedEOF)
	grader := &ambiguousAggregateGrader{err: transportErr}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
	}}}
	o := newOrchestrator(t, recognizer, nil, nil)
	o.deps.Grader = grader
	jobID := startOrchestratorJob(t, o, "assess-transport-unknown").Record.RecordID

	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("recognition: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record.Status == k12.GradingStageAwaitingConfirmation &&
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})

	unknown, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("assessment error=%v, want wrapped unexpected EOF", err)
	}
	if unknown.Record.Status != k12.GradingStageOutcomeUnknown || unknown.Fields.Retryable {
		t.Fatalf("ambiguous sent assessment must park: status=%s fields=%+v",
			unknown.Record.Status, unknown.Fields)
	}
	assertStageInvocationStatus(t, o, jobID, k12.GradingStageAssessing, k12.ModelInvocationOutcomeUnknown)

	if _, err := o.RetryAndRun(context.Background(), jobID); err == nil {
		t.Fatal("ordinary retry must reject an ambiguous sent assessment")
	}
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("query/recovery pass should remain parked: %v", err)
	}
	if grader.calls != 1 {
		t.Fatalf("ambiguous assessment was resent: calls=%d", grader.calls)
	}
}

func assertStageInvocationStatus(
	t *testing.T,
	o *GradingOrchestrator,
	jobID string,
	stage string,
	want k12.ModelInvocationStatus,
) {
	t.Helper()
	invocations, err := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatalf("list model invocations: %v", err)
	}
	for _, invocation := range invocations {
		if invocation.Stage == stage {
			if invocation.Status != want {
				t.Fatalf("%s invocation status=%s, want %s", stage, invocation.Status, want)
			}
			return
		}
	}
	t.Fatalf("%s invocation missing: %+v", stage, invocations)
}
