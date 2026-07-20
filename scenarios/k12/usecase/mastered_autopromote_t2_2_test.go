package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// newClockDeps 建带可变时钟的 Deps（T2.2 需推进时间验证「≥3天二次做对」）。
func newClockDeps(t *testing.T, clock *int64) (usecase.Deps, *toggleGrader) {
	t.Helper()
	db := openMigratedTestDB(t)
	db.Exec(`INSERT INTO agents(name) VALUES('xiaoming')`)
	cur := curriculum.New()
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(cur)); err != nil {
		t.Fatal(err)
	}
	g := &toggleGrader{}
	return usecase.Deps{
		Solver: toggleSolver{}, Grader: g, Records: k12storage.NewStore(db, reg.Records),
		Constraint: cur, Now: func() int64 { return *clock },
	}, g
}

// T2.2（hex-test 审计 · PRD §5.3.1）：距上次 ≥3 天的第二次做对 → mastered（连对两次才算掌握，
// 隔天遗忘是常态）。原 MarkRetried 恒置 retried，从不自动升 mastered。
func TestT2_2_SecondCorrectAfter3DaysMasters(t *testing.T) {
	ctx := context.Background()
	clock := int64(1_000_000)
	d, g := newClockDeps(t, &clock)
	req := usecase.GradeRequest{
		AgentName: "xiaoming", Grade: "五年级上", SourceSession: "s1",
		Problem: "3.8×3=?", StudentAnswer: "11.6", KnowledgePoints: []string{"小数乘法"},
	}

	// 判错入库 new。
	if _, err := d.GradeHomeworkProblem(ctx, req); err != nil {
		t.Fatal(err)
	}
	// 第一次做对 → retried。
	g.correct = true
	if _, err := d.GradeHomeworkProblem(ctx, req); err != nil {
		t.Fatal(err)
	}
	recs, _ := d.Records.ListByScope(ctx, "xiaoming", k12.CollectionMistakes, "")
	if recs[0].Status != k12.StatusRetried {
		t.Fatalf("第一次做对应 retried, got %q", recs[0].Status)
	}

	// 推进 3 天后第二次做对 → mastered。
	clock += 3 * 86400
	if _, err := d.GradeHomeworkProblem(ctx, req); err != nil {
		t.Fatal(err)
	}
	recs, _ = d.Records.ListByScope(ctx, "xiaoming", k12.CollectionMistakes, "")
	if recs[0].Status != k12.StatusMastered {
		t.Errorf("≥3天第二次做对应 mastered, got %q", recs[0].Status)
	}
}

// 不足 3 天的第二次做对不升 mastered（隔天遗忘防线）。
func TestT2_2_SecondCorrectWithin3DaysStaysRetried(t *testing.T) {
	ctx := context.Background()
	clock := int64(1_000_000)
	d, g := newClockDeps(t, &clock)
	req := usecase.GradeRequest{
		AgentName: "xiaoming", Grade: "五年级上", SourceSession: "s1",
		Problem: "3.8×3=?", StudentAnswer: "11.6", KnowledgePoints: []string{"小数乘法"},
	}
	d.GradeHomeworkProblem(ctx, req)
	g.correct = true
	d.GradeHomeworkProblem(ctx, req)
	clock += 1 * 86400 // 仅 1 天
	d.GradeHomeworkProblem(ctx, req)
	recs, _ := d.Records.ListByScope(ctx, "xiaoming", k12.CollectionMistakes, "")
	if recs[0].Status != k12.StatusRetried {
		t.Errorf("不足3天第二次做对应仍 retried, got %q", recs[0].Status)
	}
}
