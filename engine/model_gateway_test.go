package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon/observe/events"
	"github.com/hexagon-codes/hexclaw/featureflag"
)

func withGatewayFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagModelGatewayV1: on,
	})
	return featureflag.WithContext(ctx, flags)
}

// gwTestProvider 是测试用 Provider（不和其他 test 文件中 scriptedProvider 冲突）。
type gwTestProvider struct {
	name      string
	completes int32
	streams   int32
	err       error
	usage     llm.Usage
}

func (p *gwTestProvider) Name() string                             { return p.name }
func (p *gwTestProvider) Models() []llm.ModelInfo                  { return nil }
func (p *gwTestProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }
func (p *gwTestProvider) Complete(_ context.Context, _ llm.CompletionRequest) (*llm.CompletionResponse, error) {
	atomic.AddInt32(&p.completes, 1)
	if p.err != nil {
		return nil, p.err
	}
	return &llm.CompletionResponse{Content: "ok-" + p.name, Usage: p.usage}, nil
}
func (p *gwTestProvider) Stream(_ context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	atomic.AddInt32(&p.streams, 1)
	return &llm.Stream{}, nil
}

func TestChain_FlagOffReturnsInner(t *testing.T) {
	inner := &gwTestProvider{name: "p"}
	rec := NewInMemoryRecorder()
	wrapped := Chain(withGatewayFlag(context.Background(), false), inner, ObserveMiddleware(rec))
	// 直接是 inner，不经过 middleware
	if wrapped != hexagonProvider(inner) {
		t.Errorf("flag OFF 应直接返回 inner")
	}
	_, _ = wrapped.Complete(context.Background(), llm.CompletionRequest{})
	if rec.Count() != 0 {
		t.Errorf("flag OFF 时 recorder 不应被调；got %d", rec.Count())
	}
}

func TestChain_FlagOnAppliesObserveMiddleware(t *testing.T) {
	inner := &gwTestProvider{name: "p", usage: llm.Usage{PromptTokens: 100, CompletionTokens: 50}}
	rec := NewInMemoryRecorder()
	wrapped := Chain(withGatewayFlag(context.Background(), true), inner, ObserveMiddleware(rec))

	resp, err := wrapped.Complete(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok-p" {
		t.Errorf("inner.Complete 应被透传调用")
	}
	calls := rec.Calls()
	if len(calls) != 1 || calls[0].Method != "Complete" || calls[0].Provider != "p" {
		t.Errorf("recorder 应捕获 Complete 调用；got %+v", calls)
	}
	if calls[0].InputTokens != 100 || calls[0].OutputTokens != 50 {
		t.Errorf("token usage 应被捕获；got %+v", calls[0])
	}
}

func TestChain_PreservesOnionOrder(t *testing.T) {
	inner := &gwTestProvider{name: "core"}
	var order []string
	mu := sync.Mutex{}

	mw := func(label string) ProviderMiddleware {
		return func(next llm.Provider) llm.Provider {
			return &recordingMW{inner: next, label: label, order: &order, mu: &mu}
		}
	}

	wrapped := Chain(withGatewayFlag(context.Background(), true), inner, mw("outer"), mw("middle"), mw("inner-mw"))
	_, _ = wrapped.Complete(context.Background(), llm.CompletionRequest{})

	// 期望：outer-before, middle-before, inner-mw-before, core, inner-mw-after, middle-after, outer-after
	wantPrefix := []string{"outer-before", "middle-before", "inner-mw-before"}
	for i, w := range wantPrefix {
		if order[i] != w {
			t.Errorf("middleware 洋葱顺序错；want[%d]=%s got=%v", i, w, order)
		}
	}
}

type recordingMW struct {
	inner llm.Provider
	label string
	order *[]string
	mu    *sync.Mutex
}

func (r *recordingMW) Name() string            { return r.inner.Name() }
func (r *recordingMW) Models() []llm.ModelInfo { return r.inner.Models() }
func (r *recordingMW) CountTokens(m []llm.Message) (int, error) {
	return r.inner.CountTokens(m)
}
func (r *recordingMW) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	r.mu.Lock()
	*r.order = append(*r.order, r.label+"-before")
	r.mu.Unlock()
	resp, err := r.inner.Complete(ctx, req)
	r.mu.Lock()
	*r.order = append(*r.order, r.label+"-after")
	r.mu.Unlock()
	return resp, err
}
func (r *recordingMW) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	return r.inner.Stream(ctx, req)
}

func TestChain_NilMiddlewareSkipped(t *testing.T) {
	inner := &gwTestProvider{name: "p"}
	wrapped := Chain(withGatewayFlag(context.Background(), true), inner, nil)
	// nil mw 应被跳过 → wrapped == inner
	if wrapped != hexagonProvider(inner) {
		t.Errorf("nil middleware 应被跳过")
	}
}

func TestObserveMiddleware_NilRecorderIsZeroOverhead(t *testing.T) {
	inner := &gwTestProvider{name: "p"}
	wrapped := ObserveMiddleware(nil)(inner)
	// 应该是 inner 本身（因为 ObserveMiddleware 在 nil recorder 下返回 identity）
	if wrapped != hexagonProvider(inner) {
		t.Errorf("nil recorder 应直接返回 inner")
	}
}

