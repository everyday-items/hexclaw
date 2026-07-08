package usecase

import "testing"

func TestSolveEvidence_StrongTrust(t *testing.T) {
	cases := []struct {
		name string
		ev   SolveEvidence
		want bool
	}{
		{"数值执行=强", SolveEvidence{EvidenceType: EvidenceNumericExec}, true},
		{"符号执行=强", SolveEvidence{EvidenceType: EvidenceSymbolicExec}, true},
		{"异构模型且真异构=强", SolveEvidence{EvidenceType: EvidenceHeterogeneousModel, SolverModel: "glm", VerifierModel: "gpt"}, true},
		{"异构类型但同源=弱", SolveEvidence{EvidenceType: EvidenceHeterogeneousModel, SolverModel: "glm", VerifierModel: "glm"}, false},
		{"同源启发=弱", SolveEvidence{EvidenceType: EvidenceHeuristic}, false},
		{"无验证=弱", SolveEvidence{EvidenceType: EvidenceNone}, false},
	}
	for _, c := range cases {
		if got := c.ev.StrongTrust(); got != c.want {
			t.Errorf("%s: StrongTrust=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestSolveEvidence_Badge(t *testing.T) {
	// 同源一致 → 不得显"已程序验算"强徽章，只弱徽章
	weak := SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceHeuristic}
	if b := weak.Badge(); b != "verified-weak" {
		t.Errorf("同源一致应 verified-weak, got %q", b)
	}
	strong := SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}
	if b := strong.Badge(); b != "verified-strong" {
		t.Errorf("数值验算一致应 verified-strong, got %q", b)
	}
	if b := (SolveEvidence{Verdict: VerdictOutOfScope}).Badge(); b != "out-of-scope" {
		t.Errorf("超纲应 out-of-scope, got %q", b)
	}
}
