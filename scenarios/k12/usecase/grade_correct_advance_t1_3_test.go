package usecase_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type toggleSolver struct{}

func (toggleSolver) Solve(_ context.Context, _, _, _ string) (usecase.SolveResult, error) {
	return usecase.SolveResult{Solution: "解"}, nil
}

type toggleGrader struct{ correct bool }

func (g *toggleGrader) Grade(context.Context, string, string, string) (usecase.GradeOutcome, error) {
	if g.correct {
		return usecase.GradeOutcome{Correct: true}, nil
	}
	return usecase.GradeOutcome{Correct: false, WrongStep: "第一步", ErrorCause: "计算失误", KnowledgePoint: "小数乘法"}, nil
}

func newToggleDeps(t *testing.T) (usecase.Deps, *toggleGrader) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('xiaoming')`)
	cur := curriculum.New()
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(cur)); err != nil {
		t.Fatal(err)
	}
	g := &toggleGrader{}
	return usecase.Deps{
		Solver: toggleSolver{}, Grader: g, Records: records.NewStore(db, reg.Records),
		Constraint: cur, Now: func() int64 { return 1000 },
	}, g
}

// T1.3（hex-test 审计）：答对时若同题已在错题本 → 状态推进（PRD §3.4.4-2 / 流程图:98
// 「对同题批改为对时推进 retried」）。原实现答对直接 return，不查不推进旧错题。
func TestT1_3_CorrectAdvancesExistingMistake(t *testing.T) {
	ctx := context.Background()
	d, g := newToggleDeps(t)
	req := usecase.GradeRequest{
		AgentName: "xiaoming", Grade: "五年级上", SourceSession: "s1",
		Problem: "3.8×3=?", StudentAnswer: "11.6", KnowledgePoints: []string{"小数乘法"},
	}

	// 第一次判错 → 入库 new。
	if _, err := d.GradeHomeworkProblem(ctx, req); err != nil {
		t.Fatal(err)
	}
	recs, _ := d.Records.ListByScope(ctx, "xiaoming", k12.CollectionMistakes, "")
	if len(recs) != 1 || recs[0].Status != k12.StatusNew {
		t.Fatalf("应入库 1 条 new, got %d 条 status=%v", len(recs), statusOf(recs))
	}

	// 同题再批改为对 → 该错题应推进 retried（孩子重做做对）。
	g.correct = true
	req.StudentAnswer = "11.4"
	if _, err := d.GradeHomeworkProblem(ctx, req); err != nil {
		t.Fatal(err)
	}
	recs, _ = d.Records.ListByScope(ctx, "xiaoming", k12.CollectionMistakes, "")
	if len(recs) != 1 {
		t.Fatalf("答对不新增记录, got %d 条", len(recs))
	}
	if recs[0].Status != k12.StatusRetried {
		t.Errorf("答对同题应推进 retried, got %q", recs[0].Status)
	}
}

func statusOf(recs []*records.AgentRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Status
	}
	return out
}
