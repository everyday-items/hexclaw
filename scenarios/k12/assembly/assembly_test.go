package assembly

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// fakeSolveExec 模拟 engine 的 solve skill（Execute 边界）。真实现走 LLM，单测在此边界打桩。
type fakeSolveExec struct{}

func (fakeSolveExec) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	if _, grading := args["student_answer"]; grading {
		return &skill.Result{
			Content: "批改：第一步小数点错位",
			Metadata: map[string]string{
				"solve_mode": "grading", "solve_verdict": "agree", "solve_evidence": "numeric_exec",
				"grade_correct": "false", "grade_wrong_step": "3.8×3 误算为 10.4",
				"grade_misconception": "小数点错位",
			},
		}, nil
	}
	return &skill.Result{
		Content:  "解：3.8×3=11.4\n\n```hexclaw-subagents\n[]\n```",
		Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"},
	}, nil
}

type capturedInsights struct{ n int }

func (c *capturedInsights) WriteWeakness(context.Context, string, string, string) error {
	c.n++
	return nil
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatalf("agent: %v", err)
	}
	return db
}

// TestWire_RealAdapterClosedLoop 用真 SolveAdapter（over fake executor）跑通整条业务闭环。
func TestWire_RealAdapterClosedLoop(t *testing.T) {
	db := newDB(t)
	ins := &capturedInsights{}
	k, err := Wire(db, fakeSolveExec{}, WithInsights(ins))
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	if k.Deps.VerifiedGrader == nil {
		t.Fatal("production wiring must explicitly retain the verified-solution grader fast path")
	}
	ctx := context.Background()

	// 六缝装配：错题本 schema 已注册
	if _, err := k.Registry.Records.Get(k12.CollectionMistakes); err != nil {
		t.Fatalf("错题本 schema 应已注册: %v", err)
	}

	// 批改一道答错的题 → 走真 adapter（solve→grade）→ 入库 + 学情
	res, err := k.Deps.GradeHomeworkProblem(ctx, usecase.GradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "s1",
		Problem: "3.8×3=?", StudentAnswer: "10.4", KnowledgePoints: []string{"小数乘法"},
	})
	if err != nil {
		t.Fatalf("闭环: %v", err)
	}
	if !res.RecordCreated {
		t.Fatal("答错应入库")
	}
	if res.Evidence.Badge() != "verified-strong" {
		t.Errorf("code_exec 一致应强徽章, got %q", res.Evidence.Badge())
	}
	// 学情信号经 Transactional Outbox 投影（§6.9）：显式补投 pending（生产由 Outbox.Start 驱动）。
	if err := k.Outbox.ProcessPending(ctx); err != nil {
		t.Fatalf("outbox 投递: %v", err)
	}
	if ins.n != 1 {
		t.Errorf("应写 1 条学情, got %d", ins.n)
	}
	// 知识点从识题回填（grader 不产 KP）
	rec, _ := k.Records.Get(ctx, res.RecordID)
	f, _ := k12.ParseMistakeFields(rec.Fields)
	if f.KnowledgePoint != "小数乘法" {
		t.Errorf("知识点应从识题回填为『小数乘法』, got %q", f.KnowledgePoint)
	}

	// 错题已进错题本
	book, _ := k.Records.ListByScope(ctx, "mingming", k12.CollectionMistakes, "")
	if len(book) != 1 {
		t.Errorf("错题本应有 1 条, got %d", len(book))
	}
	// 但刚录入的错题到期在明天（now+间隔），今天的复习队列应为空（间隔复习正确语义）
	if q, _ := k.Deps.ReviewQueue(ctx, "mingming"); len(q) != 0 {
		t.Errorf("新错题今天不该进复习队列, got %d", len(q))
	}
}

func TestWire_NilGuards(t *testing.T) {
	if _, err := Wire(nil, fakeSolveExec{}); err == nil {
		t.Error("db=nil 应报错")
	}
	db := newDB(t)
	if _, err := Wire(db, nil); err == nil {
		t.Error("solveSkill=nil 应报错")
	}
}
