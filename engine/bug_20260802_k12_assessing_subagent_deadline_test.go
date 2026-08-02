package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// REG-P0: a durable K12 assessing deadline is already frozen by the Job. The
// generic per-subagent anti-hang guard must not silently shorten it.
func TestBUG20260802K12FrozenStageDeadlineOutranksGenericPerTryGuard(t *testing.T) {
	stageCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	stageCtx = WithAuthoritativeCallerDeadline(stageCtx)

	var seenDeadline time.Time
	_, err := runSubAgentWithRetry(stageCtx, func(ctx context.Context, _ SubAgentSpec) (SubAgentResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("durable K12 child lost the frozen stage deadline")
		}
		seenDeadline = deadline
		select {
		case <-time.After(70 * time.Millisecond):
			return SubAgentResult{Output: "ok"}, nil
		case <-ctx.Done():
			return SubAgentResult{}, ctx.Err()
		}
	}, SubAgentSpec{Agent: "solver"}, 25*time.Millisecond)
	if err != nil {
		t.Fatalf("authoritative K12 child was cut by generic per-try guard: %v", err)
	}
	if remaining := time.Until(seenDeadline); remaining < 250*time.Millisecond {
		t.Fatalf("child deadline remaining=%s, want frozen stage rather than 25ms guard", remaining)
	}
}

func TestBUG20260802GenericSubAgentStillUsesConfiguredPerTryGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	childCtx, cancelChild := newSubAgentAttemptContext(ctx, 25*time.Millisecond)
	defer cancelChild()
	deadline, ok := childCtx.Deadline()
	if !ok || time.Until(deadline) > 50*time.Millisecond {
		t.Fatalf("ordinary child deadline=%v, want configured generic per-try guard", deadline)
	}
	select {
	case <-childCtx.Done():
		if !errors.Is(childCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("ordinary child error=%v, want generic per-try deadline", childCtx.Err())
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ordinary child did not receive configured generic per-try guard")
	}
}

// REG-P0: both full Solve and verified-grade use a 5 minute engine wall guard
// today. A K12 durable operation must retain the already-frozen stage deadline
// through both shared paths, while this test makes the former guard observable.
func TestBUG20260802K12DeadlineSurvivesSolveAndVerifiedGradeWallGuards(t *testing.T) {
	previousWall := orchestrateMaxWall
	orchestrateMaxWall = 25 * time.Millisecond
	defer func() { orchestrateMaxWall = previousWall }()

	stageCtx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	stageCtx = WithAuthoritativeCallerDeadline(stageCtx)

	var (
		mu        sync.Mutex
		callCount int
	)
	exec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 300*time.Millisecond {
			return SubAgentResult{}, errors.New("shared engine child received a shortened deadline")
		}
		select {
		case <-time.After(45 * time.Millisecond):
		case <-ctx.Done():
			return SubAgentResult{}, ctx.Err()
		}
		mu.Lock()
		callCount++
		mu.Unlock()
		switch spec.Agent {
		case solverAgentName:
			return SubAgentResult{Output: "步骤：按图形关系求解\n答案：42"}, nil
		case verifierAgentName:
			return SubAgentResult{Output: "VERDICT: AGREE\nCOMPUTED: 42"}, nil
		case graderAgentName:
			return SubAgentResult{Output: "CORRECT: yes\nGUIDANCE: 继续保持"}, nil
		default:
			return SubAgentResult{}, errors.New("unexpected subagent")
		}
	}

	solver := NewSolveSkill(exec, NewSubAgentRegistry(""))
	if _, err := solver.Execute(stageCtx, map[string]any{
		"problem": "图形阴影面积题，请给出面积。", "self_consistency": 1,
	}); err != nil {
		t.Fatalf("full solve was cut by generic wall guard: %v", err)
	}
	if _, err := solver.GradeVerified(stageCtx,
		"图形阴影面积题，请给出面积。", "步骤：按图形关系求解\n答案：42", "42"); err != nil {
		t.Fatalf("verified grade was cut by generic wall guard: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if callCount != 3 {
		t.Fatalf("engine child calls=%d, want solver+verifier+grader", callCount)
	}
}
