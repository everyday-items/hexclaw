package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
)

// scriptedProvider 在每次 Complete/Stream 时取出脚本里下一项错误（或成功）。
// nil 表示成功；非 nil 表示返回该错误。脚本耗尽后默认成功。
type scriptedProvider struct {
	name        string
	completeSeq []error
	streamSeq   []error
	calls       atomic.Int32
}

func (p *scriptedProvider) Name() string { return p.name }
func (p *scriptedProvider) Complete(_ context.Context, _ llm.CompletionRequest) (*llm.CompletionResponse, error) {
	idx := int(p.calls.Add(1)) - 1
	if idx < len(p.completeSeq) {
		if err := p.completeSeq[idx]; err != nil {
			return nil, err
		}
	}
	return &llm.CompletionResponse{Content: "ok-from-" + p.name}, nil
}
func (p *scriptedProvider) Stream(_ context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	idx := int(p.calls.Add(1)) - 1
	if idx < len(p.streamSeq) {
		if err := p.streamSeq[idx]; err != nil {
			return nil, err
		}
	}
	// 返回非 nil 占位流（测试只验证 wrapper 是否成功调出 Stream）
	return &llm.Stream{}, nil
}
func (p *scriptedProvider) Models() []llm.ModelInfo                  { return nil }
func (p *scriptedProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }

func TestCompleteWithFailover_SuccessFirstTry(t *testing.T) {
	p := &scriptedProvider{name: "p1"}
	fc := &LLMCallContext{Provider: p, ProviderName: "p1"}
	resp, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
	if resp.Content != "ok-from-p1" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("expected 1 call, got %d", got)
	}
}

func TestCompleteWithFailover_InvalidKeyFailsFast(t *testing.T) {
	p := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{errors.New("401 unauthorized")},
	}
	fc := &LLMCallContext{Provider: p, ProviderName: "p1"}
	_, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("invalid key should not retry; got %d calls", got)
	}
}

func TestCompleteWithFailover_RateLimitRetriesSameProvider(t *testing.T) {
	// 第 1 次 429，第 2 次成功
	p := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{errors.New("429 too many requests")},
	}
	fc := &LLMCallContext{Provider: p, ProviderName: "p1"}
	// 用很小的 sleep 加速测试 —— 通过 monkey-patch 不可行，所以构造时直接覆盖默认 backoff
	// HandleFailover 默认 BackoffSeconds=4 太长，这里用 ctx deadline 验证不会被卡死即可
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := CompleteWithFailover(ctx, fc, llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("expected eventual success, got: %v", err)
	}
	if resp.Content != "ok-from-p1" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if got := p.calls.Load(); got != 2 {
		t.Errorf("expected 2 calls (1 retry), got %d", got)
	}
}

func TestCompleteWithFailover_RateLimitSwitchesAfterRetriesExhausted(t *testing.T) {
	p1 := &scriptedProvider{
		name: "p1",
		completeSeq: []error{
			errors.New("429 too many requests"),
			errors.New("429 too many requests"),
			errors.New("429 too many requests"),
			errors.New("429 too many requests"),
		},
	}
	p2 := &scriptedProvider{name: "p2"}
	var fallbackCalls atomic.Int32
	var unhealthyMarks atomic.Int32
	fc := &LLMCallContext{
		Provider:     p1,
		ProviderName: "p1",
		Fallback: func(exclude ...string) (llm.Provider, string, error) {
			fallbackCalls.Add(1)
			return p2, "p2", nil
		},
		MarkProviderUnhealthy: func(name, reason string) {
			if name == "p1" && reason == llm.FailRateLimit.String() {
				unhealthyMarks.Add(1)
			}
		},
		Backoff: func(context.Context, time.Duration) error { return nil },
	}

	resp, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("expected success after switching provider, got: %v", err)
	}
	if resp.Content != "ok-from-p2" {
		t.Fatalf("expected p2 response, got %q", resp.Content)
	}
	if got := p1.calls.Load(); got != 4 {
		t.Fatalf("expected p1 called 4 times before switch, got %d", got)
	}
	if got := p2.calls.Load(); got != 1 {
		t.Fatalf("expected p2 called once, got %d", got)
	}
	if got := fallbackCalls.Load(); got != 1 {
		t.Fatalf("expected one fallback call, got %d", got)
	}
	if got := unhealthyMarks.Load(); got != 1 {
		t.Fatalf("expected one unhealthy mark, got %d", got)
	}
	if fc.ProviderName != "p2" {
		t.Fatalf("expected final provider p2, got %q", fc.ProviderName)
	}
}

func TestCompleteWithFailover_ModelNotFoundSwitchesWhenFallbackAvailable(t *testing.T) {
	p1 := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{errors.New("404 model not found")},
	}
	p2 := &scriptedProvider{name: "p2"}
	var unhealthyMarks atomic.Int32
	fc := &LLMCallContext{
		Provider:     p1,
		ProviderName: "p1",
		Fallback: func(exclude ...string) (llm.Provider, string, error) {
			return p2, "p2", nil
		},
		MarkProviderUnhealthy: func(name, reason string) {
			if name == "p1" && reason == llm.FailModelNotFound.String() {
				unhealthyMarks.Add(1)
			}
		},
	}

	resp, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("expected model-not-found to switch provider, got: %v", err)
	}
	if resp.Content != "ok-from-p2" {
		t.Fatalf("expected p2 response, got %q", resp.Content)
	}
	if got := p1.calls.Load(); got != 1 {
		t.Fatalf("expected p1 fail-fast before switch, got %d calls", got)
	}
	if got := p2.calls.Load(); got != 1 {
		t.Fatalf("expected p2 called once, got %d", got)
	}
	if got := unhealthyMarks.Load(); got != 1 {
		t.Fatalf("expected one unhealthy mark, got %d", got)
	}
}

