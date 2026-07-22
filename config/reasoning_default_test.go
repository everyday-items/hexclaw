package config

import "testing"

// TestApplyReasoningDefault_PicksCloudStrongText 未配 reasoning_provider 时，兜底指向云端强文本
// provider（BUG-20260712 治本 #5）——避免解题/批改/热身落到视觉/本地弱模型。
func TestApplyReasoningDefault_PicksCloudStrongText(t *testing.T) {
	c := &Config{}
	c.LLM.Providers = map[string]LLMProviderConfig{
		"ollama-local": {BaseURL: "http://localhost:11434/v1", Model: "qwen3.5:9b"},
		"deepseek":     {APIKey: "sk-x", Model: "deepseek-chat"},
	}
	chosen, applied := c.ApplyReasoningDefault()
	if !applied || chosen != "deepseek" {
		t.Fatalf("应兜底到云端 deepseek, got chosen=%q applied=%v", chosen, applied)
	}
	if c.LLM.ReasoningProvider != "deepseek" {
		t.Errorf("ReasoningProvider 应被填为 deepseek, got %q", c.LLM.ReasoningProvider)
	}
}

// TestApplyReasoningDefault_RespectsExplicit 已显式配置 → 不覆盖（尊重契约，无回归）。
func TestApplyReasoningDefault_RespectsExplicit(t *testing.T) {
	c := &Config{}
	c.LLM.ReasoningProvider = "my-strong"
	c.LLM.Providers = map[string]LLMProviderConfig{"deepseek": {APIKey: "sk-x"}}
	if _, applied := c.ApplyReasoningDefault(); applied {
		t.Error("已显式配置不应被兜底覆盖")
	}
	if c.LLM.ReasoningProvider != "my-strong" {
		t.Errorf("显式值被改: %q", c.LLM.ReasoningProvider)
	}
}

// TestApplyReasoningDefault_OnlyLocalNoop 只有本地 provider → 不兜底（不把弱模型钉成 reasoning）。
func TestApplyReasoningDefault_OnlyLocalNoop(t *testing.T) {
	c := &Config{}
	c.LLM.Providers = map[string]LLMProviderConfig{
		"ollama": {BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen3.5:9b"},
	}
	if chosen, applied := c.ApplyReasoningDefault(); applied {
		t.Errorf("只有本地 provider 不应兜底, got %q", chosen)
	}
	if c.LLM.ReasoningProvider != "" {
		t.Errorf("不应填 ReasoningProvider, got %q", c.LLM.ReasoningProvider)
	}
}

// TestApplyReasoningDefault_SkipsDisabled 被禁用的云端 provider 不参与兜底。
func TestApplyReasoningDefault_SkipsDisabled(t *testing.T) {
	c := &Config{}
	c.LLM.Providers = map[string]LLMProviderConfig{
		"deepseek": {APIKey: "sk-x", Enabled: boolPtr(false)},
		"openai":   {APIKey: "sk-y"},
	}
	chosen, applied := c.ApplyReasoningDefault()
	if !applied || chosen != "openai" {
		t.Fatalf("应跳过禁用的 deepseek 选 openai, got %q applied=%v", chosen, applied)
	}
}

// reasoning_provider 是一个跨字段引用。配置一旦保存了该引用，就不能在 provider 被删后
// 留下悬空值，否则 solve 会静默掉回默认（常见是本地 Ollama），把“强推理模型”契约悄悄改掉。
func TestValidate_RejectsDanglingReasoningProvider(t *testing.T) {
	c := DefaultConfig()
	c.LLM.Default = "ollama"
	c.LLM.Providers = map[string]LLMProviderConfig{
		"ollama": {BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen3.5:9b"},
	}
	c.LLM.ReasoningProvider = "deleted-cloud"
	c.LLM.ReasoningModel = "gpt-5.6-sol"

	if err := c.Validate(); err == nil {
		t.Fatal("dangling reasoning_provider must be an explicit configuration error")
	}
}
