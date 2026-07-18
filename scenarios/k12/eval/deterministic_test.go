package eval

// 常规门（不走 LLM）的确定性 eval：§5.7 六套件中可纯逻辑评测的部分——
//   套件 4 boundary：超纲判定/年级边界（生产 curriculum 词表 + k12.IsBeyond 为唯一真相）；
//   套件 5 product ：答案泄露检测（静态产物用例 + 真实打印卷产物接线检查）；
//   套件 6 redline ：作品反馈禁则拦截（生产 INV-011 拦截器为唯一真相）。
// 常规门只跑 dev 分割；holdout 只在发布评审（HEXCLAW_K12_EVAL=1 + SPLIT=holdout）运行。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// runBoundaryCases 套件 4 kind=boundary 的确定性执行器（dev/holdout 共用；返回套件结果）。
func runBoundaryCases(t *testing.T, split string) SuiteResult {
	t.Helper()
	s := loadSplit(t, Suites[3], split)
	cur := curriculum.New()
	res := SuiteResult{Suite: s.Suite, SuiteNo: s.SuiteNo, Mode: "deterministic"}
	for _, c := range s.Cases {
		if c.Kind != "boundary" {
			continue
		}
		var in BoundaryInput
		var exp BoundaryExpected
		mustUnmarshal(t, c, c.Input, &in)
		mustUnmarshal(t, c, c.Expected, &exp)
		res.Total++
		fg, ok := cur.FirstGrade(context.Background(), in.KnowledgePoint)
		if !ok {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: 知识点 %q 不在课标词表（fail-visible）", c.ID, in.KnowledgePoint))
			continue
		}
		if fg != exp.FirstGrade {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: 首学年级 got %s want %s", c.ID, fg, exp.FirstGrade))
			continue
		}
		if got := k12.IsBeyond(c.Grade, fg); got != exp.OutOfScope {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: 超纲判定 got %v want %v（grade=%s kp=%s）", c.ID, got, exp.OutOfScope, c.Grade, in.KnowledgePoint))
			continue
		}
		res.Passed++
	}
	finishRate(&res)
	return res
}

// runProductCases 套件 5 kind=product 的确定性执行器。
func runProductCases(t *testing.T, split string) SuiteResult {
	t.Helper()
	s := loadSplit(t, Suites[4], split)
	res := SuiteResult{Suite: s.Suite, SuiteNo: s.SuiteNo, Mode: "deterministic"}
	for _, c := range s.Cases {
		if c.Kind != "product" {
			continue
		}
		var in ProductInput
		var exp ProductExpected
		mustUnmarshal(t, c, c.Input, &in)
		mustUnmarshal(t, c, c.Expected, &exp)
		res.Total++
		if got := AnswerLeaked(in.Product, in.Answer); got != exp.Leak {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: 泄露判定 got %v want %v（%s）", c.ID, got, exp.Leak, in.ProductKind))
			continue
		}
		res.Passed++
	}
	finishRate(&res)
	return res
}

// runRedlineCases 套件 6 kind=redline 的确定性执行器（生产 INV-011 拦截器为唯一真相）。
func runRedlineCases(t *testing.T, split string) SuiteResult {
	t.Helper()
	s := loadSplit(t, Suites[5], split)
	res := SuiteResult{Suite: s.Suite, SuiteNo: s.SuiteNo, Mode: "deterministic"}
	for _, c := range s.Cases {
		if c.Kind != "redline" {
			continue
		}
		var in RedlineInput
		var exp RedlineExpected
		mustUnmarshal(t, c, c.Input, &in)
		mustUnmarshal(t, c, c.Expected, &exp)
		res.Total++
		reason := usecase.WorkFeedbackRedlineViolation(in.Feedback)
		if got := reason != ""; got != exp.Violation {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: 禁则判定 got %v(%s) want %v", c.ID, got, reason, exp.Violation))
			continue
		}
		res.Passed++
	}
	finishRate(&res)
	return res
}

func TestSuite4BoundaryDeterministic(t *testing.T) {
	res := runBoundaryCases(t, "dev")
	assertAllPassed(t, res)
}

func TestSuite5AnswerLeakDeterministic(t *testing.T) {
	res := runProductCases(t, "dev")
	assertAllPassed(t, res)
}

func TestSuite6RedlineDeterministic(t *testing.T) {
	res := runRedlineCases(t, "dev")
	assertAllPassed(t, res)
}

// TestSuite5RealPaperProductNoLeak 答案泄露的**接线**检查：真实走 CreatePracticeSet →
// FinalizeBasket 产出打印卷聚合，逐题断言家长打印面（QuestionMarkdown）不包含答案
// （题答分离是产品红线：题目卷只出题，答案供家长核对另行呈现）。
func TestSuite5RealPaperProductNoLeak(t *testing.T) {
	d := wireDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly,
		Title:      "eval 泄露接线卷",
		Items: []k12.PracticeItem{
			{ItemID: "e1", Subject: "数学", QuestionMarkdown: "3.8 × 0.65 = （　　）", ExpectedAnswerMarkdown: "2.47",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
			{ItemID: "e2", Subject: "数学", QuestionMarkdown: "解方程：3x + 7 = 25", ExpectedAnswerMarkdown: "x=6",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
		},
	}
	id, _, err := d.CreatePracticeSet(ctx, "eval-agent", "s", f)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := d.FinalizeBasket(ctx, "eval-agent", id, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range v.Fields.Items {
		if AnswerLeaked(it.QuestionMarkdown, it.ExpectedAnswerMarkdown) {
			t.Fatalf("打印卷题面泄露答案: item=%s question=%q answer=%q", it.ItemID, it.QuestionMarkdown, it.ExpectedAnswerMarkdown)
		}
	}
}

// ---------- 共用小工具 ----------

func loadSplit(t *testing.T, m SuiteMeta, split string) Suite {
	t.Helper()
	s, err := LoadSuite(SuitePath(m, split))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(m, split); err != nil {
		t.Fatal(err)
	}
	return s
}

func mustUnmarshal(t *testing.T, c Case, raw json.RawMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("case %s: %v", c.ID, err)
	}
}

func finishRate(res *SuiteResult) {
	if res.Total > 0 {
		res.PassRate = float64(res.Passed) / float64(res.Total)
	}
}

func assertAllPassed(t *testing.T, res SuiteResult) {
	t.Helper()
	if res.Total == 0 {
		t.Fatalf("套件 %s 无确定性用例", res.Suite)
	}
	if len(res.Failures) > 0 || res.Passed != res.Total {
		t.Fatalf("套件 %s 确定性 eval %d/%d 通过，失败：%v", res.Suite, res.Passed, res.Total, res.Failures)
	}
}

// evalStubSolve 装配所需的 SolveExecutor：确定性链路绝不触发 solve，一旦被调用即失败取证。
type evalStubSolve struct{}

func (evalStubSolve) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return nil, fmt.Errorf("eval deterministic path: solve executor must not be invoked")
}

// wireDataDeps 用内存 sqlite 装配真实用例依赖（迁移 + agent 种子），只走数据路径。
func wireDataDeps(t *testing.T) usecase.Deps {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('eval-agent')`); err != nil {
		t.Fatal(err)
	}
	k, err := assembly.Wire(db, evalStubSolve{})
	if err != nil {
		t.Fatal(err)
	}
	return k.Deps
}
