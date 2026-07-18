package engineadapter

// K12-INV-019 入库口径钉死（架构设计-v0.5.0 §7 / ADR-K12-008，裁决 2026-07-18）：
// canonical_answer 与 solution 在 **入库前** 由 adapter 边界 Normalize 为 Unicode 规范形态
// （存储即规范形）——下游 IM/导出/桌面全部拿到干净 Unicode；channel.LaTeXToUnicode
// 出口兜底只是第二道防线。
//
// 本测试走真实链路：fake 模型（违反提示词硬禁、输出 LaTeX）→ SolveAdapter（归一化边界）
// → usecase.GradeHomeworkProblem（判错入库 CanonicalAnswer=已验算解法）→ SQLite 存储，
// 断言库内 canonical_answer / 错步 / 错因 无任何 LaTeX 残留、已是 Unicode 规范形。
// RED 条件：任何人移除 solve_adapter.go 的 NormalizeMathText（stripReports / gradeOutcome
// 边界）即红。

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

func TestINV019_CanonicalAnswerStoredNormalized_NoLatexResidue(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // 与生产「写路径单写连接」同构（§6.15）
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	reg := scenario.NewRegistry()
	constraint := k12.NewCurriculumStub()
	if err := reg.Assemble(k12.Pack(constraint)); err != nil {
		t.Fatal(err)
	}
	store := k12storage.NewStore(db, reg.Records)

	// fake 模型输出 LaTeX（真机取证过：模型会违反提示词的 Unicode 约束，BUG-20260712-U）。
	adapterUnderTest := NewSolveAdapter(&fakeExec{
		solveResult: &skill.Result{
			Content: `解：长方体体积 \( V = l \times w \times h \)，` +
				`\[ V = 3.8 \, \text{cm} \times 3 \, \text{cm} \times 1 \, \text{cm} \]，答案 11.4 \text{cm}^3`,
			Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"},
		},
		gradeResult: &skill.Result{
			Metadata: map[string]string{
				"grade_correct":       "false",
				"grade_wrong_step":    `第二步 \( 3.8 \times 3 \) 误算`,
				"grade_misconception": `把 \frac{1}{2} 当成了 0.2`,
			},
		},
	})

	deps := usecase.Deps{
		Solver:     adapterUnderTest,
		Grader:     adapterUnderTest,
		Records:    store,
		Constraint: constraint,
		Now:        func() int64 { return 1000 },
	}
	res, err := deps.GradeHomeworkProblem(ctx, usecase.GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "s-inv019",
		Problem: "3.8 × 3 = ?", StudentAnswer: "10.4", KnowledgePoints: []string{"小数乘法"},
	})
	if err != nil {
		t.Fatalf("批改闭环: %v", err)
	}
	if !res.RecordCreated {
		t.Fatal("判错应入库错题")
	}

	recs, err := store.ListByScope(ctx, "mingming", k12.CollectionMistakes, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("应恰好 1 条错题, got %d", len(recs))
	}
	f, err := k12.ParseMistakeFields(recs[0].Fields)
	if err != nil {
		t.Fatal(err)
	}
	if f.CanonicalAnswer == "" {
		t.Fatal("判错入库应携带已验算 canonical_answer（§3.8 治本①）")
	}
	assertNoLatex(t, "库内 canonical_answer", f.CanonicalAnswer)
	assertNoLatex(t, "库内 wrong_process", f.WrongProcess)
	assertNoLatex(t, "库内 error_cause", f.ErrorCause)
	if !strings.Contains(f.CanonicalAnswer, "×") || !strings.Contains(f.CanonicalAnswer, "cm³") {
		t.Errorf("canonical_answer 应为 Unicode 规范形（× / cm³），got %q", f.CanonicalAnswer)
	}
}
