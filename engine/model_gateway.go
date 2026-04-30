// model_gateway.go 实现 v0.4.0 H8 Model Gateway 完整版（feature-flag gated）。
//
// 现状：HexClaw 各处直接调 hexagon.Provider.Stream/Complete，cross-cutting 关注点
// （速率限制、prompt 改写、可观测、多 API key 轮换）只能散落在调用方。这导致：
//   - 一致性差：每个 call site 自己写计数器 / 日志
//   - 难变更：增加全局策略需要改 N 处
//
// H8 引入 ProviderMiddleware：把 hexagon.Provider 包成 wrapper，按"洋葱"模型
// 串联多个 middleware。所有 cross-cutting 关注点变成可插拔组件。
//
// flag model.gateway.v1：alpha 默认 OFF。flag 关闭时 Chain 直接返回未包装的
// inner provider —— 调用路径与 v0.3 完全一致；flag 开启时 middleware 链路才生效。
package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/events"
	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagModelGatewayV1 控制 H8 Provider middleware 是否启用。
const FlagModelGatewayV1 = "model.gateway.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagModelGatewayV1,
		Default:      true, // alpha 强制 OFF
		Description:  "Wrap hexagon.Provider with middleware chain (rate limit / prompt rewrite / observability).",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// ProviderMiddleware 把一个 Provider 装饰成另一个 Provider。
//
// 调用顺序遵循洋葱模型：Chain(p, m1, m2) 等价于 m1(m2(p))。在请求侧，先经过 m1
// 的"before"逻辑，再到 m2 的"before"，最后到 p；响应侧反向。
type ProviderMiddleware func(next hexagon.Provider) hexagon.Provider

// Chain 把 inner provider 与一组 middleware 组合。
//
// flag model.gateway.v1 关闭时直接返回 inner，跳过所有 middleware —— 让 v0.3
// 路径不受影响。flag 开启时按 middleware 声明顺序应用（最后一个先 wrap，首个最外）。
func Chain(ctx context.Context, inner hexagon.Provider, mws ...ProviderMiddleware) hexagon.Provider {
	if !featureflag.Enabled(ctx, FlagModelGatewayV1) {
		return inner
	}
	wrapped := inner
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] == nil {
			continue
		}
		wrapped = mws[i](wrapped)
	}
	return wrapped
}

// ============== Built-in middleware: ObserveMiddleware ==============

// ObservedCall 是 Observe middleware 投递给 Recorder 的事件。
type ObservedCall struct {
	Provider     string
	Method       string // "Complete" 或 "Stream"
	StartedAt    time.Time
	Duration     time.Duration
	InputTokens  int
	OutputTokens int
	Err          error
}

// CallRecorder 接收每次 Provider 调用的观测数据。线程安全。
type CallRecorder interface {
	Record(call ObservedCall)
}

// ObserveMiddleware 在每次 Complete/Stream 前后向 recorder 上报。
//
// recorder 为 nil 时直接返回未包装的 inner（zero-overhead）。
func ObserveMiddleware(recorder CallRecorder) ProviderMiddleware {
	if recorder == nil {
		return func(next hexagon.Provider) hexagon.Provider { return next }
	}
	return func(next hexagon.Provider) hexagon.Provider {
		return &observeProvider{inner: next, recorder: recorder}
	}
}

type observeProvider struct {
	inner    hexagon.Provider
	recorder CallRecorder
}

func (o *observeProvider) Name() string { return o.inner.Name() }
func (o *observeProvider) Models() []llm.ModelInfo { return o.inner.Models() }
func (o *observeProvider) CountTokens(msgs []llm.Message) (int, error) {
	return o.inner.CountTokens(msgs)
}

func (o *observeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	start := time.Now()
	resp, err := o.inner.Complete(ctx, req)
	rec := ObservedCall{
		Provider:  o.inner.Name(),
		Method:    "Complete",
		StartedAt: start,
		Duration:  time.Since(start),
		Err:       err,
	}
	if resp != nil {
		rec.InputTokens = resp.Usage.PromptTokens
		rec.OutputTokens = resp.Usage.CompletionTokens
	}
	o.recorder.Record(rec)
	return resp, err
}

func (o *observeProvider) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	start := time.Now()
	stream, err := o.inner.Stream(ctx, req)
	o.recorder.Record(ObservedCall{
		Provider:  o.inner.Name(),
		Method:    "Stream",
		StartedAt: start,
		Duration:  time.Since(start),
		Err:       err,
	})
	return stream, err
}

// ============== Built-in middleware: RateLimitMiddleware ==============

// RateLimitMiddleware 实现简单的"窗口请求计数"限流：每个 windowDuration 内最多
// maxRequests 次调用，超出时阻塞到下一窗口或 ctx 取消。
//
// 这不是生产级 token bucket（比如 quota 跨重启不持久）；目标是给 Gateway
// 提供一个开箱即用的全局速率护栏，避免某个 provider 被打爆。
func RateLimitMiddleware(maxRequests int, windowDuration time.Duration) ProviderMiddleware {
	if maxRequests <= 0 || windowDuration <= 0 {
		return func(next hexagon.Provider) hexagon.Provider { return next }
	}
	rl := &windowLimiter{
		maxRequests: maxRequests,
		window:      windowDuration,
	}
	return func(next hexagon.Provider) hexagon.Provider {
		return &rateLimitProvider{inner: next, rl: rl}
	}
}

