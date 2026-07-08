package engineadapter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

type argSpy struct{ got map[string]any }

func (s *argSpy) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	s.got = args
	return &skill.Result{Content: "解", Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"}}, nil
}

// 回归锁（修复①）：grade + constraint 必须透传给 solve skill（此前被丢弃 → 年级约束空转）。
func TestSolve_ForwardsGradeAndConstraint(t *testing.T) {
	spy := &argSpy{}
	a := NewSolveAdapter(spy)
	if _, err := a.Solve(context.Background(), "3.8×3", "五年级上", "分数加减、小数乘法"); err != nil {
		t.Fatal(err)
	}
	if spy.got["grade"] != "五年级上" {
		t.Errorf("grade 未透传，args=%v", spy.got)
	}
	if spy.got["constraint"] != "分数加减、小数乘法" {
		t.Errorf("constraint 未透传，args=%v", spy.got)
	}
}

// 回归锁（修复③）：强徽章「已程序验算」只在 solve_evidence=numeric_exec 时给；
// 纯 agree（无 code_exec 证据 = model）只能是弱徽章。
func TestEvidence_StrongOnlyWhenNumericExec(t *testing.T) {
	strong := evidenceFromMeta(map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"})
	if !strong.StrongTrust() || strong.Badge() != "verified-strong" {
		t.Errorf("numeric_exec 应强徽章, got %s / %s", strong.EvidenceType, strong.Badge())
	}
	weak := evidenceFromMeta(map[string]string{"solve_verdict": "agree", "solve_evidence": "model"})
	if weak.StrongTrust() || weak.Badge() == "verified-strong" {
		t.Errorf("model agree 不应强徽章, got %s / %s", weak.EvidenceType, weak.Badge())
	}
	// 无 solve_evidence（老数据）→ 保守判弱，不误显强。
	legacy := evidenceFromMeta(map[string]string{"solve_verdict": "agree"})
	if legacy.StrongTrust() {
		t.Errorf("缺 solve_evidence 应保守判弱, got %s", legacy.EvidenceType)
	}
}

// 回归锁（修复②）：out_of_scope verdict 映射为对应徽章。
func TestEvidence_OutOfScopeVerdict(t *testing.T) {
	ev := evidenceFromMeta(map[string]string{"solve_verdict": "out_of_scope"})
	if ev.Verdict != usecase.VerdictOutOfScope {
		t.Errorf("out_of_scope verdict 应保留, got %s", ev.Verdict)
	}
	if ev.Badge() != "out-of-scope" {
		t.Errorf("超纲徽章应 out-of-scope, got %s", ev.Badge())
	}
}