func TestObserveMiddleware_RecordsErrors(t *testing.T) {
	inner := &gwTestProvider{name: "p", err: errors.New("upstream boom")}
	rec := NewInMemoryRecorder()
	wrapped := ObserveMiddleware(rec)(inner)
	_, err := wrapped.Complete(context.Background(), llm.CompletionRequest{})
	if err == nil {
		t.Fatal("error 应透传")
	}
	calls := rec.Calls()
	if len(calls) != 1 || calls[0].Err == nil {
		t.Errorf("error 应被记录；got %+v", calls)
	}
}

func TestRateLimitMiddleware_Throttles(t *testing.T) {
	inner := &gwTestProvider{name: "p"}
	// 1 req / 200ms：第 2 次调用应当至少等到第 1 次窗口结束
	wrapped := RateLimitMiddleware(1, 200*time.Millisecond)(inner)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	t1 := time.Now()
	_, _ = wrapped.Complete(ctx, llm.CompletionRequest{})
	_, _ = wrapped.Complete(ctx, llm.CompletionRequest{})
	elapsed := time.Since(t1)
	if elapsed < 100*time.Millisecond {
		t.Errorf("第二次调用应被节流；elapsed=%v", elapsed)
	}
}

func TestRateLimitMiddleware_RespectsCtxCancel(t *testing.T) {
	inner := &gwTestProvider{name: "p"}
	wrapped := RateLimitMiddleware(1, 5*time.Second)(inner)

	ctx, cancel := context.WithCancel(context.Background())
	// 先用掉 quota
	_, _ = wrapped.Complete(ctx, llm.CompletionRequest{})
	cancel()
	_, err := wrapped.Complete(ctx, llm.CompletionRequest{})
	if err == nil {
		t.Error("ctx cancel 应导致 error")
	}
}

func TestRateLimitMiddleware_InvalidParamsIdentity(t *testing.T) {
	inner := &gwTestProvider{name: "p"}
	if got := RateLimitMiddleware(0, 0)(inner); got != hexagonProvider(inner) {
		t.Error("无效参数应返回 inner identity")
	}
}

func TestPromptRewriteMiddleware_MutatesRequest(t *testing.T) {
	inner := &gwTestProvider{name: "p"}
	calledWith := ""
	wrapped := PromptRewriteMiddleware(func(req *llm.CompletionRequest) {
		req.Messages = append(req.Messages, llm.Message{Role: "system", Content: "injected"})
	})(inner)

	// 用一个二级 wrapper 抓取最终透传到 inner 的 req
	finalRecorder := &reqRecorder{inner: wrapped, captured: &calledWith}
	_, _ = finalRecorder.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if calledWith == "" {
		t.Fatal("recorder 应抓到改写后的请求")
	}
	// inner 被调用应有 2 messages（user + system）
}

func TestPromptRewriteMiddleware_NilFnIdentity(t *testing.T) {
	inner := &gwTestProvider{name: "p"}
	if got := PromptRewriteMiddleware(nil)(inner); got != hexagonProvider(inner) {
		t.Error("nil fn 应返回 inner identity")
	}
}

// reqRecorder 在 Complete 前抓取 messages 内容进 captured。
type reqRecorder struct {
	inner    llm.Provider
	captured *string
}

func (r *reqRecorder) Name() string            { return r.inner.Name() }
func (r *reqRecorder) Models() []llm.ModelInfo { return r.inner.Models() }
func (r *reqRecorder) CountTokens(m []llm.Message) (int, error) {
	return r.inner.CountTokens(m)
}
func (r *reqRecorder) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	for _, m := range req.Messages {
		*r.captured += string(m.Role) + ":" + m.Content + ";"
	}
	return r.inner.Complete(ctx, req)
}
func (r *reqRecorder) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	return r.inner.Stream(ctx, req)
}

// hexagonProvider 是 llm.Provider 类型别名，便于在 == 比较时不显式 import 路径。
type hexagonProvider = llm.Provider

// v0.4.0 H8: EventsRecorder 把 ObservedCall 投递到 events.Emitter
func TestEventsRecorder_RecordEmitsEvent(t *testing.T) {
	sink := events.NewMemorySink()
	emitter := events.NewEmitter(sink, "test")

	rec := NewEventsRecorder(emitter, "test.observe")
	rec.Record(ObservedCall{
		Provider:     "openai",
		Method:       "Complete",
		Duration:     50 * time.Millisecond,
		InputTokens:  100,
		OutputTokens: 50,
	})

	got := sink.Events()
	if len(got) != 1 {
		t.Fatalf("expected 1 event；got %d", len(got))
	}
	if got[0].Type != "llm.call.observed" {
		t.Errorf("type=%q want llm.call.observed", got[0].Type)
	}
	if got[0].Source != "test.observe" {
		t.Errorf("source=%q want test.observe", got[0].Source)
	}
	if got[0].Data["provider"] != "openai" {
		t.Errorf("data.provider=%v want openai", got[0].Data["provider"])
	}
}

func TestEventsRecorder_NilEmitter_NoOp(t *testing.T) {
	rec := NewEventsRecorder(nil, "")
	// 不应 panic
	rec.Record(ObservedCall{Provider: "x"})
}
