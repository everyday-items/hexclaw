package builtin

// 风险自审测试：high 风险被拦、low 放行、判级失败/无判级器保守拒、高危关键词短路。

import (
	"context"
	"errors"
	"testing"
)

func judgeReturning(word string) RiskJudgeFunc {
	return func(_ context.Context, _ string) (string, error) { return word, nil }
}

func TestLLMRiskReviewer_ParsesLevels(t *testing.T) {
	cases := map[string]RiskLevel{
		"low":             RiskLow,
		"LOW":             RiskLow,
		"low risk":        RiskLow,
		"medium":          RiskMedium,
		"med":             RiskMedium,
		"high":            RiskHigh,
		"definitely high": RiskHigh,
		"banana":          RiskHigh, // unparseable → conservative deny
		"":                RiskHigh,
	}
	for reply, want := range cases {
		r := NewLLMRiskReviewer(judgeReturning(reply))
		got, err := r.Assess(context.Background(), "send to feishu", "today's digest")
		if err != nil {
			t.Fatalf("reply %q: unexpected err %v", reply, err)
		}
		if got != want {
			t.Fatalf("reply %q: got level %d, want %d", reply, got, want)
		}
	}
}

func TestLLMRiskReviewer_HighRiskKeyword_ShortCircuits(t *testing.T) {
	called := false
	judge := func(_ context.Context, _ string) (string, error) { called = true; return "low", nil }
	r := NewLLMRiskReviewer(judge)
	lvl, err := r.Assess(context.Background(), "send to discord", "here is the api key sk-abc123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if lvl != RiskHigh {
		t.Fatalf("credential payload must be high, got %d", lvl)
	}
	if called {
		t.Fatalf("high-risk keyword must short-circuit before calling the LLM")
	}
}

func TestLLMRiskReviewer_JudgeError_FailsClosed(t *testing.T) {
	r := NewLLMRiskReviewer(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("llm down")
	})
	lvl, err := r.Assess(context.Background(), "send", "hi")
	if err == nil {
		t.Fatalf("judge error must propagate")
	}
	if lvl != RiskHigh {
		t.Fatalf("judge error must fail closed to high, got %d", lvl)
	}
}

func TestLLMRiskReviewer_NilJudge_FailsClosed(t *testing.T) {
	r := NewLLMRiskReviewer(nil)
	lvl, err := r.Assess(context.Background(), "send", "hi")
	if err == nil || lvl != RiskHigh {
		t.Fatalf("nil judge must fail closed (high+err), got lvl=%d err=%v", lvl, err)
	}
}
