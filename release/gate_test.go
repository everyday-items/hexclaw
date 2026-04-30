package release

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGate_RunAllPassReturnsNoError(t *testing.T) {
	g := &Gate{Checks: []Check{
		FuncCheck{N: "a", Fn: func(_ context.Context) Result { return Result{Status: StatusPass} }},
		FuncCheck{N: "b", Fn: func(_ context.Context) Result { return Result{Status: StatusPass} }},
	}}
	rep, err := g.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.PassCount != 2 || rep.FailCount != 0 {
		t.Errorf("counts wrong: %+v", rep)
	}
}

func TestGate_FailCountReportsError(t *testing.T) {
	g := &Gate{Checks: []Check{
		FuncCheck{N: "a", Fn: func(_ context.Context) Result { return Result{Status: StatusPass} }},
		FuncCheck{N: "b", Fn: func(_ context.Context) Result { return Result{Status: StatusFail, Message: "broken"} }},
	}}
	rep, err := g.Run(context.Background())
	if err == nil {
		t.Fatal("any FAIL 应返回 error")
	}
	var gf *GateFailure
	if !errors.As(err, &gf) {
		t.Errorf("应返回 *GateFailure；got %T", err)
	}
	if rep.FailCount != 1 {
		t.Errorf("FailCount wrong: %d", rep.FailCount)
	}
	if !strings.Contains(err.Error(), "b") {
		t.Errorf("error 应携带失败 check 名；got %v", err)
	}
}

func TestGate_FailFastShortCircuits(t *testing.T) {
	calls := 0
	g := &Gate{
		FailFast: true,
		Checks: []Check{
			FuncCheck{N: "a", Fn: func(_ context.Context) Result {
				calls++
				return Result{Status: StatusFail}
			}},
			FuncCheck{N: "b", Fn: func(_ context.Context) Result {
				calls++
				return Result{Status: StatusPass}
			}},
		},
	}
	rep, err := g.Run(context.Background())
	if err == nil {
		t.Fatal("FAIL 应返回 error")
	}
	if calls != 1 {
		t.Errorf("FailFast 应在第 1 次失败时短路；got %d 次调用", calls)
	}
	if !rep.Aborted {
		t.Error("Aborted 应为 true")
	}
}

func TestGate_CollectAllRunsEverything(t *testing.T) {
	calls := 0
	g := &Gate{
		FailFast: false,
		Checks: []Check{
			FuncCheck{N: "a", Fn: func(_ context.Context) Result {
				calls++
				return Result{Status: StatusFail}
			}},
			FuncCheck{N: "b", Fn: func(_ context.Context) Result {
				calls++
				return Result{Status: StatusPass}
			}},
		},
	}
	_, err := g.Run(context.Background())
	if err == nil {
		t.Fatal("应返回错")
	}
	if calls != 2 {
		t.Errorf("collect-all 应跑完所有；got %d", calls)
	}
}

func TestGate_StatusCounts(t *testing.T) {
	g := &Gate{Checks: []Check{
		FuncCheck{N: "a", Fn: func(_ context.Context) Result { return Result{Status: StatusPass} }},
		FuncCheck{N: "b", Fn: func(_ context.Context) Result { return Result{Status: StatusWarn} }},
		FuncCheck{N: "c", Fn: func(_ context.Context) Result { return Result{Status: StatusSkip} }},
		FuncCheck{N: "d", Fn: func(_ context.Context) Result { return Result{Status: StatusPass} }},
	}}
	rep, err := g.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.PassCount != 2 || rep.WarnCount != 1 || rep.SkipCount != 1 || rep.FailCount != 0 {
		t.Errorf("counts wrong: %+v", rep)
	}
}

func TestGate_DurationFilled(t *testing.T) {
	g := &Gate{Checks: []Check{
		FuncCheck{N: "slow", Fn: func(_ context.Context) Result {
			time.Sleep(10 * time.Millisecond)
			return Result{Status: StatusPass}
		}},
	}}
	rep, _ := g.Run(context.Background())
	if rep.Duration < 10*time.Millisecond {
		t.Errorf("Duration 应被填充；got %v", rep.Duration)
	}
	if rep.Results[0].Result.Duration < 10*time.Millisecond {
		t.Errorf("Result.Duration 应被填充")
	}
}

func TestGate_ContextCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &Gate{Checks: []Check{
		FuncCheck{N: "a", Fn: func(_ context.Context) Result { return Result{Status: StatusPass} }},
	}}
	_, err := g.Run(ctx)
	if err == nil {
		t.Error("已取消 ctx 应返回 error")
	}
}

func TestFuncCheck_NoFnSkips(t *testing.T) {
	c := FuncCheck{N: "x"}
	res := c.Run(context.Background())
	if res.Status != StatusSkip {
		t.Errorf("nil fn 应 Skip；got %s", res.Status)
	}
}

func TestDefault10_HasTenChecks(t *testing.T) {
	checks := Default10()
	if len(checks) != 10 {
		t.Errorf("Default10 应有 10 项；got %d", len(checks))
	}
	expected := []string{"tests-pass", "tests-coverage", "lint-clean", "docs-current",
		"version-bump", "signatures-valid", "migration-ready", "config-validated",
		"sbom-fresh", "flag-stage-audit"}
	for i, name := range expected {
		if checks[i].Name() != name {
			t.Errorf("Default10[%d].Name = %q, want %q", i, checks[i].Name(), name)
		}
	}
}

func TestDefault10_AllSkipByDefault(t *testing.T) {
	g := &Gate{Checks: Default10()}
	rep, err := g.Run(context.Background())
	if err != nil {
		t.Fatalf("Default10 不应失败（全部 Skip）；got %v", err)
	}
	if rep.SkipCount != 10 {
		t.Errorf("应全部 Skip；got %+v", rep)
	}
}

func TestGateFailure_ErrorIncludesNames(t *testing.T) {
	rep := &Report{
		Results: []NamedResult{
			{Name: "a", Result: Result{Status: StatusFail}},
			{Name: "b", Result: Result{Status: StatusPass}},
			{Name: "c", Result: Result{Status: StatusFail}},
		},
		FailCount: 2,
	}
	gf := &GateFailure{Report: rep}
	if !strings.Contains(gf.Error(), "a") || !strings.Contains(gf.Error(), "c") {
		t.Errorf("error 应列出所有失败 check；got %s", gf.Error())
	}
}

func TestGate_NilGate(t *testing.T) {
	var g *Gate
	if _, err := g.Run(context.Background()); err == nil {
		t.Error("nil gate 应报错")
	}
}
