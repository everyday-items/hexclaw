package usecase

import (
	"context"
	"testing"
)

type verifiedReuseGrader struct {
	legacyCalls   int
	verifiedCalls int
	gotSubject    string
	gotSolution   string
}

func (g *verifiedReuseGrader) Grade(context.Context, string, string, string) (GradeOutcome, error) {
	g.legacyCalls++
	return GradeOutcome{}, nil
}

func (g *verifiedReuseGrader) GradeVerified(_ context.Context, subject, _, _, verifiedSolution string) (GradeOutcome, error) {
	g.verifiedCalls++
	g.gotSubject = subject
	g.gotSolution = verifiedSolution
	return GradeOutcome{Correct: true}, nil
}

// BUG-20260714：GradeHomeworkProblem 已先跑过 solver+verifier，旧 adapter 随后又从零跑一遍
// solver+verifier+grader，整张 9 题超过 7 分钟。批改必须复用第一阶段的已验证解法，只追加 grader 对比。
func TestBUG20260714_GradeReusesAlreadyVerifiedSolution(t *testing.T) {
	g := &verifiedReuseGrader{}
	d, _ := newPipeline(t, fakeSolver{
		solution: "解：3.8×3=11.4\n答案：11.4",
		ev:       SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec},
	}, g, nil)
	// 生产 composition root 显式接入，避免 decorator/接口收窄后静默丢掉快口。
	d.VerifiedGrader = g

	res, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级上",
		Problem: "3.8×3=?", StudentAnswer: "11.4", KnowledgePoints: []string{"小数乘法"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Outcome.Correct {
		t.Fatal("expected correct result")
	}
	if g.verifiedCalls != 1 || g.legacyCalls != 0 {
		t.Fatalf("verified=%d legacy=%d, want verified=1 legacy=0", g.verifiedCalls, g.legacyCalls)
	}
	if g.gotSubject != "数学" || g.gotSolution != "解：3.8×3=11.4\n答案：11.4" {
		t.Fatalf("verified solution was not reused: subject=%q solution=%q", g.gotSubject, g.gotSolution)
	}
}
