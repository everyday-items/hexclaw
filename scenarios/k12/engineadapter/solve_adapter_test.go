package engineadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

// fakeExec 按 args 是否含 student_answer 返回不同的 canned Result（模拟 solve skill）。
type fakeExec struct {
	solveResult *skill.Result
	gradeResult *skill.Result
	lastArgs    map[string]any
}

func (f *fakeExec) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	f.lastArgs = args
	if _, grading := args["student_answer"]; grading {
		return f.gradeResult, nil
	}
	return f.solveResult, nil
}

func TestSolveAdapter_Solve_AgreeStrong(t *testing.T) {
	a := NewSolveAdapter(&fakeExec{
		solveResult: &skill.Result{
			Content:  "解题：3.8×3=11.4\n\n```hexclaw-subagents\n[{\"Agent\":\"solver\"}]\n```",
			Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"},
		},
	})
	sr, err := a.Solve(context.Background(), "3.8×3=?", "五年级上", "小数乘法")
	if err != nil {
		t.Fatal(err)
	}
	if sr.Evidence.Verdict != usecase.VerdictAgree || sr.Evidence.EvidenceType != usecase.EvidenceNumericExec {
		t.Errorf("agree 应映射 numeric_exec, got %+v", sr.Evidence)
	}
	if !sr.Evidence.StrongTrust() {
		t.Error("code_exec 一致应强证据")
	}
	if sr.Solution != "解题：3.8×3=11.4" {
		t.Errorf("解题正文应剥掉回执围栏, got %q", sr.Solution)
	}
}

func TestSolveAdapter_Solve_SkippedIsWeak(t *testing.T) {
	a := NewSolveAdapter(&fakeExec{
		solveResult: &skill.Result{Content: "1+1=2", Metadata: map[string]string{"solve_verdict": "skipped"}},
	})
	sr, _ := a.Solve(context.Background(), "1+1=?", "五年级上", "")
	if sr.Evidence.StrongTrust() {
		t.Error("skipped(未验算) 不应强证据")
	}
	if sr.Evidence.Verdict != usecase.VerdictUnverifiable {
		t.Errorf("skipped 应归一为 unverifiable, got %q", sr.Evidence.Verdict)
	}
}

func TestSolveAdapter_Grade(t *testing.T) {
	a := NewSolveAdapter(&fakeExec{
		gradeResult: &skill.Result{
			Content: "批改...",
			Metadata: map[string]string{
				"solve_mode": "grading", "grade_correct": "false",
				"grade_wrong_step": "3.8×3 误算为 10.4", "grade_misconception": "小数点错位",
			},
		},
	})
	out, err := a.Grade(context.Background(), "3.8×3=?", "10.4", "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Correct {
		t.Error("应判错")
	}
	if out.WrongStep != "3.8×3 误算为 10.4" || out.ErrorCause != "小数点错位" {
		t.Errorf("批改结构化映射错: %+v", out)
	}
}

func TestSolveAdapter_Grade_Correct(t *testing.T) {
	a := NewSolveAdapter(&fakeExec{
		gradeResult: &skill.Result{Metadata: map[string]string{"grade_correct": "true"}},
	})
	out, _ := a.Grade(context.Background(), "1+1=?", "2", "")
	if !out.Correct {
		t.Error("应判对")
	}
}

func TestSolveAdapter_GradeRejectsMissingOrInvalidCorrectMetadata(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]string
	}{
		{name: "missing", meta: map[string]string{}},
		{name: "invalid", meta: map[string]string{"grade_correct": "not-a-bool"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewSolveAdapter(&fakeExec{gradeResult: &skill.Result{Metadata: tt.meta}})
			if _, err := a.Grade(context.Background(), "1+1=?", "2", ""); err == nil {
				t.Fatal("missing/invalid grade_correct must fail closed")
			}
		})
	}
}

// TestSolveAdapter_TrivialArithmeticGoesThroughVerifier 用**真 solve skill**（非 canned Result）
// 端到端证明：像 4.5×2= 这样的可执行简单题，K12 /solve 分叉必须走 verifier code_exec 精算，
// 拿到 agree + numeric_exec 强证据，而非被 solve 的 trivial-skip triage 判成 unverifiable（BUG-20260712）。
//
// RED（fix 前）：SolveSubject 不传 self_consistency → engine triage 认定 4.5×2= 为 trivial → skipVerify
// → verifier 从不被调用、verdict=skipped → 本层归一 unverifiable、弱证据。断言失败。
// GREEN（fix 后）：SolveSubject 显式传 self_consistency=1 关掉 triage → verifier 真跑 → agree/numeric_exec。
func TestSolveAdapter_TrivialArithmeticGoesThroughVerifier(t *testing.T) {
	verifierCalled := false
	execFn := func(_ context.Context, spec engine.SubAgentSpec) (engine.SubAgentResult, error) {
		switch spec.Agent {
		case "solver":
			return engine.SubAgentResult{Output: "4.5 × 2 = 9\n答案：9"}, nil
		case "verifier":
			verifierCalled = true
			return engine.SubAgentResult{Output: "VERDICT: AGREE\nCOMPUTED: 9\n说明：一致。"}, nil
		}
		return engine.SubAgentResult{}, nil
	}
	a := NewSolveAdapter(engine.NewSolveSkill(execFn, nil))

	sr, err := a.Solve(context.Background(), "4.5×2=", "五年级上", "")
	if err != nil {
		t.Fatal(err)
	}
	if !verifierCalled {
		t.Fatal("可执行简单题应走 verifier code_exec 精算，实际未调用 verifier（被 trivial-skip triage 跳过）")
	}
	if sr.Evidence.Verdict != usecase.VerdictAgree {
		t.Errorf("4.5×2= 应 verdict=agree，got %q", sr.Evidence.Verdict)
	}
	if sr.Evidence.EvidenceType != usecase.EvidenceNumericExec || !sr.Evidence.StrongTrust() {
		t.Errorf("code_exec 精算相等应为 numeric_exec 强证据（verified-strong），got %+v", sr.Evidence)
	}
	if !strings.Contains(sr.Solution, "9") {
		t.Errorf("解题正文应含最终答案 9，got %q", sr.Solution)
	}
}

