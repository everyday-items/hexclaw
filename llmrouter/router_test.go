package llmrouter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
)

func TestNew_NoProviders(t *testing.T) {
	cfg := config.LLMConfig{
		Default:   "deepseek",
		Providers: map[string]config.LLMProviderConfig{},
	}

	_, err := New(cfg)
	if err == nil {
		t.Fatal("没有可用 Provider 时应返回错误")
	}
}

func TestNew_SkipEmptyAPIKey(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "openai",
		Providers: map[string]config.LLMProviderConfig{
			"openai":   {APIKey: "", Model: "gpt-4o"},               // 空 key，跳过
			"deepseek": {APIKey: "sk-test", Model: "deepseek-chat"}, // 有 key
		},
	}

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("创建路由器失败: %v", err)
	}

	// 只应加载 deepseek
	if len(r.Providers()) != 1 {
		t.Errorf("期望 1 个 Provider，得到 %d", len(r.Providers()))
	}

	// 默认应自动切换到 deepseek
	if r.DefaultName() != "deepseek" {
		t.Errorf("默认 Provider 应切换到 deepseek，得到 %s", r.DefaultName())
	}
}

func TestRouter_Route(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "test-provider",
		Providers: map[string]config.LLMProviderConfig{
			"test-provider": {APIKey: "sk-test", Model: "test-model"},
		},
	}

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("创建路由器失败: %v", err)
	}

	p, name, err := r.Route(context.Background())
	if err != nil {
		t.Fatalf("路由失败: %v", err)
	}
	if name != "test-provider" {
		t.Errorf("期望 test-provider，得到 %s", name)
	}
	if p == nil {
		t.Error("Provider 不应为 nil")
	}
}

func TestRouter_Fallback(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "primary",
		Providers: map[string]config.LLMProviderConfig{
			"primary":  {APIKey: "sk-1", Model: "model-1"},
			"fallback": {APIKey: "sk-2", Model: "model-2"},
		},
	}

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("创建路由器失败: %v", err)
	}

	// 排除 primary，应返回 fallback
	p, name, err := r.Fallback("primary")
	if err != nil {
		t.Fatalf("降级失败: %v", err)
	}
	if name != "fallback" {
		t.Errorf("期望 fallback，得到 %s", name)
	}
	if p == nil {
		t.Error("降级 Provider 不应为 nil")
	}

	// 如果只有一个 Provider，排除后无可用备用
	_, _, err = r.Fallback("fallback")
	if err != nil {
		// primary 还在，所以不会报错
	}
}

// newTestSelectorDirect 创建测试用的 Selector（直接构造，跳过 Provider 创建）
//
// 由于真实 Provider 需要 API Key，测试路由选择逻辑时使用 nil Provider 占位。
func newTestSelectorDirect(providers []string, defaultP string, routing config.LLMRoutingConfig) *Selector {
	s := &Selector{
		providers: make(map[string]hexagon.Provider),
		cfg: config.LLMConfig{
			Default: defaultP,
			Routing: routing,
		},
		defaultP: defaultP,
	}
	for _, name := range providers {
		s.providers[name] = nil
	}
	return s
}

// TestRoute_DefaultStrategy 测试默认策略（路由未启用）返回默认 Provider
func TestRoute_DefaultStrategy(t *testing.T) {
	s := newTestSelectorDirect(
		[]string{"openai", "deepseek"},
		"openai",
		config.LLMRoutingConfig{Enabled: false},
	)

	_, name, err := s.Route(context.Background())
	if err != nil {
		t.Fatalf("Route 返回错误: %v", err)
	}
	if name != "openai" {
		t.Errorf("期望 openai，得到 %s", name)
	}
}

// TestRoute_CostAware 测试 cost-aware 策略选择最低成本 Provider
func TestRoute_CostAware(t *testing.T) {
	s := newTestSelectorDirect(
		[]string{"openai", "deepseek", "anthropic"},
		"openai",
		config.LLMRoutingConfig{Enabled: true, Strategy: "cost-aware"},
	)

	_, name, err := s.Route(context.Background())
	if err != nil {
		t.Fatalf("Route 返回错误: %v", err)
	}
	if name != "deepseek" {
		t.Errorf("cost-aware 期望 deepseek，得到 %s", name)
	}
}

// TestRoute_CostAwareWithOllama 测试有 ollama 时 cost-aware 选择 ollama
func TestRoute_CostAwareWithOllama(t *testing.T) {
	s := newTestSelectorDirect(
		[]string{"openai", "deepseek", "ollama"},
		"openai",
		config.LLMRoutingConfig{Enabled: true, Strategy: "cost-aware"},
	)

	_, name, err := s.Route(context.Background())
	if err != nil {
		t.Fatalf("Route 返回错误: %v", err)
	}
	if name != "ollama" {
		t.Errorf("cost-aware 期望 ollama，得到 %s", name)
	}
}

