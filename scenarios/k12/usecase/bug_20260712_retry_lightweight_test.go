package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// spyRetrySolver 同时实现 Solver（全对抗验算链）与 RetryGenerator（轻量出题）。
// BUG-20260712 治本断言：「再练一道」只走轻量 GenerateSimilar 出题，
// 绝不落全对抗多智能体验算链（变式生成→solver→verifier→code_exec→self-consistency→不合格重出，真机 68s）。
type spyRetrySolver struct {
	generateCalls int
	solveCalls    int
	gotSubject    string
	gotGrade      string
}

func (s *spyRetrySolver) Solve(context.Context, string, string, string) (SolveResult, error) {
	s.solveCalls++
	return SolveResult{Solution: "全链解", Evidence: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}}, nil
}

func (s *spyRetrySolver) SolveSubject(_ context.Context, subject, _, _, _ string) (SolveResult, error) {
	s.solveCalls++
	s.gotSubject = subject
	return SolveResult{Solution: "全链解", Evidence: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}}, nil
}

func (s *spyRetrySolver) GenerateSimilar(_ context.Context, subject, _, grade string) (SolveResult, error) {
	s.generateCalls++
	s.gotSubject = subject
	s.gotGrade = grade
	return SolveResult{Solution: "轻量相似题", Evidence: SolveEvidence{Verdict: VerdictUnverifiable, EvidenceType: EvidenceNone}}, nil
}

// TestGenerateRetry_UsesLightweightPath_NotFullSolveChain —— 再练一道走轻量单次出题、
// 不跑全对抗验算链；同时保留 subject 路由 + grade 透传。
func TestGenerateRetry_UsesLightweightPath_NotFullSolveChain(t *testing.T) {
	spy := &spyRetrySolver{}
	d, _ := newPipeline(t, spy, fakeGrader{}, nil)
	sr, err := d.GenerateRetry(context.Background(), ReviewItem{
		Fields: k12.MistakeFields{Subject: "物理", Question: "速度是多少", KnowledgePoint: "速度"},
	}, "初二上")
	if err != nil {
		t.Fatal(err)
	}
	if spy.generateCalls != 1 {
		t.Fatalf("轻量出题应恰调 1 次 GenerateSimilar, got %d", spy.generateCalls)
	}
	if spy.solveCalls != 0 {
		t.Fatalf("再练一道不得落全对抗验算链, Solve/SolveSubject 被调 %d 次", spy.solveCalls)
	}
	if spy.gotSubject != "物理" {
		t.Fatalf("subject 路由丢失: got %q", spy.gotSubject)
	}
	if spy.gotGrade != "初二上" {
		t.Fatalf("grade 未透传: got %q", spy.gotGrade)
	}
	if sr.Solution != "轻量相似题" {
		t.Fatalf("未取轻量出题结果: got %q", sr.Solution)
	}
}

// TestGenerateRetry_LegacySolverFallsBackToFullChain —— 只实现 Solver 的老 adapter
// 仍走全链（向后兼容），不因新增轻量口而破。
func TestGenerateRetry_LegacySolverFallsBackToFullChain(t *testing.T) {
	solver := &subjectCaptureSolver{}
	d, _ := newPipeline(t, solver, fakeGrader{}, nil)
	if _, err := d.GenerateRetry(context.Background(), ReviewItem{
		Fields: k12.MistakeFields{Subject: "物理", Question: "速度是多少", KnowledgePoint: "速度"},
	}, "初二上"); err != nil {
		t.Fatal(err)
	}
	if solver.subject != "物理" || solver.constraint != "" {
		t.Fatalf("回退全链丢失 subject 路由: subject=%q constraint=%q", solver.subject, solver.constraint)
	}
}
