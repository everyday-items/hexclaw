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

// e2eSolver / e2eGrader：记录收到的 constraint，判错以触发入库。
type e2eSolver struct{ gotGrade, gotConstraint string }

func (s *e2eSolver) Solve(_ context.Context, _, grade, constraint string) (usecase.SolveResult, error) {
	s.gotGrade, s.gotConstraint = grade, constraint
	return usecase.SolveResult{Solution: "解"}, nil
}

type e2eGrader struct{}

func (e2eGrader) Grade(context.Context, string, string, string) (usecase.GradeOutcome, error) {
	return usecase.GradeOutcome{Correct: false, WrongStep: "第一步", ErrorCause: "计算失误"}, nil
}

func newRealDeps(t *testing.T) (usecase.Deps, *e2eSolver) {
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
	cur := curriculum.New() // 真人教版课标（含本轮补的 5 下）
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(cur)); err != nil {
		t.Fatal(err)
	}
	sv := &e2eSolver{}
	return usecase.Deps{
		Solver: sv, Grader: e2eGrader{}, Records: records.NewStore(db, reg.Records),
		Constraint: cur, Now: func() int64 { return 1000 },
	}, sv
}

// 五年级下真课标端到端：五下题 → 约束串含五下白名单，正常批改入库。
func TestGrade5B_E2E_InScope(t *testing.T) {
	d, sv := newRealDeps(t)
	res, err := d.GradeHomeworkProblem(context.Background(), usecase.GradeRequest{
		AgentName: "xiaoming", Grade: "五年级下", SourceSession: "s1",
		Problem: "1/2 + 1/3 = ?", StudentAnswer: "2/5", KnowledgePoints: []string{"异分母分数加减法"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OutOfScope {
		t.Fatal("五下做五下题不应超纲")
	}
	if !res.RecordCreated {
		t.Error("判错应入库")
	}
	// 关键：grade + 五下白名单约束真的透传给了 solver（Critical 修复端到端验证）。
	if sv.gotGrade != "五年级下" {
		t.Errorf("solver 应收到 grade 五年级下, got %q", sv.gotGrade)
	}
	if sv.gotConstraint == "" {
		t.Error("solver 应收到五下已学方法白名单约束（非空）")
	}
}

// 五年级下真课标端到端：六年级知识点 → 超纲错发反问，不批改不入库。
func TestGrade5B_E2E_OutOfScope(t *testing.T) {
	d, _ := newRealDeps(t)
	res, err := d.GradeHomeworkProblem(context.Background(), usecase.GradeRequest{
		AgentName: "xiaoming", Grade: "五年级下", SourceSession: "s2",
		Problem: "1/2 ÷ 1/3 = ?", StudentAnswer: "?", KnowledgePoints: []string{"分数除法"}, // 六上
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OutOfScope || res.OutOfScopeKP != "分数除法" {
		t.Fatalf("五下遇六上分数除法应超纲, got OOS=%v KP=%q", res.OutOfScope, res.OutOfScopeKP)
	}
	if res.RecordCreated {
		t.Error("超纲错发不应入库")
	}
}

// 冷启动：五下题的知识点 → 倒查推断五年级下建档。
func TestGrade5B_E2E_ColdStart(t *testing.T) {
	d, _ := newRealDeps(t)
	d.Profiles = newMemProfilesE2E()
	res, err := d.ColdStartProvision(context.Background(), "xiaoming", "小明", []string{"通分", "异分母分数加减法"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Inferred || res.Grade != "五年级下" {
		t.Errorf("五下知识点应推断五年级下, got %+v", res)
	}
}

type memProfilesE2E struct{ m map[string]k12.ChildProfile }

func newMemProfilesE2E() *memProfilesE2E { return &memProfilesE2E{m: map[string]k12.ChildProfile{}} }
func (p *memProfilesE2E) GetProfile(_ context.Context, a string) (k12.ChildProfile, error) {
	return p.m[a], nil
}
func (p *memProfilesE2E) SaveProfile(_ context.Context, a string, pr k12.ChildProfile) error {
	p.m[a] = pr
	return nil
}
