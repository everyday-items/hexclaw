package llmrouter

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// TestBug20260622_DisabledProviderSkippedInRouting 回归锁定 [BUG-PROVIDER-DISABLE]：
// provider.Enabled==false 时，配置/Key 仍持久化保留，但不得参与路由（buildSelectorState
// 不应为其建 provider 实例，activeCfg 也不应含它）。Enabled==nil 视为启用（向后兼容）。
func TestBug20260622_DisabledProviderSkippedInRouting(t *testing.T) {
	enabled, disabled := true, false
	cfg := config.LLMConfig{
		Default: "openai",
		Providers: map[string]config.LLMProviderConfig{
			"openai":   {APIKey: "sk-aaa", Enabled: &enabled},
			"deepseek": {APIKey: "sk-bbb", BaseURL: "https://api.deepseek.com", Enabled: &disabled},
			"moonshot": {APIKey: "sk-ccc"}, // Enabled=nil → 视为启用
		},
	}

	providers, activeCfg, _ := buildSelectorState(cfg)

	if _, ok := providers["deepseek"]; ok {
		t.Error("禁用 provider 不应进入路由 selector")
	}
	if _, ok := activeCfg.Providers["deepseek"]; ok {
		t.Error("禁用 provider 不应出现在 activeCfg.Providers")
	}
	if _, ok := providers["openai"]; !ok {
		t.Error("启用 provider 应在路由中")
	}
	if _, ok := providers["moonshot"]; !ok {
		t.Error("Enabled=nil 应视为启用")
	}
}
