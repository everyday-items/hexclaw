package eval

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func withFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{FlagEvalFrameworkV1: on})
	return featureflag.WithContext(ctx, flags)
}

func TestSuite_FlagOff(t *testing.T) {
	s := V04Suite()
	if _, err := s.Run(context.Background()); !errors.Is(err, ErrEvalDisabled) {
		t.Errorf("flag OFF 应返回 ErrEvalDisabled；got %v", err)
	}
}

func TestV04Suite_HasTenCases(t *testing.T) {
	s := V04Suite()
	if len(s.Cases) != 10 {
		t.Errorf("应有 10 条 evalcase；got %d", len(s.Cases))
	}
}

func TestV04Suite_AllPassWithFakeRun(t *testing.T) {
	ctx := withFlag(context.Background(), true)
	rep, err := V04Suite().Run(ctx)
	if err != nil {
		t.Fatalf("默认 fake 实现应全 PASS；got %v", err)
	}
	if rep.Total != 10 || rep.Passed != 10 || rep.Failed != 0 {
		t.Errorf("counts wrong: %+v", rep)
	}
}

func TestAssertions_PassFail(t *testing.T) {
	if err := AssertContent("hi")(Output{Content: "hi there"}); err != nil {
		t.Error("expected pass")
	}
	if err := AssertContent("missing")(Output{Content: "x"}); err == nil {
		t.Error("expected fail")
	}
	if err := AssertNoError()(Output{}); err != nil {
		t.Error("nil err 应通过")
	}
	if err := AssertNoError()(Output{Error: errors.New("boom")}); err == nil {
		t.Error("有 err 应失败")
	}
	if err := AssertError("rate")(Output{Error: errors.New("rate limit")}); err != nil {
		t.Error("含 substr 应通过")
	}
	if err := AssertError("missing")(Output{}); err == nil {
		t.Error("无 err 应失败")
	}
	if err := AssertEventEmitted("x")(Output{Events: []string{"x"}}); err != nil {
		t.Error("expected pass")
	}
	if err := AssertEventEmitted("y")(Output{Events: []string{"x"}}); err == nil {
		t.Error("缺事件应失败")
	}
	if err := AssertCostBelow(1)(Output{Cost: 0.5}); err != nil {
		t.Error("expected pass")
	}
	if err := AssertCostBelow(0.1)(Output{Cost: 1}); err == nil {
		t.Error("超额应失败")
	}
	if err := AssertToolCalled("x")(Output{ToolCalls: []string{"x"}}); err != nil {
		t.Error("expected pass")
	}
	if err := AssertToolCalled("y")(Output{}); err == nil {
		t.Error("缺 tool 应失败")
	}
}

func TestSuite_FailReportsCounts(t *testing.T) {
	ctx := withFlag(context.Background(), true)
	s := &Suite{Cases: []EvalCase{
		{ID: "a", Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{}, nil
		}, Assertions: []Assertion{AssertContent("missing")}},
	}}
	rep, err := s.Run(ctx)
	if err == nil {
		t.Fatal("应失败")
	}
	if rep.Failed != 1 {
		t.Errorf("Failed=1；got %d", rep.Failed)
	}
	if len(rep.Results[0].Failures) == 0 {
		t.Error("应记录 failure 信息")
	}
}

func TestSortedCaseIDs(t *testing.T) {
	ids := SortedCaseIDs(V04Suite())
	if len(ids) != 10 {
		t.Errorf("应 10 个 ID；got %d", len(ids))
	}
}
