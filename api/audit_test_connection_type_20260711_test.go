package api

// hex-test 审计 · 契约#2：测试连接把 14 种 provider 一律当 OpenAI 协议打。
// llmTestProviderFactory 恒 hexagon.NewOpenAI，从不读 cfg.Type → anthropic(原生 /v1/messages)、
// gemini(/v1beta) 等被拼成 /chat/completions，有效 Key 也测不通/404。真实路由
// llmrouter.createProvider 是类型感知的，测试连接却没复用。
// RED：factory 对 anthropic/ollama 返回的 provider 仍是 openai → FAIL；
// GREEN：复用类型感知工厂后返回对应 provider。

import "testing"

func TestLLMTestProviderFactory_IsTypeAware_Contract2(t *testing.T) {
	cases := []struct {
		typ      string
		wantName string
	}{
		{"anthropic", "anthropic"},
		{"ollama", "ollama"},
		{"openai", "openai"},
		{"deepseek", "openai"}, // 声明 OpenAI 兼容的走 openai 正确
	}
	for _, c := range cases {
		p := llmTestProviderFactory(llmConnectionTestProvider{
			Type: c.typ, Model: "m", APIKey: "k", BaseURL: "http://127.0.0.1:1",
		})
		namer, ok := p.(interface{ Name() string })
		if !ok {
			t.Fatalf("type=%s: provider 未暴露 Name()，无法校验协议", c.typ)
		}
		if got := namer.Name(); got != c.wantName {
			t.Errorf("type=%s 应创建 %s 协议 provider，实际 %s（测试连接用错协议，有效 Key 也测不通）",
				c.typ, c.wantName, got)
		}
	}
}
