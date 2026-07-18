package usecase

import (
	"context"
	"testing"
)

// Bug 2026-07-18（真实环境回归矩阵 A5 发现）：
// 词表外知识点（如「一元二次方程」——不在课标 35 词表）在超纲倒查里被 `ok &&` 静默跳过：
// 初中题只要挂一个词表外知识点名就能绕过硬拦截被正常批改，且响应无任何提示。
// curriculum 包 fail-visible 契约（PRD §5.2.4）明文要求「词表外 → 调用方显性提示
// 『不在课标映射内』，不静默」。
//
// 契约：GradeResult / SolveHomeworkResult 必须携带 CurriculumUnmapped（词表外知识点清单），
// 让调用方（HTTP DTO / 前端 / IM 文案）能显性呈现，不得静默吞掉。
// 词表内知识点不得进入该清单。
func TestBug20260718_UnmappedKPFailVisible_Grade(t *testing.T) {
	ins := &fakeInsights{}
	d, _ := newPipeline(t,
		fakeSolver{solution: "x=2 或 x=3", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec, SolverModel: "m", VerifierModel: "v"}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		ins,
	)
	ctx := context.Background()

	res, err := d.GradeHomeworkProblem(ctx, GradeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级下", SourceSession: "s1",
		Problem: "解方程：x²-5x+6=0", StudentAnswer: "x=2或x=3",
		// 「一元二次方程」词表外；「简易方程」词表内（五年级上，不超纲）。
		KnowledgePoints: []string{"一元二次方程", "简易方程"},
	})
	if err != nil {
		t.Fatalf("批改报错: %v", err)
	}
	if len(res.CurriculumUnmapped) != 1 || res.CurriculumUnmapped[0] != "一元二次方程" {
		t.Fatalf("fail-visible 契约：词表外知识点必须显性透出 CurriculumUnmapped=[一元二次方程]，got %v", res.CurriculumUnmapped)
	}
}

// 解题分叉同一契约（同一个超纲倒查入口）。
func TestBug20260718_UnmappedKPFailVisible_Solve(t *testing.T) {
	ins := &fakeInsights{}
	d, _ := newPipeline(t,
		fakeSolver{solution: "x=1", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{},
		ins,
	)
	res, err := d.SolveHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级下",
		Problem: "解方程：x²-1=0", KnowledgePoints: []string{"一元二次方程"},
	})
	if err != nil {
		t.Fatalf("解题报错: %v", err)
	}
	if len(res.CurriculumUnmapped) != 1 || res.CurriculumUnmapped[0] != "一元二次方程" {
		t.Fatalf("fail-visible 契约：解题分叉也必须透出 CurriculumUnmapped，got %v", res.CurriculumUnmapped)
	}
}

// 词表内知识点正常路径不受影响：不产生 unmapped 噪音。
func TestBug20260718_MappedKPNoUnmappedNoise(t *testing.T) {
	ins := &fakeInsights{}
	d, _ := newPipeline(t,
		fakeSolver{solution: "11.4", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		ins,
	)
	res, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Subject: "数学", Grade: "五年级上",
		Problem: "3.8×3=", StudentAnswer: "11.4", KnowledgePoints: []string{"简易方程"},
	})
	if err != nil {
		t.Fatalf("批改报错: %v", err)
	}
	if len(res.CurriculumUnmapped) != 0 {
		t.Fatalf("词表内知识点不得进 CurriculumUnmapped，got %v", res.CurriculumUnmapped)
	}
}
