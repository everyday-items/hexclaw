package usecase

import (
	"context"
	"testing"
)

type solverReportedOutOfScope struct{}

func (solverReportedOutOfScope) Solve(context.Context, string, string, string) (SolveResult, error) {
	return SolveResult{
		Evidence:     SolveEvidence{Verdict: VerdictOutOfScope, EvidenceType: EvidenceNone},
		OutOfScopeKP: "分数的意义和性质",
	}, nil
}

func TestBUG20260714_SolverReportedOutOfScopeSkipsGrader(t *testing.T) {
	d, store := newPipeline(t, solverReportedOutOfScope{}, panicGrader{}, nil)
	res, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级上", SourceSession: "scope-from-solver",
		Problem: "一个数的3/8是24，求这个数？", StudentAnswer: "64", KnowledgePoints: []string{"分数乘除法"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OutOfScope || res.OutOfScopeKP != "分数的意义和性质" || res.RecordCreated {
		t.Fatalf("solver-reported scope result was not propagated: %+v", res)
	}
	items, err := store.ListByScope(context.Background(), "mingming", "mistakes", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("out-of-scope result must not create a mistake, got %d", len(items))
	}
}

func TestBUG20260714_BlankSolvePropagatesSolverReportedOutOfScope(t *testing.T) {
	d, _ := newPipeline(t, solverReportedOutOfScope{}, panicGrader{}, nil)
	res, err := d.SolveHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级上",
		Problem: "一个数的3/8是24，求这个数？", KnowledgePoints: []string{"分数乘除法"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OutOfScope || res.OutOfScopeKP != "分数的意义和性质" {
		t.Fatalf("blank solve lost solver-reported scope: %+v", res)
	}
}
