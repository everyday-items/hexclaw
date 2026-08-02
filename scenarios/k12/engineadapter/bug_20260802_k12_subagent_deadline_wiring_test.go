package engineadapter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

type k12SubagentDeadlinePhysicalExecutor struct{}

func (k12SubagentDeadlinePhysicalExecutor) ExecuteGradingPhysicalCall(
	context.Context,
	usecase.GradingPhysicalCallSpec,
	func(context.Context) (string, error),
) (usecase.GradingPhysicalCallResult, error) {
	return usecase.GradingPhysicalCallResult{}, errors.New("test executor must not send")
}

type k12SubagentDeadlineCaptureExec struct {
	mu   sync.Mutex
	ctxs []context.Context
}

func (*k12SubagentDeadlineCaptureExec) SupportsSubAgentCallInterceptor() bool { return true }

func (e *k12SubagentDeadlineCaptureExec) capture(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ctxs = append(e.ctxs, ctx)
}

func (e *k12SubagentDeadlineCaptureExec) Execute(ctx context.Context, _ map[string]any) (*skill.Result, error) {
	e.capture(ctx)
	return &skill.Result{Content: "答案：42", Metadata: map[string]string{
		"solve_verdict": "agree", "solve_evidence": "numeric_exec",
	}}, nil
}

func (e *k12SubagentDeadlineCaptureExec) GradeVerified(
	ctx context.Context, _, _, _ string,
) (*skill.Result, error) {
	e.capture(ctx)
	return &skill.Result{Metadata: map[string]string{"grade_correct": "true"}}, nil
}

// REG-P0: this is the production composition seam. Only a durable grading
// physical-call context with an actual frozen deadline may opt into the shared
// engine's authoritative-caller deadline mode.
func TestBUG20260802SolveAdapterWiresAuthoritativeDeadlineOnlyForDurableK12Calls(t *testing.T) {
	stageCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	stageCtx = usecase.WithGradingPhysicalCallExecutor(stageCtx, k12SubagentDeadlinePhysicalExecutor{})

	capture := &k12SubagentDeadlineCaptureExec{}
	adapter := NewSolveAdapter(capture)
	if _, err := adapter.SolveSubject(stageCtx, "数学", "图形题", "五年级", ""); err != nil {
		t.Fatalf("solve through production adapter: %v", err)
	}
	if _, err := adapter.GradeVerified(stageCtx, "数学", "图形题", "42", "答案：42"); err != nil {
		t.Fatalf("verified grade through production adapter: %v", err)
	}

	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.ctxs) != 2 {
		t.Fatalf("captured contexts=%d, want solve plus verified grade", len(capture.ctxs))
	}
	for index, got := range capture.ctxs {
		if !engine.HasAuthoritativeCallerDeadline(got) {
			t.Fatalf("captured context[%d] lacks durable deadline authority marker", index)
		}
		deadline, ok := got.Deadline()
		if !ok || time.Until(deadline) < 250*time.Millisecond {
			t.Fatalf("captured context[%d] deadline=%v, want original frozen stage deadline", index, deadline)
		}
	}

	plain := context.Background()
	if engine.HasAuthoritativeCallerDeadline(plain) {
		t.Fatal("ordinary context unexpectedly carries K12 durable deadline authority")
	}
}
