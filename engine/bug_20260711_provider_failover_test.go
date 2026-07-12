package engine

// BUG-20260711 云端 provider 自动回退：默认走 openrouter 免费模型，额度耗尽 429 后
// 整个云端不可用；用户已配健康的智谱(glm-4.5)但引擎不回退。本套件钉死不变量：
//   - provider 硬失败（429/5xx/额度/超时/连接失败）且非用户显式 pin → 标记短期熔断 +
//     自动换下一个健康 provider 重试一次，让对话在有备用 provider 时仍成功。
//   - 用户显式 pin 的 provider 不静默改派（尊重选择）——只把错误人性化。
//   - isProviderUnavailableError 表驱动正反例锁分类边界（排除“工具不支持”，那条走去工具降级）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// failoverProvider 是一个可控测试 provider：按名字返回固定 content 或恒定错误，
// 并原子计数被调用次数（用于断言“回退是否真的换到了它/绕过了它”）。
type failoverProvider struct {
	name    string
	content string
	err     error
	calls   int32
}

func (p *failoverProvider) Name() string { return p.name }

func (p *failoverProvider) Complete(ctx context.Context, req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	atomic.AddInt32(&p.calls, 1)
	if p.err != nil {
		return nil, p.err
	}
	return &hexagon.CompletionResponse{
		Content: p.content,
		Usage:   hexagon.Usage{TotalTokens: 12},
	}, nil
}

func (p *failoverProvider) Stream(ctx context.Context, req hexagon.CompletionRequest) (*llm.Stream, error) {
	atomic.AddInt32(&p.calls, 1)
	if p.err != nil {
		return nil, p.err
	}
	// 用 OpenAI 流式契约格式（choices[].delta.content）把 content 作为一次性 chunk 吐出。
	payload, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]string{"content": p.content}}},
	})
	sse := "data: " + string(payload) + "\n\ndata: [DONE]\n\n"
	return llm.NewStream(strings.NewReader(sse), llm.StreamOpenAIFormat), nil
}

func (p *failoverProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock"}}
}

func (p *failoverProvider) CountTokens(messages []hexagon.Message) (int, error) {
	return len(messages), nil
}

func (p *failoverProvider) callCount() int32 { return atomic.LoadInt32(&p.calls) }

// newFailoverEngine 装配一个 2-provider 云端 router 的引擎：默认 p_bad，备用 p_good。
func newFailoverEngine(t *testing.T, bad, good *failoverProvider) *ReActEngine {
	t.Helper()
	return newEngineWithProviders(t,
		map[string]hexagon.Provider{
			bad.name:  bad,
			good.name: good,
		},
		map[string]config.LLMProviderConfig{
			bad.name:  {Model: "bad-model"},
			good.name: {Model: "good-model"},
		},
		bad.name,
	)
}

const failoverGoodContent = "这是来自健康 provider 的正常回答内容"

// TestProviderFailover_NonStreaming 是 RED-A：非流式路径 429 → 回退到 p_good。
func TestProviderFailover_NonStreaming(t *testing.T) {
	bad := &failoverProvider{name: "p_bad", err: errors.New("429 Too Many Requests: free-models-per-day limit reached")}
	good := &failoverProvider{name: "p_good", content: failoverGoodContent}
	eng := newFailoverEngine(t, bad, good)

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-fo-1", Platform: adapter.PlatformAPI,
		UserID: "user-1", ChatID: "chat-1",
		Content: "你好，帮我算一下 2+2",
	})
	if err != nil {
		t.Fatalf("Process 返回错误（应回退成功）: %v", err)
	}
	if !strings.Contains(reply.Content, failoverGoodContent) {
		t.Fatalf("未发生回退：reply=%q，应含健康 provider 的内容 %q", reply.Content, failoverGoodContent)
	}
	if good.callCount() == 0 {
		t.Fatalf("备用 provider p_good 从未被调用——回退没发生")
	}
	if bad.callCount() == 0 {
		t.Fatalf("默认 provider p_bad 应被先调用一次")
	}
}

