package engineadapter

import (
	"context"
	"testing"

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
