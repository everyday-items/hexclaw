package engine

import (
	"context"
	"testing"
	"time"
)

// UT-ACC-10: 嵌套 JSON 字段 + 数字字段 + 缺字段
func TestAcceptanceNestedJSONField(t *testing.T) {
	e := NewAcceptanceEngine()
	ctx := context.Background()
	out := `结果 {"meta":{"status":"ok","count":3}} 完`
	if v := e.Evaluate(ctx, out, []AcceptanceSpec{{Kind: AcceptJSONField, Path: "meta.status", Value: "ok"}}); !v.Passed {
		t.Fatalf("嵌套 path 应过: %+v", v)
	}
	if v := e.Evaluate(ctx, out, []AcceptanceSpec{{Kind: AcceptJSONField, Path: "meta.count", Value: "3"}}); !v.Passed {
		t.Fatalf("数字字段应过: %+v", v)
	}
	if v := e.Evaluate(ctx, out, []AcceptanceSpec{{Kind: AcceptJSONField, Path: "meta.missing", Value: "x"}}); v.Passed {
		t.Fatalf("缺字段应不过")
	}
}

// UT-ACC-11: 加权得分——可选项不过拉低 Score 但不阻 Passed
func TestAcceptanceWeightedScore(t *testing.T) {
	e := NewAcceptanceEngine()
	ctx := context.Background()
	specs := []AcceptanceSpec{
		{Kind: AcceptContains, Value: "A", Weight: 1},
		{Kind: AcceptContains, Value: "B", Weight: 3, Optional: true},
	}
	v := e.Evaluate(ctx, "only A here", specs)
	if !v.Passed {
		t.Fatalf("可选项不过不应阻 Passed: %+v", v)
	}
	if v.Score < 0.24 || v.Score > 0.26 {
		t.Fatalf("加权得分应≈0.25(1/(1+3)): %v", v.Score)
	}
}

// UT-ACC-12: 真实命令执行器 + stdout 正则断言
func TestAcceptanceCommandRealRunnerPattern(t *testing.T) {
	e := &AcceptanceEngine{CmdRunner: NewExecCommandRunner(5 * time.Second)}
	ctx := context.Background()
	if v := e.Evaluate(ctx, "x", []AcceptanceSpec{{Kind: AcceptCommand, Value: `echo "PASS: 0 failures"`, Pattern: `PASS`}}); !v.Passed {
		t.Fatalf("真实命令 stdout 匹配应过: %+v", v)
	}
	if v := e.Evaluate(ctx, "x", []AcceptanceSpec{{Kind: AcceptCommand, Value: `echo nope`, Pattern: `PASS`}}); v.Passed {
		t.Fatalf("stdout 不匹配应不过: %+v", v)
	}
}

// UT-ACC-13: 非法正则不致 panic、判不过
func TestAcceptanceInvalidRegex(t *testing.T) {
	v := NewAcceptanceEngine().Evaluate(context.Background(), "x", []AcceptanceSpec{{Kind: AcceptRegex, Value: "(unclosed"}})
	if v.Passed {
		t.Fatalf("非法正则应不过: %+v", v)
	}
}