// TestProviderFailover_Streaming 是 RED-B：流式路径 429 → 回退到 p_good。
func TestProviderFailover_Streaming(t *testing.T) {
	bad := &failoverProvider{name: "p_bad", err: errors.New("503 Service Unavailable")}
	good := &failoverProvider{name: "p_good", content: failoverGoodContent}
	eng := newFailoverEngine(t, bad, good)

	ch, err := eng.ProcessStream(context.Background(), &adapter.Message{
		ID: "msg-fo-2", Platform: adapter.PlatformAPI,
		UserID: "user-2", ChatID: "chat-2",
		Content: "帮我写一句问候语",
	})
	if err != nil {
		t.Fatalf("ProcessStream 返回错误: %v", err)
	}
	var sb strings.Builder
	var lastErr error
	for chunk := range ch {
		if chunk.Error != nil {
			lastErr = chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	got := sb.String()
	if lastErr != nil {
		t.Fatalf("流式最终带错误（应回退成功）: %v；内容=%q", lastErr, got)
	}
	if !strings.Contains(got, failoverGoodContent) {
		t.Fatalf("流式未回退：内容=%q，应含健康 provider 内容 %q", got, failoverGoodContent)
	}
	if good.callCount() == 0 {
		t.Fatalf("流式备用 provider p_good 从未被调用——回退没发生")
	}
}

// TestProviderFailover_StreamTripsCircuitBreaker 是 RED-C：流式路径回退后必须熔断失败
// provider，使后续请求直接路由到健康 provider、不再重复吃 429（否则每条消息都先失败一次）。
func TestProviderFailover_StreamTripsCircuitBreaker(t *testing.T) {
	bad := &failoverProvider{name: "p_bad", err: errors.New("429 Too Many Requests")}
	good := &failoverProvider{name: "p_good", content: failoverGoodContent}
	eng := newFailoverEngine(t, bad, good)

	// 每轮用不同内容，绕开语义缓存（否则第二条同文命中缓存、根本不打 provider，测不出熔断）。
	prompts := []string{"帮我写第一句问候语", "再换一句不同的问候语"}
	for i, p := range prompts {
		ch, err := eng.ProcessStream(context.Background(), &adapter.Message{
			ID: "msg-fo-c-" + p, Platform: adapter.PlatformAPI,
			UserID: "user-c", ChatID: "chat-c", Content: p,
		})
		if err != nil {
			t.Fatalf("ProcessStream #%d 出错: %v", i, err)
		}
		for range ch {
		}
	}
	// 第一条请求熔断 p_bad 后，第二条应直接走 p_good，绝不再打 p_bad。
	if bad.callCount() != 1 {
		t.Fatalf("熔断未生效：p_bad 被调用了 %d 次（应恰好 1 次，后续请求应直连 p_good）", bad.callCount())
	}
	if good.callCount() < 2 {
		t.Fatalf("两条请求都应由 p_good 承接，但 p_good 只被调用 %d 次", good.callCount())
	}
}

// TestProviderFailover_MultiProviderLoop 是 RED-D：一个请求内连着遇到多个不可用 provider
// 时，应遍历到健康的那个（而非单跳一次就放弃）。
func TestProviderFailover_MultiProviderLoop(t *testing.T) {
	bad1 := &failoverProvider{name: "a_bad1", err: errors.New("429 Too Many Requests")}
	bad2 := &failoverProvider{name: "b_bad2", err: errors.New("503 Service Unavailable")}
	good := &failoverProvider{name: "c_good", content: failoverGoodContent}
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{bad1.name: bad1, bad2.name: bad2, good.name: good},
		map[string]config.LLMProviderConfig{
			bad1.name: {Model: "m1"}, bad2.name: {Model: "m2"}, good.name: {Model: "m3"},
		},
		bad1.name)

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-fo-d", Platform: adapter.PlatformAPI,
		UserID: "user-d", ChatID: "chat-d", Content: "hi",
	})
	if err != nil {
		t.Fatalf("Process 出错（应遍历到 c_good 成功）: %v", err)
	}
	if !strings.Contains(reply.Content, failoverGoodContent) {
		t.Fatalf("未遍历到健康 provider：reply=%q", reply.Content)
	}
	if good.callCount() == 0 {
		t.Fatalf("健康 provider c_good 从未被调用——多跳回退没发生（bad1=%d bad2=%d）", bad1.callCount(), bad2.callCount())
	}
}

// TestProviderFailover_ExplicitPinNoFailover：用户显式 pin p_bad → 不回退（尊重选择）。
func TestProviderFailover_ExplicitPinNoFailover(t *testing.T) {
	bad := &failoverProvider{name: "p_bad", err: errors.New("429 Too Many Requests")}
	good := &failoverProvider{name: "p_good", content: failoverGoodContent}
	eng := newFailoverEngine(t, bad, good)

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-fo-3", Platform: adapter.PlatformAPI,
		UserID: "user-3", ChatID: "chat-3",
		Content:  "你好",
		Metadata: map[string]string{"provider": "p_bad"}, // 显式 pin
	})
	// 显式 pin 失败：不回退，返回（人性化）错误，且绝不静默改派到 p_good。
	if good.callCount() != 0 {
		t.Fatalf("显式 pin 时不应回退，但 p_good 被调用了 %d 次", good.callCount())
	}
	if err == nil && strings.Contains(reply.Content, failoverGoodContent) {
		t.Fatalf("显式 pin 被静默改派到了 p_good（reply=%q）——违反尊重用户选择", reply.Content)
	}
}

// TestIsProviderUnavailableError 表驱动锁分类边界。
func TestIsProviderUnavailableError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429", errors.New("429 Too Many Requests"), true},
		{"rate limit", errors.New("upstream rate limit exceeded"), true},
		{"quota free models", errors.New("free-models-per-day limit reached"), true},
		{"insufficient", errors.New("insufficient balance"), true},
		{"500", errors.New("500 internal server error"), true},
		{"502", errors.New("502 Bad Gateway"), true},
		{"503 unavailable", errors.New("503 service unavailable"), true},
		{"connection refused", errors.New("dial tcp: connection refused"), true},
		{"timeout", errors.New("context deadline exceeded"), true},
		{"i/o timeout", errors.New("read tcp: i/o timeout"), true},
		{"nil", nil, false},
		{"normal长文本", errors.New("这是一段正常的模型回答，讲了很多内容但没有任何故障特征"), false},
		{"tool unsupported", errors.New("No endpoints found that support tool use"), false},
		{"does not support tools", errors.New("this model does not support tools"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isProviderUnavailableError(c.err); got != c.want {
				t.Fatalf("isProviderUnavailableError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
