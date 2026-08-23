package usecase

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// TestGrade_ErrorCause_StripsVerifierSelfCheck（项-6b · RED→GREEN）：
// grader 偶发把 verifier 的自查过程/评审链原文塞进 misconception（真机取证：错因存成
// 「未分析…自查:- 关键条件是否都用到? √…自查通过」）。治本：入库 error_cause 只留简洁错因，
// 不含「自查:」/核对勾叉行/评审链原文。
func TestGrade_ErrorCause_StripsVerifierSelfCheck(t *testing.T) {
	dump := "移项时把 +15 抄成 -15。\n自查:\n- 关键条件是否都用到? √\n- 推理正确 √\n自查通过"
	d, store := newPipeline(t,
		fakeSolver{solution: "x=14", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictDisagree, WrongStep: "移项", ErrorCause: dump, KnowledgePoint: "简易方程"}},
		&fakeInsights{},
	)
	ctx := context.Background()
	res, err := d.GradeHomeworkProblem(ctx, GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "s1",
		Problem: "2x+15=43", StudentAnswer: "x=29", KnowledgePoints: []string{"简易方程"},
	})
	if err != nil {
		t.Fatalf("批改闭环报错: %v", err)
	}
	rec, err := store.Get(ctx, res.RecordID)
	if err != nil {
		t.Fatalf("取错题: %v", err)
	}
	f, _ := k12.ParseMistakeFields(rec.Fields)
	// 简洁错因保留、自查全文剥离。
	if !strings.Contains(f.ErrorCause, "移项") {
		t.Fatalf("应保留简洁错因，得 %q", f.ErrorCause)
	}
	for _, bad := range []string{"自查", "√", "关键条件是否", "自查通过"} {
		if strings.Contains(f.ErrorCause, bad) {
			t.Fatalf("error_cause 不应含验算自查原文 %q，得 %q", bad, f.ErrorCause)
		}
	}
	// 返回给前端的 outcome 同样是简洁版。
	if strings.Contains(res.Outcome.ErrorCause, "自查") {
		t.Fatalf("返回 outcome.ErrorCause 不应含自查原文，得 %q", res.Outcome.ErrorCause)
	}
}

// TestSanitizeErrorCause_Units 直接锁定纯函数不变量。
func TestSanitizeErrorCause_Units(t *testing.T) {
	cases := []struct{ in, want string }{
		{"计算失误", "计算失误"}, // 干净短错因原样保留
		{"移项符号错。自查:- 推理正确 √ 自查通过", "移项符号错。"},
		{"未分析\n自查:\n- 关键条件是否都用到? √", "未分析"},
		{"- 关键条件是否都用到? √\n- 推理正确 √", ""}, // 全是核对行 → 空
		{"- 推理正确 ×", ""},
		{"42=18×2 不成立，误把两边作为相等。", "42=18×2 不成立，误把两边作为相等。"},
		{"42=18 × 2 不成立，误把两边作为相等。", "42=18 × 2 不成立，误把两边作为相等。"},
		{"  进位算错  ", "进位算错"},
	}
	for _, c := range cases {
		if got := sanitizeErrorCause(c.in); got != c.want {
			t.Errorf("sanitizeErrorCause(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
