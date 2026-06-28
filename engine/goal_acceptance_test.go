package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGoalValidate(t *testing.T) {
	if err := (&Goal{}).Validate(); err == nil {
		t.Fatal("空 intent 应报错")
	}
	g := &Goal{Intent: "整理资料", Acceptance: []AcceptanceSpec{{Kind: AcceptContains, Value: "完成"}}}
	if err := g.Validate(); err != nil {
		t.Fatalf("合法 goal 不应报错: %v", err)
	}
	bad := &Goal{Intent: "x", Acceptance: []AcceptanceSpec{{Kind: AcceptJSONField}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("json_field 缺 path 应报错")
	}
	badConf := &Goal{Intent: "x", Escalation: EscalationPolicy{MinConfidence: 1.5}}
	if err := badConf.Validate(); err == nil {
		t.Fatal("min_confidence 越界应报错")
	}
}

func TestGoalWithDefaults(t *testing.T) {
	g := Goal{Intent: "x"}.WithDefaults()
	if g.Budget.MaxRounds != goalDefaults.MaxRounds || g.Budget.MaxWall != goalDefaults.MaxWall {
		t.Fatalf("预算默认值未填: %+v", g.Budget)
	}
	if g.Escalation.NoProgressRounds != goalDefaults.NoProgressRounds {
		t.Fatalf("升级默认值未填: %+v", g.Escalation)
	}
	// 显式值不被覆盖
	g2 := Goal{Intent: "x", Budget: GoalBudget{MaxRounds: 9}}.WithDefaults()
	if g2.Budget.MaxRounds != 9 {
		t.Fatalf("显式 MaxRounds 被覆盖: %d", g2.Budget.MaxRounds)
	}
}

func TestGoalHasFormalAcceptance(t *testing.T) {
	if (&Goal{Intent: "x"}).HasFormalAcceptance() {
		t.Fatal("无断言不应判有形式化验收")
	}
	if !(&Goal{Intent: "x", Acceptance: []AcceptanceSpec{{Kind: AcceptContains, Value: "a"}}}).HasFormalAcceptance() {
		t.Fatal("有断言应判有形式化验收")
	}
}

func TestAcceptanceDeterministicKinds(t *testing.T) {
	e := NewAcceptanceEngine()
	ctx := context.Background()

	v := e.Evaluate(ctx, "任务完成，结果 OK", []AcceptanceSpec{{Kind: AcceptContains, Value: "完成"}})
	if !v.Passed || v.Score != 1 {
		t.Fatalf("contains 应过: %+v", v)
	}
	v = e.Evaluate(ctx, "结果 OK", []AcceptanceSpec{{Kind: AcceptContains, Value: "完成"}})
	if v.Passed {
		t.Fatalf("contains 应不过: %+v", v)
	}
	v = e.Evaluate(ctx, "status=200 done", []AcceptanceSpec{{Kind: AcceptRegex, Value: `status=\d{3}`}})
	if !v.Passed {
		t.Fatalf("regex 应过: %+v", v)
	}
	v = e.Evaluate(ctx, `前言 {"status":"success","n":3} 后记`, []AcceptanceSpec{{Kind: AcceptJSONField, Path: "status", Value: "success"}})
	if !v.Passed {
		t.Fatalf("json_field 应过: %+v", v)
	}
	v = e.Evaluate(ctx, `{"status":"error"}`, []AcceptanceSpec{{Kind: AcceptJSONField, Path: "status", Value: "success"}})
	if v.Passed {
		t.Fatalf("json_field 应不过: %+v", v)
	}
}

// fakeCmdRunner 按命令文本返回预设 exit code + stdout。
type fakeCmdRunner struct {
	code   int
	stdout string
	err    error
}

func (f fakeCmdRunner) Run(_ context.Context, _, _ string) (string, int, error) {
	return f.stdout, f.code, f.err
}

func TestAcceptanceCommand(t *testing.T) {
	ctx := context.Background()
	// 无 runner → inconclusive → 不过
	v := NewAcceptanceEngine().Evaluate(ctx, "x", []AcceptanceSpec{{Kind: AcceptCommand, Value: "go test ./..."}})
	if v.Passed {
		t.Fatalf("无命令执行器应 inconclusive 不过: %+v", v)
	}
	if !strings.Contains(v.Results[0].Detail, "inconclusive") {
		t.Fatalf("应标注 inconclusive: %s", v.Results[0].Detail)
	}
	// exit 0 → 过
	e := &AcceptanceEngine{CmdRunner: fakeCmdRunner{code: 0, stdout: "ok PASS"}}
	v = e.Evaluate(ctx, "x", []AcceptanceSpec{{Kind: AcceptCommand, Value: "true"}})
	if !v.Passed {
		t.Fatalf("exit0 应过: %+v", v)
	}
	// exit 非 0 → 不过
	e = &AcceptanceEngine{CmdRunner: fakeCmdRunner{code: 1, stdout: "FAIL"}}
	v = e.Evaluate(ctx, "x", []AcceptanceSpec{{Kind: AcceptCommand, Value: "false"}})
	if v.Passed {
		t.Fatalf("exit1 应不过: %+v", v)
	}
	// exit0 但 stdout 不匹配 Pattern → 不过
	e = &AcceptanceEngine{CmdRunner: fakeCmdRunner{code: 0, stdout: "0 failures"}}
	v = e.Evaluate(ctx, "x", []AcceptanceSpec{{Kind: AcceptCommand, Value: "go test", Pattern: `PASS`}})
	if v.Passed {
		t.Fatalf("stdout 不匹配应不过: %+v", v)
	}
}

func TestAcceptanceLLMRubric(t *testing.T) {
	ctx := context.Background()
	// 无 judge → inconclusive 不过
	v := NewAcceptanceEngine().Evaluate(ctx, "x", []AcceptanceSpec{{Kind: AcceptLLMRubric, Value: "是否专业", Threshold: 7}})
	if v.Passed {
		t.Fatalf("无裁判应 inconclusive 不过: %+v", v)
	}
	// 裁判给 8 分，阈值 7 → 过
	judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { return "8\n写得清楚", nil })
	e := &AcceptanceEngine{Judge: judge}
	v = e.Evaluate(ctx, "一篇结构清晰的稿子", []AcceptanceSpec{{Kind: AcceptLLMRubric, Value: "结构是否清晰", Threshold: 7}})
	if !v.Passed {
		t.Fatalf("rubric 8>=7 应过: %+v", v)
	}
	// 裁判给 5 分，阈值 7 → 不过
	judge = LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { return "5\n一般", nil })
	e = &AcceptanceEngine{Judge: judge}
	v = e.Evaluate(ctx, "x", []AcceptanceSpec{{Kind: AcceptLLMRubric, Value: "r", Threshold: 7}})
	if v.Passed {
		t.Fatalf("rubric 5<7 应不过: %+v", v)
	}
}

