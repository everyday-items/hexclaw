package usecase

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// --- fakes（engine 能力的替身，用例层可脱离真 engine 测全闭环）---

type fakeSolver struct {
	solution string
	ev       SolveEvidence
}

func (f fakeSolver) Solve(context.Context, string, string, string) (SolveResult, error) {
	return SolveResult{Solution: f.solution, Evidence: f.ev}, nil
}

type fakeGrader struct{ outcome GradeOutcome }

func (f fakeGrader) Grade(context.Context, string, string, string) (GradeOutcome, error) {
	return f.outcome, nil
}

type fakeInsights struct{ notes []string }

func (f *fakeInsights) WriteWeakness(_ context.Context, _, _, note string) error {
	f.notes = append(f.notes, note)
	return nil
}

func newPipeline(t *testing.T, solver Solver, grader Grader, ins Insights) (Deps, *k12storage.Store) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// :memory: 每条连接各是一个独立空库——编排器异步 goroutine 会触发连接池开第二条连接
	// （表全丢）。收敛单连接与生产“写路径单写连接”（§6.15）同构。
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, a := range []string{"mingming", "eval-agent"} {
		if _, err := db.Exec(`INSERT INTO agents(name) VALUES(?)`, a); err != nil {
			t.Fatalf("agent %s: %v", a, err)
		}
	}
	reg := scenario.NewRegistry()
	constraint := k12.NewCurriculumStub()
	if err := reg.Assemble(k12.Pack(constraint)); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	store := k12storage.NewStore(db, reg.Records)
	// Outbox 学情投影（§6.9）：harness 用同步投递器——域写提交即消费，
	// 既有「判错入库 → 学情信号」断言的时序语义保持不变。
	k12storage.NewSyncDispatcher(store, InsightsConsumer{Insights: ins})
	return Deps{
		Solver:     solver,
		Grader:     grader,
		Insights:   ins,
		Records:    store,
		Constraint: constraint,
		Now:        func() int64 { return 1000 },
	}, store
}

// TestClosedLoop_WrongAnswer 完整闭环：答错 → 解题验算 → 批改 → 错题入库 + 到期 + 学情。
func TestClosedLoop_WrongAnswer(t *testing.T) {
	ins := &fakeInsights{}
	d, store := newPipeline(t,
		fakeSolver{solution: "11.4", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec, SolverModel: "glm", VerifierModel: "gpt"}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictDisagree, WrongStep: "3.8×3 误算为 10.4", ErrorCause: "计算失误", KnowledgePoint: "小数乘法"}},
		ins,
	)
	ctx := context.Background()

	res, err := d.GradeHomeworkProblem(ctx, GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "s1",
		Problem: "3.8 × 3 = ?", StudentAnswer: "10.4", KnowledgePoints: []string{"小数乘法"},
	})
	if err != nil {
		t.Fatalf("闭环报错: %v", err)
	}
	// 错题应入库
	if !res.RecordCreated {
		t.Fatal("答错应入库错题")
	}
	// 证据强徽章（数值验算一致）
	if b := res.Evidence.Badge(); b != "verified-strong" {
		t.Errorf("数值验算应强徽章, got %q", b)
	}
	// 记录落库 + 首次复习到期 = now(1000)+86400
	rec, err := store.Get(ctx, res.RecordID)
	if err != nil {
		t.Fatalf("取记录: %v", err)
	}
	if rec.Status != k12.StatusNew {
		t.Errorf("入库状态应 new, got %q", rec.Status)
	}
	if rec.DueAt == nil || *rec.DueAt != 1000+FirstReviewInterval {
		t.Errorf("到期应 %d, got %v", 1000+FirstReviewInterval, rec.DueAt)
	}
	// 学情信号已写
	if len(ins.notes) != 1 {
		t.Errorf("应写 1 条学情薄弱信号, got %d", len(ins.notes))
	}

	// 幂等：同题再判错一次 → 不重复入库
	res2, _ := d.GradeHomeworkProblem(ctx, GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "s1",
		Problem: "3.8×3 = ?", StudentAnswer: "10.4", KnowledgePoints: []string{"小数乘法"},
	})
	if res2.RecordCreated {
		t.Error("同题再错应幂等去重，不重复入库")
	}
}

// TestClosedLoop_CorrectAnswer 答对 → 无副作用（不入库、不写学情）。
func TestClosedLoop_CorrectAnswer(t *testing.T) {
	ins := &fakeInsights{}
	d, store := newPipeline(t,
		fakeSolver{solution: "11.4", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		ins,
	)
	ctx := context.Background()
	res, err := d.GradeHomeworkProblem(ctx, GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "s2", Problem: "1+1=?", StudentAnswer: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RecordCreated {
		t.Error("答对不应入库")
	}
	if got, _ := store.ListByScope(ctx, "mingming", k12.CollectionMistakes, ""); len(got) != 0 {
		t.Errorf("答对错题本应为空, got %d", len(got))
	}
	if len(ins.notes) != 0 {
		t.Error("答对不应写学情")
	}
}

// TestClosedLoop_OutOfScope 超纲知识点 → 错发反问，不解题不批改不入库。
func TestClosedLoop_OutOfScope(t *testing.T) {
	ins := &fakeInsights{}
	// solver/grader 若被调用会 panic（证明超纲短路在它们之前）
	d, store := newPipeline(t, panicSolver{}, panicGrader{}, ins)
	ctx := context.Background()
	res, err := d.GradeHomeworkProblem(ctx, GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "s3",
		Problem: "解方程组 ...", KnowledgePoints: []string{"解方程组"}, // 首学初一下 > 五年级上
	})
	if err != nil {
		t.Fatalf("超纲不应报错: %v", err)
	}
	if !res.OutOfScope || res.OutOfScopeKP != "解方程组" {
		t.Fatalf("应识别超纲知识点『解方程组』, got %+v", res)
	}
	if res.RecordCreated {
		t.Error("超纲不应入库")
	}
	if got, _ := store.ListByScope(ctx, "mingming", k12.CollectionMistakes, ""); len(got) != 0 {
		t.Error("超纲错题本应为空")
	}
}

type panicSolver struct{}

func (panicSolver) Solve(context.Context, string, string, string) (SolveResult, error) {
	panic("solver 不应在超纲短路后被调用")
}

type panicGrader struct{}

func (panicGrader) Grade(context.Context, string, string, string) (GradeOutcome, error) {
	panic("grader 不应在超纲短路后被调用")
}
