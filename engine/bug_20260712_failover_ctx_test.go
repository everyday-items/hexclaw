package engine

// BUG-20260712-b failover 继承被取消的 ctx（真机 "hello" 整条对话仍失败的最后一环）。
//
// 复现的真机链路（在 BUG-20260712-a egress 修复之后仍失败）：
//   1) 路由先选本地 Ollama(tools=33) → CPU 巨型 prompt header 超时 → 本地 HTTP 客户端**取消了
//      共享请求 ctx**，错误呈 "context canceled"。
//   2) 引擎回退到云端健康 provider（智谱 glm-4v-flash），egress 已 cloud-safe（-a 修复），
//      但回退请求的 ctx 是从**那个已被取消的 ctx**派生的 → 云端 provider 拿到手就是 canceled
//      → 真实 HTTP 客户端立刻 "context canceled" 失败，provider 被熔断。
//   3) 没有更多健康 provider → 落到友好错误 "模型服务暂时不可用"。用户看到的是「本地能用、云端
//      也配了，却什么都回不出来」。真机日志取证：`to=智谱 AI model=glm-4v-flash ... err=context
//      canceled`——回退**打到了**智谱，却被毒化的 ctx 就地枪毙。
//
// 修复（本套件钉死）：rebuildRequestForFailover 用 context.WithoutCancel 脱离上游取消，让回退请求
// 拿到一个干净、可用的 ctx；各 provider 客户端自带超时兜底，不会无限挂。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
)

// ctxPoisonLocalProvider 模拟本地 Ollama header 超时：被调用时**取消共享请求 ctx**（cancel），
// 再返回 "context canceled" 错误——真机里本地 HTTP 客户端就是这样毒化整条链的。
type ctxPoisonLocalProvider struct {
	name   string
	cancel context.CancelFunc
	calls  int32
}

func (p *ctxPoisonLocalProvider) Name() string { return p.name }

func (p *ctxPoisonLocalProvider) poison() error {
	atomic.AddInt32(&p.calls, 1)
	if p.cancel != nil {
		p.cancel() // 本地超时取消共享 ctx（毒化后续回退）
	}
	return errors.New(`Post "http://localhost:11434/api/chat": context canceled`)
}

func (p *ctxPoisonLocalProvider) Complete(_ context.Context, _ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	return nil, p.poison()
}

func (p *ctxPoisonLocalProvider) Stream(_ context.Context, _ hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	return nil, p.poison()
}

func (p *ctxPoisonLocalProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "qwen3.5:9b", Name: "Qwen"}}
}

func (p *ctxPoisonLocalProvider) CountTokens(messages []hexagon.Message) (int, error) {
	return len(messages), nil
}

func (p *ctxPoisonLocalProvider) callCount() int32 { return atomic.LoadInt32(&p.calls) }

// ctxHealthCloudProvider 是回退目标：像真实 HTTP 客户端一样，收到**已取消的 ctx** 就立刻失败
// （记 sawCanceled），只有拿到干净 ctx 才 capture egress 信封并正常回答 "ok"。
type ctxHealthCloudProvider struct {
	egressCaptureProvider
	sawCanceled int32
}

func (p *ctxHealthCloudProvider) Complete(ctx context.Context, _ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		atomic.StoreInt32(&p.sawCanceled, 1)
		return nil, fmt.Errorf(`Post "https://open.bigmodel.cn": %w`, err)
	}
	p.capture(ctx)
	return &hexagon.CompletionResponse{Content: "ok"}, nil
}

func (p *ctxHealthCloudProvider) Stream(ctx context.Context, _ hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	if err := ctx.Err(); err != nil {
		atomic.StoreInt32(&p.sawCanceled, 1)
		return nil, fmt.Errorf(`Post "https://open.bigmodel.cn": %w`, err)
	}
	p.capture(ctx)
	body := strings.Join([]string{
		`data: {"id":"c1","model":"mock-model","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func (p *ctxHealthCloudProvider) canceledSeen() bool { return atomic.LoadInt32(&p.sawCanceled) == 1 }

// TestFailover_NonStreaming_DetachesCanceledCtx 非流式：本地取消共享 ctx 后回退云端，
// 回退请求必须拿到干净 ctx（rebuildRequestForFailover WithoutCancel），云端不得看见 canceled。
func TestFailover_NonStreaming_DetachesCanceledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local := &ctxPoisonLocalProvider{name: "Ollama (本地)", cancel: cancel}
	cloud := &ctxHealthCloudProvider{}
	eng := newFailoverEgressEngine(t, local, cloud, local.name, "openrouter")

	reply, err := eng.Process(ctx, &adapter.Message{
		ID: "foe-ctx-nonstream", Platform: adapter.PlatformAPI,
		UserID: "u-ctx-1", ChatID: "c-ctx-1",
		Content: "hello",
	})
	if err != nil {
		t.Fatalf("BUG 复现：本地取消共享 ctx 后回退云端继承了被取消的 ctx → 云端就地 context canceled：%v", err)
	}
	if !strings.Contains(reply.Content, "ok") {
		t.Fatalf("应拿到云端 provider 的正常回答，got %q", reply.Content)
	}
	if local.callCount() == 0 {
		t.Fatalf("本地 provider 应先被调用一次（是回退+取消的起点）")
	}
	if cloud.canceledSeen() {
		t.Fatalf("BUG 复现：回退请求把被取消的 ctx 透传给了云端（应 WithoutCancel 脱钩）")
	}
}

// TestFailover_Streaming_DetachesCanceledCtx 流式（真机同构，日志里就是流式回退）：同上。
func TestFailover_Streaming_DetachesCanceledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local := &ctxPoisonLocalProvider{name: "Ollama (本地)", cancel: cancel}
	cloud := &ctxHealthCloudProvider{}
	eng := newFailoverEgressEngine(t, local, cloud, local.name, "openrouter")

	ch, err := eng.ProcessStream(ctx, &adapter.Message{
		ID: "foe-ctx-stream", Platform: adapter.PlatformAPI,
		UserID: "u-ctx-2", ChatID: "c-ctx-2",
		Content: "hello",
	})
	if err != nil {
		t.Fatalf("ProcessStream 建流失败: %v", err)
	}
	out, derr := drainStream(t, ch)
	if derr != nil {
		t.Fatalf("BUG 复现：流式本地取消 ctx 后回退云端继承取消 → 云端就地 context canceled：%v", derr)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("流式应拿到云端 provider 的正常回答，got %q", out)
	}
	if local.callCount() == 0 {
		t.Fatalf("本地 provider 应先被调用一次（是回退+取消的起点）")
	}
	if cloud.canceledSeen() {
		t.Fatalf("BUG 复现：流式回退请求把被取消的 ctx 透传给了云端（应 WithoutCancel 脱钩）")
	}
}
