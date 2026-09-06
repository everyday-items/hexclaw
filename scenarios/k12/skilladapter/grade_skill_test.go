package skilladapter_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/skilladapter"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type fakeSolver struct{ sol string }

func (f fakeSolver) Solve(context.Context, string, string, string) (usecase.SolveResult, error) {
	return usecase.SolveResult{Solution: f.sol}, nil
}

type fakeGrader struct{ oc usecase.GradeOutcome }

func (f fakeGrader) Grade(context.Context, string, string, string) (usecase.GradeOutcome, error) {
	return f.oc, nil
}

func newDeps(t *testing.T, oc usecase.GradeOutcome) usecase.Deps {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		t.Fatal(err)
	}
	return usecase.Deps{
		Solver:  fakeSolver{sol: "解：11.4"},
		Grader:  fakeGrader{oc: oc},
		Records: k12storage.NewStore(db, reg.Records),
		Now:     func() int64 { return 1000 },
	}
}

func TestGradeSkill_ScopesToRoutedAgentAndLogsMistake(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{Verdict: usecase.VerdictDisagree, WrongStep: "小数点错位", ErrorCause: "计算失误", KnowledgePoint: "小数乘法"})
	sk := skilladapter.NewGradeSkill(deps)

	// ctx 带已路由 Agent（engine 会 stamp）→ 入库 scope 到 mingming。
	ctx := skill.WithRoutedAgent(context.Background(), "mingming")
	res, err := sk.Execute(ctx, map[string]any{
		"problem": "3.8×3", "student_answer": "11.6", "grade": "五年级上",
		"knowledge_points": []any{"小数乘法"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "第一个错步") || !strings.Contains(res.Content, "已记入错题本") {
		t.Errorf("批改内容应含错步 + 入库提示: %q", res.Content)
	}
	if !strings.Contains(res.Content, "解：11.4") || strings.Contains(res.Content, "别直接给正确答案") {
		t.Errorf("家长批改结果必须保留完整解法: %q", res.Content)
	}
	if res.Metadata["k12_record_created"] != "true" {
		t.Errorf("应入库, meta=%v", res.Metadata)
	}
	// 确认真落库到 mingming scope。
	recs, err := deps.Records.ListByScope(context.Background(), "mingming", k12.CollectionMistakes, "")
	if err != nil || len(recs) != 1 {
		t.Fatalf("mingming 错题本应有 1 条, got %d err=%v", len(recs), err)
	}
}

func TestGradeSkill_CorrectAnswerNoMistake(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{Verdict: usecase.VerdictAgree})
	sk := skilladapter.NewGradeSkill(deps)
	ctx := skill.WithRoutedAgent(context.Background(), "mingming")
	res, err := sk.Execute(ctx, map[string]any{"problem": "1+1", "student_answer": "2", "grade": "一年级上"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "答对") {
		t.Errorf("答对应无入库, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "解：11.4") {
		t.Errorf("答对也应保留家长参考解法: %q", res.Content)
	}
	recs, _ := deps.Records.ListByScope(context.Background(), "mingming", k12.CollectionMistakes, "")
	if len(recs) != 0 {
		t.Errorf("答对不入库, got %d", len(recs))
	}
}

func TestGradeSkill_NoAgentErrors(t *testing.T) {
	sk := skilladapter.NewGradeSkill(newDeps(t, usecase.GradeOutcome{}))
	// 无 ctx agent 且无 args agent → 拒绝（不猜实例）。
	_, err := sk.Execute(context.Background(), map[string]any{"problem": "x", "student_answer": "y"})
	if err == nil {
		t.Error("无法确定实例应报错")
	}
}

func TestGradeSkill_MatchIsFalse(t *testing.T) {
	sk := skilladapter.NewGradeSkill(usecase.Deps{})
	if sk.Match("批改一下") {
		t.Error("k12_grade 只经 LLM 工具调用，Match 应恒 false")
	}
	if sk.Name() != "k12_grade" {
		t.Error("工具名应 k12_grade")
	}
}
