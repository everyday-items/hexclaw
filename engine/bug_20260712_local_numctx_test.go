package engine

// BUG-20260712-c 本地模型卡死：ai-core 自动 num_ctx 分档 + 粘性"只升不降" + 预热把 num_ctx 抬到
// 16384/32768，9B 模型 KV cache 在 16GB 机器上撑爆物理内存 → 狂刷 swap → 每 token 等磁盘 → 整机
// 卡死（用户报"v0.4.9 前本地一直正常，这版新建空会话没挂智能体也特别慢"）。
//
// 真机取证（Intel i7-8850H / 16GB / 纯 CPU）：
//   - num_ctx=16384：裸 ollama "你好" 超时 >120s，swap 用到 11GB。
//   - num_ctx=2048 ：热请求 7s，正常回"您好！有什么可以帮助您的吗？"。
//
// 修复：Ollama provider 配置 num_ctx 后，本地请求显式下发 num_ctx（ai-core 当契约，跳过自动分档
// 与 needed>numCtx 报错，长 prompt 由 context-shift 优雅截断而非撑爆内存）；云端 provider 不注入。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// numCtxCaptureProvider 记录每次请求 metadata 里的 num_ctx（断言注入与否/取值）。
type numCtxCaptureProvider struct {
	mu     sync.Mutex
	seen   []any // 每次调用的 req.Metadata["num_ctx"]（nil=未注入）
	called bool
}

func (p *numCtxCaptureProvider) record(req hexagon.CompletionRequest) {
	p.mu.Lock()
	p.called = true
	if req.Metadata != nil {
		p.seen = append(p.seen, req.Metadata["num_ctx"])
	} else {
		p.seen = append(p.seen, nil)
	}
	p.mu.Unlock()
}

func (p *numCtxCaptureProvider) Name() string { return "numctx-capture" }

func (p *numCtxCaptureProvider) Complete(_ context.Context, req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	p.record(req)
	return &hexagon.CompletionResponse{Content: "ok"}, nil
}

func (p *numCtxCaptureProvider) Stream(_ context.Context, req hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	p.record(req)
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: [DONE]`, "",
	}, "\n")
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func (p *numCtxCaptureProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "qwen3.5:9b", Name: "Qwen"}}
}
func (p *numCtxCaptureProvider) CountTokens([]llm.Message) (int, error) { return 1, nil }

func (p *numCtxCaptureProvider) firstNumCtx(t *testing.T) any {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.called {
		t.Fatal("provider 未被调用")
	}
	return p.seen[0]
}

func numCtxEngine(t *testing.T, name string, cfg config.LLMProviderConfig, p hexagon.Provider) *ReActEngine {
	t.Helper()
	return newEngineWithProviders(t,
		map[string]hexagon.Provider{name: p},
		map[string]config.LLMProviderConfig{name: cfg},
		name,
	)
}

// TestLocalNumCtxCap_InjectedForLocalProvider 本地 Ollama + 配置 num_ctx=4096 → 请求带 num_ctx=4096。
func TestLocalNumCtxCap_InjectedForLocalProvider(t *testing.T) {
	p := &numCtxCaptureProvider{}
	eng := numCtxEngine(t, "Ollama (本地)", config.LLMProviderConfig{Model: "qwen3.5:9b", NumCtx: 4096}, p)
	if _, err := eng.Process(context.Background(), &adapter.Message{
		ID: "nc-local", Platform: adapter.PlatformAPI, UserID: "u", Content: "你好",
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	got := p.firstNumCtx(t)
	if !numCtxEquals(got, 4096) {
		t.Fatalf("BUG 复现：本地请求未按配置注入 num_ctx=4096（KV 会自动分档到 16384 撑爆内存），got %v", got)
	}
}

// TestLocalNumCtxCap_NotInjectedWhenUnset 未配置 num_ctx → 不注入（保持 ai-core 自动分档，不影响大内存机）。
func TestLocalNumCtxCap_NotInjectedWhenUnset(t *testing.T) {
	p := &numCtxCaptureProvider{}
	eng := numCtxEngine(t, "Ollama (本地)", config.LLMProviderConfig{Model: "qwen3.5:9b"}, p)
	if _, err := eng.Process(context.Background(), &adapter.Message{
		ID: "nc-unset", Platform: adapter.PlatformAPI, UserID: "u", Content: "你好",
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := p.firstNumCtx(t); got != nil {
		t.Fatalf("未配置 num_ctx 时不应注入（应留给自动分档），got %v", got)
	}
}

// TestLocalNumCtxCap_NotInjectedForCloudProvider 云端 provider 即便配了 num_ctx 也不注入（isLocal=false）。
func TestLocalNumCtxCap_NotInjectedForCloudProvider(t *testing.T) {
	p := &numCtxCaptureProvider{}
	eng := numCtxEngine(t, "智谱 AI", config.LLMProviderConfig{Model: "glm-4.5", NumCtx: 4096}, p)
	if _, err := eng.Process(context.Background(), &adapter.Message{
		ID: "nc-cloud", Platform: adapter.PlatformAPI, UserID: "u", Content: "你好",
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := p.firstNumCtx(t); got != nil {
		t.Fatalf("云端 provider 不应注入 num_ctx（仅 Ollama 生效），got %v", got)
	}
}

func numCtxEquals(got any, want int) bool {
	switch v := got.(type) {
	case int:
		return v == want
	case int64:
		return int(v) == want
	case float64:
		return int(v) == want
	default:
		return false
	}
}
