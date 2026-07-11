package main

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/skill"
)

type countingSolveExecutor struct {
	calls int
	ctx   context.Context
}

func (e *countingSolveExecutor) Execute(ctx context.Context, _ map[string]any) (*skill.Result, error) {
	e.calls++
	e.ctx = ctx
	return &skill.Result{Content: "ok"}, nil
}

func TestBUG20260710_K12SolveCarriesExplicitEgressClassification(t *testing.T) {
	next := &countingSolveExecutor{}
	exec := classifiedSolveExecutor{next: next}
	if _, err := exec.Execute(context.Background(), map[string]any{"problem": "1+1"}); err != nil {
		t.Fatal(err)
	}
	if next.calls != 1 {
		t.Fatalf("classified executor calls=%d", next.calls)
	}
	got, _ := egress.RequestsFromContext(next.ctx)
	if len(got) != 2 || got[0].Purpose != egress.PurposeSolveVerify || got[1].Purpose != egress.PurposeSolveVerify ||
		got[0].DataClass != egress.ClassGeneral || got[1].DataClass != egress.ClassSensitiveProfile {
		t.Fatalf("solve 出网标签错误: %+v", got)
	}
}
