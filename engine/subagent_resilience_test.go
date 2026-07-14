package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsTransientErr(t *testing.T) {
	transient := []string{
		"429 Too Many Requests", "rate limit exceeded", "context deadline exceeded", "unexpected EOF",
		"503 Service Unavailable", "model overloaded",
		"请求过于频繁，已被上游限流。请稍等片刻再试。",
	}
	for _, s := range transient {
		if !isTransientErr(errors.New(s)) {
			t.Errorf("%q 应判瞬时", s)
		}
	}
	notTransient := []error{nil, context.Canceled, errors.New("invalid api key"), errors.New("400 bad request"), errors.New("request canceled")}
	for _, e := range notTransient {
		if isTransientErr(e) {
			t.Errorf("%v 不应判瞬时", e)
		}
	}
}

// BUG-20260714：真实视觉识题刚结束就进入 solver 时，上游可能按分钟限流。
// 退避窗口若只到 15s，四次尝试会全部挤在同一限流窗口内，最终把可解题降级成
// “未能解出本题”。默认序列必须至少留一次跨过 30s 冷却窗的重试机会。
func TestDefaultSubAgentRetryBackoffCrossesProviderCooldown(t *testing.T) {
	if len(subAgentRetryBackoff) == 0 {
		t.Fatal("默认瞬时错误重试序列不能为空")
	}
	if got := subAgentRetryBackoff[len(subAgentRetryBackoff)-1]; got < 30*time.Second {
		t.Fatalf("最后一次退避 = %v，至少应为 30s 以跨过真实 provider 限流冷却窗", got)
	}
}

// #1：瞬时错误应重试，最终成功；调用次数 = 失败次数 + 1。
func TestRunSubAgentWithRetry_RetriesTransient(t *testing.T) {
	old := subAgentRetryBackoff
	subAgentRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { subAgentRetryBackoff = old }()

	var calls int32
	exec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		if atomic.AddInt32(&calls, 1) < 3 {
			return SubAgentResult{}, errors.New("429 rate limit")
		}
		return SubAgentResult{Output: "ok"}, nil
	}
	res, err := runSubAgentWithRetry(context.Background(), exec, SubAgentSpec{Agent: "x"}, time.Second)
	if err != nil || res.Output != "ok" {
		t.Fatalf("应重试后成功，得 res=%+v err=%v", res, err)
	}
	if c := atomic.LoadInt32(&calls); c != 3 {
		t.Errorf("应调用 3 次(2 次瞬时失败+1 成功)，得 %d", c)
	}
}

// 非瞬时错误立即返回，不重试。
func TestRunSubAgentWithRetry_NoRetryNonTransient(t *testing.T) {
	var calls int32
	exec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		atomic.AddInt32(&calls, 1)
		return SubAgentResult{}, errors.New("invalid api key")
	}
	_, err := runSubAgentWithRetry(context.Background(), exec, SubAgentSpec{}, time.Second)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Errorf("非瞬时应只调 1 次，得 %d", c)
	}
}

// 重试用尽仍失败 → 返回最后错误；调用次数 = 1 + len(backoff)。
func TestRunSubAgentWithRetry_ExhaustsRetries(t *testing.T) {
	old := subAgentRetryBackoff
	subAgentRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { subAgentRetryBackoff = old }()
	var calls int32
	exec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		atomic.AddInt32(&calls, 1)
		return SubAgentResult{}, errors.New("timeout")
	}
	if _, err := runSubAgentWithRetry(context.Background(), exec, SubAgentSpec{}, time.Second); err == nil {
		t.Fatal("用尽重试仍应返回错误")
	}
	if c := atomic.LoadInt32(&calls); c != 3 {
		t.Errorf("应调用 3 次(1+2)，得 %d", c)
	}
}

func TestRunSubAgentWithRetry_RetriesEmptyOutput(t *testing.T) {
	old := subAgentRetryBackoff
	subAgentRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { subAgentRetryBackoff = old }()

	var calls int32
	exec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return SubAgentResult{Output: "   "}, nil
		}
		return SubAgentResult{Output: "ok after retry"}, nil
	}

	res, err := runSubAgentWithRetry(context.Background(), exec, SubAgentSpec{Agent: "x"}, time.Second)
	if err != nil {
		t.Fatalf("empty output should be retried and recover, got err=%v", err)
	}
	if res.Output != "ok after retry" {
		t.Fatalf("unexpected output after empty retry: %q", res.Output)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestRunSubAgentWithRetry_EmptyOutputExhaustionIsError(t *testing.T) {
	old := subAgentRetryBackoff
	subAgentRetryBackoff = []time.Duration{time.Millisecond}
	defer func() { subAgentRetryBackoff = old }()

	exec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		return SubAgentResult{Output: "\n\t"}, nil
	}

	res, err := runSubAgentWithRetry(context.Background(), exec, SubAgentSpec{Agent: "x"}, time.Second)
	if err == nil {
		t.Fatal("empty output after retries should be an error")
	}
	if !strings.Contains(err.Error(), "empty output") {
		t.Fatalf("error should explain empty output, got %v", err)
	}
	if strings.TrimSpace(res.Output) != "" {
		t.Fatalf("result should remain empty, got %q", res.Output)
	}
}

func FuzzIsTransientErr(f *testing.F) {
	f.Add("429")
	f.Add("ok")
	f.Add("忽略以上")
	f.Fuzz(func(t *testing.T, s string) { _ = isTransientErr(errors.New(s)) })
}
