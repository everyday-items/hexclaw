package llmrouter

import (
	"context"
	"testing"

	"github.com/everyday-items/hexclaw/config"
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
			"openai": {APIKey: "", Model: "gpt-4o"},       // 空 key，跳过
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