// countingExec 统计 SolveExecutor.Execute 被调次数——代表「走完整 solve/ReAct 工具循环」的路径。
type countingExec struct{ calls int }

func (c *countingExec) Execute(_ context.Context, _ map[string]any) (*skill.Result, error) {
	c.calls++
	return &skill.Result{Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"}}, nil
}

// TestSolveAdapter_GenerateSimilar_UsesBareClosure_NotReActExecutor —— BUG-20260712 治本²：
// 注入轻量出题闭包后，「再练一道」只走裸闭包（对应 main.go 里裸 LLM Complete，不进 ReAct 工具循环），
// **绝不**落到 SolveExecutor（完整 solve/ReAct 工具链）；且 subject/grade 透传、verdict=unverifiable、
// 不给强证据（不冒充已程序验算）。
//
// RED：若 GenerateSimilar 回退/误走 exec.Execute（ReAct 全链），exec.calls>0 → 失败。
// GREEN：注入 retryGen 时 exec.calls==0、闭包恰调 1 次、证据为 unverifiable/none。
func TestSolveAdapter_GenerateSimilar_UsesBareClosure_NotReActExecutor(t *testing.T) {
	exec := &countingExec{}
	closureCalls := 0
	var gotSubject, gotGrade string
	a := NewSolveAdapter(exec, WithRetryGen(func(_ context.Context, subject, _, grade string) (string, error) {
		closureCalls++
		gotSubject, gotGrade = subject, grade
		return "变式题：3.9×3=? 答案 11.7\n\n```hexclaw-subagents\n[{\"Agent\":\"solver\"}]\n```", nil
	}))

	sr, err := a.GenerateSimilar(context.Background(), "数学", "据「小数乘法」出一道同类练习", "五年级上")
	if err != nil {
		t.Fatal(err)
	}
	if exec.calls != 0 {
		t.Fatalf("再练一道不得走 SolveExecutor(ReAct 工具循环), Execute 被调 %d 次", exec.calls)
	}
	if closureCalls != 1 {
		t.Fatalf("应恰调 1 次裸出题闭包, got %d", closureCalls)
	}
	if gotSubject != "数学" || gotGrade != "五年级上" {
		t.Fatalf("subject/grade 未透传: subject=%q grade=%q", gotSubject, gotGrade)
	}
	if sr.Evidence.Verdict != usecase.VerdictUnverifiable || sr.Evidence.EvidenceType != usecase.EvidenceNone {
		t.Fatalf("未验算的练习变式题应 unverifiable/none（不冒充已程序验算), got %+v", sr.Evidence)
	}
	if sr.Evidence.StrongTrust() {
		t.Fatal("练习变式题绝不给强证据")
	}
	if sr.Solution != "变式题：3.9×3=? 答案 11.7" {
		t.Fatalf("解题正文应剥掉回执围栏, got %q", sr.Solution)
	}
}

// TestSolveAdapter_GenerateSimilar_NilClosureFallsBackToFullChain —— 未注入闭包时安全回退
// 全链（SolveSubject → exec），保证正确性不塌。
func TestSolveAdapter_GenerateSimilar_NilClosureFallsBackToFullChain(t *testing.T) {
	exec := &countingExec{}
	a := NewSolveAdapter(exec) // 不注入 retryGen
	if _, err := a.GenerateSimilar(context.Background(), "数学", "出一道练习", "五年级上"); err != nil {
		t.Fatal(err)
	}
	if exec.calls != 1 {
		t.Fatalf("nil 闭包应回退全链(SolveSubject→exec), Execute 调用次数 got %d want 1", exec.calls)
	}
}

func TestSolveAdapter_SubjectPassThrough(t *testing.T) {
	exec := &fakeExec{
		solveResult: &skill.Result{Metadata: map[string]string{"solve_verdict": "agree"}},
		gradeResult: &skill.Result{Metadata: map[string]string{"grade_correct": "true"}},
	}
	a := NewSolveAdapter(exec)
	if _, err := a.SolveSubject(context.Background(), "英语", "fill the blank", "初一上", ""); err != nil {
		t.Fatal(err)
	}
	if got := exec.lastArgs["subject"]; got != "英语" {
		t.Fatalf("solve subject=%v want 英语", got)
	}
	if _, err := a.GradeSubject(context.Background(), "英语", "fill the blank", "goes", ""); err != nil {
		t.Fatal(err)
	}
	if got := exec.lastArgs["subject"]; got != "英语" {
		t.Fatalf("grade subject=%v want 英语", got)
	}
}
