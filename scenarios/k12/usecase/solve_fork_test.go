package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// gradeOnlySolver 解题返回解法；批改用 panicGrader 证明空白分叉不进批改。

// TestSolveFork_BlankGoesToSolveNotGrade 单一真相源显式分叉（治本核心）：
// student_answer 为空 → 走解题（SolveOnly），**不批改、不入错题本、不写学情**，且绝不报错。
// grader 用 panicGrader：一旦被调用即 panic，证明空白题短路在批改之前。
func TestSolveFork_BlankGoesToSolveNotGrade(t *testing.T) {
	ins := &fakeInsights{}
	d, store := newPipeline(t,
		fakeSolver{solution: "解：3.8×3=11.4", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		panicGrader{}, // 空白题若误入批改 → panic
		ins,
	)
	ctx := context.Background()

	res, err := d.GradeHomeworkProblem(ctx, GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "s1",
		Problem: "3.8 × 3 = ?", StudentAnswer: "", KnowledgePoints: []string{"小数乘法"},
	})
	if err != nil {
		t.Fatalf("空白题不应报错（治本前会因缺 grade_correct 而 502）: %v", err)
	}
	if !res.SolveOnly {
		t.Error("空白题应标 SolveOnly")
	}
	if res.Solution == "" {
		t.Error("空白题应返回解法")
	}
	if res.RecordCreated {
		t.Error("空白题不应入错题本")
	}
	if got, _ := store.ListByScope(ctx, "mingming", k12.CollectionMistakes, ""); len(got) != 0 {
		t.Errorf("空白题错题本应为空, got %d", len(got))
	}
	if len(ins.notes) != 0 {
		t.Error("空白题不应写学情")
	}
}

// TestSolveFork_BlankWhitespaceTreatedBlank 纯空白/空格的作答等同空白（TrimSpace 语义）。
func TestSolveFork_BlankWhitespaceTreatedBlank(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "解法", ev: SolveEvidence{Verdict: VerdictAgree}},
		panicGrader{}, &fakeInsights{})
	res, err := d.GradeHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Grade: "五年级上", Problem: "1+1=?", StudentAnswer: "   ",
	})
	if err != nil {
		t.Fatalf("空格作答应走解题不报错: %v", err)
	}
	if !res.SolveOnly {
		t.Error("空格作答应视为空白→SolveOnly")
	}
}

// TestSolveFork_AnsweredStillGrades 已答题仍走批改路径（回归保护：分叉不误伤已答卷）。
func TestSolveFork_AnsweredStillGrades(t *testing.T) {
	ins := &fakeInsights{}
	d, store := newPipeline(t,
		fakeSolver{solution: "11.4", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictDisagree, WrongStep: "误算", ErrorCause: "计算失误", KnowledgePoint: "小数乘法"}},
		ins,
	)
	ctx := context.Background()
	res, err := d.GradeHomeworkProblem(ctx, GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "s2",
		Problem: "3.8 × 3 = ?", StudentAnswer: "10.4", KnowledgePoints: []string{"小数乘法"},
	})
	if err != nil {
		t.Fatalf("已答题批改报错: %v", err)
	}
	if res.SolveOnly {
		t.Error("已答题不应标 SolveOnly")
	}
	if !res.RecordCreated {
		t.Error("已答判错应入错题本")
	}
	if got, _ := store.ListByScope(ctx, "mingming", k12.CollectionMistakes, ""); len(got) != 1 {
		t.Errorf("已答判错错题本应有 1 条, got %d", len(got))
	}
}

// TestSolveHomeworkProblem_Direct 直呼解题分叉端点：返回解法+证据，不触碰批改/记录。
func TestSolveHomeworkProblem_Direct(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "解：x=14", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		panicGrader{}, &fakeInsights{})
	res, err := d.SolveHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Grade: "五年级上", Problem: "2x+15=43", KnowledgePoints: []string{"小数乘法"},
	})
	if err != nil {
		t.Fatalf("SolveHomeworkProblem 报错: %v", err)
	}
	if res.Solution == "" || res.OutOfScope {
		t.Errorf("应返回解法且不超纲: %+v", res)
	}
}

// TestSolveHomeworkProblem_OutOfScope 空白题也守超纲红线：晚学知识点→反问不解题。
func TestSolveHomeworkProblem_OutOfScope(t *testing.T) {
	d, _ := newPipeline(t, panicSolver{}, panicGrader{}, &fakeInsights{})
	res, err := d.SolveHomeworkProblem(context.Background(), GradeRequest{
		AgentName: "mingming", Grade: "五年级上", Problem: "解方程组", KnowledgePoints: []string{"解方程组"},
	})
	if err != nil {
		t.Fatalf("超纲不应报错: %v", err)
	}
	if !res.OutOfScope || res.OutOfScopeKP != "解方程组" {
		t.Fatalf("空白题超纲应反问: %+v", res)
	}
}