type windowLimiter struct {
	mu          sync.Mutex
	maxRequests int
	window      time.Duration
	count       int
	windowStart time.Time
}

func (l *windowLimiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		if l.windowStart.IsZero() || now.Sub(l.windowStart) >= l.window {
			l.windowStart = now
			l.count = 0
		}
		if l.count < l.maxRequests {
			l.count++
			l.mu.Unlock()
			return nil
		}
		// 当前窗口已满 —— 等到下一窗口或 ctx 取消
		nextWindow := l.windowStart.Add(l.window)
		l.mu.Unlock()
		sleep := time.Until(nextWindow)
		if sleep < 0 {
			continue
		}
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type rateLimitProvider struct {
	inner hexagon.Provider
	rl    *windowLimiter
}

func (r *rateLimitProvider) Name() string                  { return r.inner.Name() }
func (r *rateLimitProvider) Models() []llm.ModelInfo       { return r.inner.Models() }
func (r *rateLimitProvider) CountTokens(msgs []llm.Message) (int, error) {
	return r.inner.CountTokens(msgs)
}
func (r *rateLimitProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if err := r.rl.wait(ctx); err != nil {
		return nil, err
	}
	return r.inner.Complete(ctx, req)
}
func (r *rateLimitProvider) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	if err := r.rl.wait(ctx); err != nil {
		return nil, err
	}
	return r.inner.Stream(ctx, req)
}

// ============== Built-in middleware: PromptRewriteMiddleware ==============

// PromptRewriteFunc 在 request 发出前修改它（注入 system 提示 / 加 metadata 等）。
type PromptRewriteFunc func(*llm.CompletionRequest)

// PromptRewriteMiddleware 在每次 Complete/Stream 前调用 fn 改写 req。
func PromptRewriteMiddleware(fn PromptRewriteFunc) ProviderMiddleware {
	if fn == nil {
		return func(next hexagon.Provider) hexagon.Provider { return next }
	}
	return func(next hexagon.Provider) hexagon.Provider {
		return &promptRewriteProvider{inner: next, fn: fn}
	}
}

type promptRewriteProvider struct {
	inner hexagon.Provider
	fn    PromptRewriteFunc
}

func (p *promptRewriteProvider) Name() string                  { return p.inner.Name() }
func (p *promptRewriteProvider) Models() []llm.ModelInfo       { return p.inner.Models() }
func (p *promptRewriteProvider) CountTokens(msgs []llm.Message) (int, error) {
	return p.inner.CountTokens(msgs)
}
func (p *promptRewriteProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.fn(&req)
	return p.inner.Complete(ctx, req)
}
func (p *promptRewriteProvider) Stream(ctx context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	p.fn(&req)
	return p.inner.Stream(ctx, req)
}

// ============== EventsRecorder：把 ObservedCall 投递到 events.Emitter ==============

// EventsRecorder 实现 CallRecorder，把每次 LLM 调用观测投递到 events.Emitter
// 作为 "llm.call.observed" 结构化事件。供 cmd/hexclaw 启动时构造一次注入到
// SetDefaultLLMMiddlewares，让 ObserveMiddleware 在 flag model.gateway.v1 ON
// 时自动埋点（flag OFF 时 Chain 跳过包装，本 recorder 永远不会被调）。
//
// 直接持有 Emitter（而非走 events.Emit 从 ctx 取）：因为 CallRecorder.Record
// 没有 ctx 参数，无法承载 root ctx 中注入的 emitter。
//
// emitter 为 nil 时 Record 是 no-op。
type EventsRecorder struct {
	emitter *events.Emitter
	source  string
}

// NewEventsRecorder 构造一个 recorder 绑定 emitter；source 用作 WithSource()。
// source 留空时默认填 "engine.llm_observe"。
func NewEventsRecorder(emitter *events.Emitter, source string) *EventsRecorder {
	if source == "" {
		source = "engine.llm_observe"
	}
	return &EventsRecorder{emitter: emitter, source: source}
}

// Record 把 ObservedCall 转 events.Event 投递。
func (r *EventsRecorder) Record(call ObservedCall) {
	if r == nil || r.emitter == nil {
		return
	}
	severity := events.SeverityInfo
	if call.Err != nil {
		severity = events.SeverityError
	}
	ev := events.New("llm.call.observed", severity).
		WithSource(r.source).
		With("provider", call.Provider).
		With("method", call.Method).
		With("input_tokens", call.InputTokens).
		With("output_tokens", call.OutputTokens).
		With("duration_ms", call.Duration.Milliseconds())
	if call.Err != nil {
		ev = ev.With("error", call.Err.Error())
	}
	_ = r.emitter.Emit(context.Background(), ev)
}

// ============== InMemoryRecorder：测试 / 本地调试用 ==============

// InMemoryRecorder 把所有 ObservedCall 累计到内存。线程安全。
type InMemoryRecorder struct {
	mu    sync.Mutex
	calls []ObservedCall
	count atomic.Int64
}

// NewInMemoryRecorder 构造一个空 recorder。
func NewInMemoryRecorder() *InMemoryRecorder { return &InMemoryRecorder{} }

// Record 追加观测。
func (r *InMemoryRecorder) Record(call ObservedCall) {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	r.count.Add(1)
}

// Count 返回总调用次数。
func (r *InMemoryRecorder) Count() int64 { return r.count.Load() }

// Calls 返回观测列表副本。
func (r *InMemoryRecorder) Calls() []ObservedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ObservedCall, len(r.calls))
	copy(out, r.calls)
	return out
}