func TestCompleteWithFailover_ProviderDownSwitches(t *testing.T) {
	p1 := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{errors.New("503 service unavailable")},
	}
	p2 := &scriptedProvider{name: "p2"}
	var fallbackCalls atomic.Int32
	fc := &LLMCallContext{
		Provider:     p1,
		ProviderName: "p1",
		Fallback: func(exclude ...string) (llm.Provider, string, error) {
			fallbackCalls.Add(1)
			for _, e := range exclude {
				if e == "p2" {
					return nil, "", errors.New("no fallback left")
				}
			}
			return p2, "p2", nil
		},
	}
	resp, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("expected success after switch, got: %v", err)
	}
	if resp.Content != "ok-from-p2" {
		t.Errorf("expected p2 to handle, got: %q", resp.Content)
	}
	if got := fallbackCalls.Load(); got != 1 {
		t.Errorf("expected 1 fallback call, got %d", got)
	}
	if fc.ProviderName != "p2" {
		t.Errorf("ProviderName should reflect fallback; got %q", fc.ProviderName)
	}
}

func TestCompleteWithFailover_ContextTooLongTriggersCompress(t *testing.T) {
	p := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{errors.New("400 context length exceeded")},
	}
	var compressCalls atomic.Int32
	fc := &LLMCallContext{
		Provider:     p,
		ProviderName: "p1",
		Compress: func(req *llm.CompletionRequest) bool {
			compressCalls.Add(1)
			return true
		},
	}
	resp, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("expected success after compress, got: %v", err)
	}
	if resp.Content != "ok-from-p1" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if got := compressCalls.Load(); got != 1 {
		t.Errorf("expected 1 compress call, got %d", got)
	}
}

func TestCompleteWithFailover_ContextTooLongWithoutCompressFails(t *testing.T) {
	p := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{errors.New("400 context length exceeded")},
	}
	fc := &LLMCallContext{Provider: p, ProviderName: "p1"} // 无 Compress
	_, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected error when no Compress provided")
	}
}

func TestCompleteWithFailover_FallbackExhausted(t *testing.T) {
	p := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{errors.New("503 service unavailable")},
	}
	fc := &LLMCallContext{
		Provider:     p,
		ProviderName: "p1",
		Fallback: func(exclude ...string) (llm.Provider, string, error) {
			return nil, "", errors.New("no provider available")
		},
	}
	_, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected error when fallback exhausted")
	}
}

func TestCompleteWithFailover_NilContextRejected(t *testing.T) {
	_, err := CompleteWithFailover(context.Background(), nil, llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected error for nil call context")
	}
	_, err = CompleteWithFailover(context.Background(), &LLMCallContext{}, llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestCompleteWithFailover_ContextCancelStops(t *testing.T) {
	p := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{errors.New("429 rate limit")},
	}
	fc := &LLMCallContext{Provider: p, ProviderName: "p1"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	_, err := CompleteWithFailover(ctx, fc, llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected error from canceled ctx")
	}
}

func TestStreamWithFailover_SwitchesProvider(t *testing.T) {
	p1 := &scriptedProvider{
		name:      "p1",
		streamSeq: []error{errors.New("503 unavailable")},
	}
	p2 := &scriptedProvider{name: "p2"}
	fc := &LLMCallContext{
		Provider:     p1,
		ProviderName: "p1",
		Fallback: func(exclude ...string) (llm.Provider, string, error) {
			return p2, "p2", nil
		},
	}
	stream, err := StreamWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if stream == nil {
		t.Fatal("expected non-nil stream")
	}
	if fc.ProviderName != "p2" {
		t.Errorf("expected provider switched to p2, got %q", fc.ProviderName)
	}
}

func TestCompleteWithFailover_LoggerInvokedOnFailure(t *testing.T) {
	p := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{errors.New("404 model not found")},
	}
	var logged atomic.Int32
	fc := &LLMCallContext{
		Provider:     p,
		ProviderName: "p1",
		Logger: func(msg string, fields ...any) {
			logged.Add(1)
		},
	}
	_, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected fail-fast error")
	}
	if got := logged.Load(); got != 1 {
		t.Errorf("expected logger called 1x, got %d", got)
	}
}

// TestCompleteWithFailover_UnknownRetriesOnce 校验未知错误重试 1 次
func TestCompleteWithFailover_UnknownRetriesOnce(t *testing.T) {
	p := &scriptedProvider{
		name:        "p1",
		completeSeq: []error{fmt.Errorf("weird parsing failure")},
	}
	fc := &LLMCallContext{Provider: p, ProviderName: "p1"}
	resp, err := CompleteWithFailover(context.Background(), fc, llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("expected eventual success after retry, got: %v", err)
	}
	if resp.Content != "ok-from-p1" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if got := p.calls.Load(); got != 2 {
		t.Errorf("expected 2 calls, got %d", got)
	}
}