func TestAcceptanceAggregateAndOptional(t *testing.T) {
	ctx := context.Background()
	e := NewAcceptanceEngine()
	// 一条必过(过) + 一条可选(不过) → 整体 Passed=true，但 Score<1
	specs := []AcceptanceSpec{
		{Kind: AcceptContains, Value: "完成"},
		{Kind: AcceptContains, Value: "图表", Optional: true},
	}
	v := e.Evaluate(ctx, "任务完成", specs)
	if !v.Passed {
		t.Fatalf("可选断言不过不应阻止 Passed: %+v", v)
	}
	if v.Score >= 1.0 {
		t.Fatalf("可选断言不过应拉低 Score: %v", v.Score)
	}
	// 一条必过(不过) → 整体 Passed=false
	v = e.Evaluate(ctx, "还没好", []AcceptanceSpec{{Kind: AcceptContains, Value: "完成"}})
	if v.Passed {
		t.Fatalf("必过断言不过应整体不过: %+v", v)
	}
	// 空断言 → 不过 + 标注
	v = e.Evaluate(ctx, "x", nil)
	if v.Passed || !strings.Contains(v.Reason, "无形式化验收") {
		t.Fatalf("空断言应不过且标注: %+v", v)
	}
}

func TestExecCommandRunnerReal(t *testing.T) {
	r := NewExecCommandRunner(5 * time.Second)
	out, code, err := r.Run(context.Background(), "echo hello && exit 0", "")
	if err != nil || code != 0 || !strings.Contains(out, "hello") {
		t.Fatalf("真实命令执行异常: out=%q code=%d err=%v", out, code, err)
	}
	_, code, err = r.Run(context.Background(), "exit 3", "")
	if err != nil || code != 3 {
		t.Fatalf("非 0 退出应作为结果而非错误: code=%d err=%v", code, err)
	}
}