// TestRoute_QualityFirst 测试 quality-first 策略选择最高质量 Provider
func TestRoute_QualityFirst(t *testing.T) {
	s := newTestSelectorDirect(
		[]string{"openai", "deepseek", "anthropic"},
		"deepseek",
		config.LLMRoutingConfig{Enabled: true, Strategy: "quality-first"},
	)

	_, name, err := s.Route(context.Background())
	if err != nil {
		t.Fatalf("Route 返回错误: %v", err)
	}
	if name != "anthropic" {
		t.Errorf("quality-first 期望 anthropic，得到 %s", name)
	}
}

// TestRoute_QualityFirstWithoutAnthropic 测试没有 anthropic 时 quality-first 选择 openai
func TestRoute_QualityFirstWithoutAnthropic(t *testing.T) {
	s := newTestSelectorDirect(
		[]string{"openai", "deepseek", "ollama"},
		"deepseek",
		config.LLMRoutingConfig{Enabled: true, Strategy: "quality-first"},
	)

	_, name, err := s.Route(context.Background())
	if err != nil {
		t.Fatalf("Route 返回错误: %v", err)
	}
	if name != "openai" {
		t.Errorf("quality-first 期望 openai，得到 %s", name)
	}
}

// TestRoute_LatencyFirst 测试 latency-first 策略选择最低延迟 Provider
func TestRoute_LatencyFirst(t *testing.T) {
	s := newTestSelectorDirect(
		[]string{"openai", "deepseek", "anthropic"},
		"anthropic",
		config.LLMRoutingConfig{Enabled: true, Strategy: "latency-first"},
	)

	_, name, err := s.Route(context.Background())
	if err != nil {
		t.Fatalf("Route 返回错误: %v", err)
	}
	if name != "deepseek" {
		t.Errorf("latency-first 期望 deepseek，得到 %s", name)
	}
}

// TestRoute_UnknownStrategy 测试未知策略回退到默认 Provider
func TestRoute_UnknownStrategy(t *testing.T) {
	s := newTestSelectorDirect(
		[]string{"openai", "deepseek"},
		"openai",
		config.LLMRoutingConfig{Enabled: true, Strategy: "random"},
	)

	_, name, err := s.Route(context.Background())
	if err != nil {
		t.Fatalf("Route 返回错误: %v", err)
	}
	if name != "openai" {
		t.Errorf("未知策略期望回退到 openai，得到 %s", name)
	}
}

// TestFallback_Excludes 测试 Fallback 排除指定 Provider
func TestFallback_Excludes(t *testing.T) {
	s := newTestSelectorDirect(
		[]string{"openai", "deepseek"},
		"openai",
		config.LLMRoutingConfig{},
	)

	_, name, err := s.Fallback("openai")
	if err != nil {
		t.Fatalf("Fallback 返回错误: %v", err)
	}
	if name == "openai" {
		t.Error("Fallback 不应返回被排除的 openai")
	}
	if name != "deepseek" {
		t.Errorf("Fallback 期望 deepseek，得到 %s", name)
	}
}

func TestSelector_ReloadUpdatesActiveConfig(t *testing.T) {
	s := NewWithProviders(config.LLMConfig{
		Default: "openai",
		Providers: map[string]config.LLMProviderConfig{
			"openai": {APIKey: "sk-openai", Model: "gpt-4o"},
		},
	}, map[string]hexagon.Provider{
		"openai": nil,
	})

	next := config.LLMConfig{
		Default: "智谱",
		Providers: map[string]config.LLMProviderConfig{
			"智谱": {APIKey: "sk-zhipu", Model: "glm-5"},
		},
	}
	if err := s.Reload(next); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}

	active := s.ActiveConfig()
	if active.Default != "智谱" {
		t.Fatalf("期望默认 provider 为智谱，实际 %q", active.Default)
	}
	if got := s.ProviderModel("智谱"); got != "glm-5" {
		t.Fatalf("期望 provider model 为 glm-5，实际 %q", got)
	}
	if _, ok := active.Providers["openai"]; ok {
		t.Fatalf("旧 provider 不应保留: %+v", active.Providers)
	}
}

// TestFallback_NoAlternative 测试只有一个 Provider 时 Fallback 失败
func TestFallback_NoAlternative(t *testing.T) {
	s := newTestSelectorDirect(
		[]string{"openai"},
		"openai",
		config.LLMRoutingConfig{},
	)

	_, _, err := s.Fallback("openai")
	if err == nil {
		t.Error("只有一个 Provider 时 Fallback 应返回错误")
	}
}

func TestRouter_WithBaseURL(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "proxy",
		Providers: map[string]config.LLMProviderConfig{
			"proxy": {
				APIKey:     "sk-proxy",
				BaseURL:    "https://my-proxy.example.com/v1",
				Model:      "gpt-4o",
				Compatible: "openai",
			},
		},
	}

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("创建路由器失败: %v", err)
	}

	p, ok := r.Get("proxy")
	if !ok {
		t.Fatal("应找到 proxy Provider")
	}
	if p == nil {
		t.Fatal("Provider 不应为 nil")
	}
}
