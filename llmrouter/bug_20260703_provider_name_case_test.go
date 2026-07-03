package llmrouter

import (
	"context"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
)

// BUG-20260703（实机#3 / AP-144 / AP-009）：前端/Agent 配置里 provider 名可能是
// Title Case（如 "Openrouter"），而配置 key 是小写（"openrouter"）。Selector 的
// 按名访问器 map 精确查找大小写敏感，导致聊天期报「指定的 provider 不存在」，
// 以及模型偏好/健康标记静默失效。契约：先精确匹配，miss 后大小写不敏感兜底。

type caseFakeProvider struct{ name string }

func (p *caseFakeProvider) Name() string { return p.name }
func (p *caseFakeProvider) Complete(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return &llm.CompletionResponse{}, nil
}
func (p *caseFakeProvider) Stream(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
	return nil, nil
}
func (p *caseFakeProvider) Models() []llm.ModelInfo                { return nil }
func (p *caseFakeProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

func newCaseTestSelector(t *testing.T) *Selector {
	t.Helper()
	return NewWithProviders(config.LLMConfig{
		Default: "openrouter",
		Providers: map[string]config.LLMProviderConfig{
			"openrouter": {APIKey: "sk-x", Model: "qwen3.6"},
		},
	}, map[string]hexagon.Provider{
		"openrouter": &caseFakeProvider{name: "openrouter"},
	})
}

// UT-CASE-001: 配置 key 为小写时，Title Case 名必须能查到 Provider（bug 主症状）。
func TestBug20260703_GetCaseInsensitiveFallback(t *testing.T) {
	s := newCaseTestSelector(t)

	if _, ok := s.Get("openrouter"); !ok {
		t.Fatalf("精确匹配回归：Get(\"openrouter\") 应命中")
	}
	p, ok := s.Get("Openrouter")
	if !ok {
		t.Fatalf("BUG-20260703 复现：Get(\"Openrouter\") 未命中小写配置 key \"openrouter\"")
	}
	if p == nil || p.Name() != "openrouter" {
		t.Fatalf("Get(\"Openrouter\") 应返回 openrouter 实例，实际 %v", p)
	}
}

// UT-CASE-002: 两个仅大小写不同的 key 并存时，精确匹配优先（兜底不能抢占）。
func TestBug20260703_ExactMatchWins(t *testing.T) {
	lower := &caseFakeProvider{name: "openrouter"}
	title := &caseFakeProvider{name: "OpenRouter"}
	s := NewWithProviders(config.LLMConfig{
		Default: "openrouter",
		Providers: map[string]config.LLMProviderConfig{
			"openrouter": {APIKey: "sk-a", Model: "m-lower"},
			"OpenRouter": {APIKey: "sk-b", Model: "m-title"},
		},
	}, map[string]hexagon.Provider{
		"openrouter": lower,
		"OpenRouter": title,
	})

	p, ok := s.Get("OpenRouter")
	if !ok || p.Name() != "OpenRouter" {
		t.Fatalf("精确匹配必须优先：Get(\"OpenRouter\") 期望 title 实例，实际 ok=%v p=%v", ok, p)
	}
	p, ok = s.Get("openrouter")
	if !ok || p.Name() != "openrouter" {
		t.Fatalf("精确匹配必须优先：Get(\"openrouter\") 期望 lower 实例，实际 ok=%v p=%v", ok, p)
	}
}

// UT-CASE-003: ProviderModel 走同一 canonical 解析——否则 Get 兜底成功但模型偏好静默丢失（半修反模式）。
func TestBug20260703_ProviderModelCaseInsensitive(t *testing.T) {
	s := newCaseTestSelector(t)
	if got := s.ProviderModel("Openrouter"); got != "qwen3.6" {
		t.Fatalf("ProviderModel(\"Openrouter\") 期望 qwen3.6，实际 %q", got)
	}
}

// UT-CASE-004: ProviderConfig 同上。
func TestBug20260703_ProviderConfigCaseInsensitive(t *testing.T) {
	s := newCaseTestSelector(t)
	pc, ok := s.ProviderConfig("Openrouter")
	if !ok || pc.Model != "qwen3.6" {
		t.Fatalf("ProviderConfig(\"Openrouter\") 期望命中(qwen3.6)，实际 ok=%v model=%q", ok, pc.Model)
	}
}

// UT-CASE-005: MarkProviderUnhealthy 用 Title Case 名标记后，健康检查必须真正生效
// （engine/llm_call.go 以 fc.ProviderName 原样回调；大小写错位=熔断永不生效）。
func TestBug20260703_MarkUnhealthyCaseInsensitive(t *testing.T) {
	s := newCaseTestSelector(t)
	s.MarkProviderUnhealthy("Openrouter", "upstream 500", time.Minute)

	s.mu.RLock()
	healthy := s.isProviderHealthyLocked("openrouter")
	s.mu.RUnlock()
	if healthy {
		t.Fatalf("MarkProviderUnhealthy(\"Openrouter\") 未落到配置 key \"openrouter\"，健康标记静默失效")
	}
}

// UT-CASE-006（hex-test 举一反三/AP-144 穷举）：IsLocalProviderName 是同类按名访问器，
// 也须走 canonical 解析——否则本地 provider 用 Title Case 名查会漏掉 base_url 判定、
// 误分类为云端（影响 AP-098 本地慢模型超时放宽）。当前调用方都传 canonical 名故为
// 潜伏项，但导出方法应对任意大小写健壮。
func TestBug20260703_IsLocalProviderNameCaseInsensitive(t *testing.T) {
	// 本地 provider，配置 key 小写且名不含 "ollama" 子串（排除 substring 兜底路径），
	// base_url 指向 localhost → 只能靠 cfg 查表判本地。
	s := NewWithProviders(config.LLMConfig{
		Default: "localllm",
		Providers: map[string]config.LLMProviderConfig{
			"localllm": {APIKey: "x", Model: "m", BaseURL: "http://127.0.0.1:11434/v1"},
		},
	}, map[string]hexagon.Provider{
		"localllm": &caseFakeProvider{name: "localllm"},
	})

	if !s.IsLocalProviderName("localllm") {
		t.Fatalf("精确名应判本地（base_url=localhost）")
	}
	if !s.IsLocalProviderName("LocalLLM") {
		t.Fatalf("[AP-144] Title Case 名未走 canonical 解析，本地 provider 被误判为云端")
	}
}
