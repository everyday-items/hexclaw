package llmrouter

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// BUG-20260712 provider 韧性回归锁（治本·第 1 部分）：
// 本地 Ollama 在运行、但配置里没有任何本地 provider（配置漂移/被 e2e 测试或误操作抹掉）时，
// buildSelectorState 应自动注册默认「Ollama (本地)」，让绑定本地模型的 agent 不再因缺 provider
// 硬崩「provider 不存在」。已有本地 provider 则尊重配置不覆盖；Ollama 未运行则不注册。

func TestBuildSelector_AutoRegistersLocalOllamaWhenRunning(t *testing.T) {
	prev := localOllamaReachable
	localOllamaReachable = func() bool { return true }
	defer func() { localOllamaReachable = prev }()

	enabled := true
	cfg := config.LLMConfig{
		Default: "智谱 AI",
		Providers: map[string]config.LLMProviderConfig{
			// 只有云端 provider（模拟 e2e 污染后 zhipu-only、Ollama 被抹）
			"智谱 AI": {APIKey: "x", BaseURL: "https://open.bigmodel.cn/api/paas/v4", Model: "glm-4v-flash", Enabled: &enabled},
		},
	}
	providers, activeCfg, _ := buildSelectorState(cfg)

	// GREEN：ollama 在跑 + 配置无本地 provider → 自动注册。RED（修前）：无此逻辑 → 不含。
	if _, ok := providers[localOllamaProviderName]; !ok {
		t.Fatalf("ollama 在跑 + 配置无本地 provider → 应自动注册 %q，实际 providers=%v", localOllamaProviderName, providerKeys(providers))
	}
	if _, ok := activeCfg.Providers[localOllamaProviderName]; !ok {
		t.Fatalf("activeCfg.Providers 应含自动注册的本地 provider")
	}
	// 云端 provider 仍在（自动注册是增量、不替换）
	if _, ok := providers["智谱 AI"]; !ok {
		t.Fatalf("云端 provider 不应被影响")
	}
}

func TestBuildSelector_NoAutoOllamaWhenNotRunning(t *testing.T) {
	prev := localOllamaReachable
	localOllamaReachable = func() bool { return false }
	defer func() { localOllamaReachable = prev }()

	enabled := true
	cfg := config.LLMConfig{
		Default: "智谱 AI",
		Providers: map[string]config.LLMProviderConfig{
			"智谱 AI": {APIKey: "x", BaseURL: "https://open.bigmodel.cn/api/paas/v4", Model: "m", Enabled: &enabled},
		},
	}
	providers, _, _ := buildSelectorState(cfg)

	if _, ok := providers[localOllamaProviderName]; ok {
		t.Fatalf("ollama 未运行不应自动注册本地 provider（否则请求会连接失败误导用户）")
	}
}

func TestBuildSelector_RespectsConfiguredLocalProvider(t *testing.T) {
	prev := localOllamaReachable
	localOllamaReachable = func() bool { return true }
	defer func() { localOllamaReachable = prev }()

	enabled := true
	cfg := config.LLMConfig{
		Default: localOllamaProviderName,
		Providers: map[string]config.LLMProviderConfig{
			// 用户已配置本地 provider（自定义模型）→ 不得被默认覆盖
			localOllamaProviderName: {BaseURL: "http://localhost:11434/v1", Model: "qwen-custom", Enabled: &enabled},
		},
	}
	_, activeCfg, _ := buildSelectorState(cfg)

	if got := activeCfg.Providers[localOllamaProviderName].Model; got != "qwen-custom" {
		t.Fatalf("已有本地 provider 不应被默认覆盖，got model=%q（应保留用户 qwen-custom）", got)
	}
}

func providerKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
