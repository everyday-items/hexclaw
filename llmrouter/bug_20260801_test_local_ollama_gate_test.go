package llmrouter

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260801_DefaultTestBinaryDoesNotAutoRegisterLocalOllama(t *testing.T) {
	t.Setenv("HEXCLAW_TEST_ALLOW_AUTO_LOCAL_OLLAMA", "")
	previousProbe := localOllamaReachable
	localOllamaReachable = func() bool { return true }
	t.Cleanup(func() { localOllamaReachable = previousProbe })

	enabled := true
	providers, activeConfig, _ := buildSelectorState(config.LLMConfig{
		Default: "remote-test",
		Providers: map[string]config.LLMProviderConfig{
			"remote-test": {APIKey: "test-key", BaseURL: "https://example.invalid/v1", Model: "test-model", Enabled: &enabled},
		},
	})

	if _, found := providers[localOllamaProviderName]; found {
		t.Fatalf("默认 Go 测试不得自动注册 %q；这会把单元测试升级为真实本机模型调用", localOllamaProviderName)
	}
	if _, found := activeConfig.Providers[localOllamaProviderName]; found {
		t.Fatalf("默认 Go 测试的 active config 不得注入 %q", localOllamaProviderName)
	}
}
