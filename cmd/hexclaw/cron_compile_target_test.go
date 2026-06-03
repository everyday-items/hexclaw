package main

import (
	"context"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
)

// stubProvider 是满足 hexagon.Provider（=ai-core/llm.Provider）的最小桩，仅供选型单测用。
type stubProvider struct{ name string }

func (s stubProvider) Name() string { return s.name }
func (s stubProvider) Complete(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return nil, nil
}
func (s stubProvider) Stream(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
	return nil, nil
}
func (s stubProvider) Models() []llm.ModelInfo                  { return nil }
func (s stubProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

// fakeRouter 实现 cronRouterView，按用例注入 providers / default。
type fakeRouter struct {
	defName   string
	configs   map[string]config.LLMProviderConfig
	available map[string]bool // 缺省视为可用；显式 false 模拟 provider 创建失败
}

func (f fakeRouter) DefaultName() string { return f.defName }

func (f fakeRouter) Providers() []string {
	out := make([]string, 0, len(f.configs))
	for name := range f.configs {
		out = append(out, name)
	}
	return out
}

func (f fakeRouter) ProviderConfig(name string) (config.LLMProviderConfig, bool) {
	pc, ok := f.configs[name]
	return pc, ok
}

func (f fakeRouter) Get(name string) (hexagon.Provider, bool) {
	if _, ok := f.configs[name]; !ok {
		return nil, false
	}
	if f.available != nil {
		if up, set := f.available[name]; set && !up {
			return nil, false
		}
	}
	return stubProvider{name: name}, true
}

const (
	ollamaBase = "http://localhost:11434/v1"
	zhipuBase  = "https://open.bigmodel.cn/api/paas/v4"
)

// TestPickCronCompileTarget 钉死「远程 chat 优先、本地兜底」选型优先级
// （用户决策 2026-05-29：cron 编译走可用的远程快模型，对话仍用本地默认）。
func TestPickCronCompileTarget(t *testing.T) {
	tests := []struct {
		name      string
		router    fakeRouter
		wantName  string // 期望选中的 provider 名；"" 表示期望 error
		wantModel string
	}{
		{
			name: "默认本地+智谱远程chat → 选远程glm（核心场景）",
			router: fakeRouter{
				defName: "Ollama (本地)",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
					"智谱 AI":        {BaseURL: zhipuBase, Model: "glm-5.1"},
				},
			},
			wantName:  "智谱 AI",
			wantModel: "glm-5.1",
		},
		{
			name: "默认就是远程chat → 直接用默认（尊重显式选择）",
			router: fakeRouter{
				defName: "智谱 AI",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
					"智谱 AI":        {BaseURL: zhipuBase, Model: "glm-5.1"},
				},
			},
			wantName:  "智谱 AI",
			wantModel: "glm-5.1",
		},
		{
			name: "只有本地 → 退回本地默认（慢路径，运行时友好报错）",
			router: fakeRouter{
				defName: "Ollama (本地)",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
				},
			},
			wantName:  "Ollama (本地)",
			wantModel: "qwen3.5:9b",
		},
		{
			name: "默认本地+远程是图像模型cogview → 跳过远程，退回本地",
			router: fakeRouter{
				defName: "Ollama (本地)",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
					"智谱 AI":        {BaseURL: zhipuBase, Model: "cogview-4"},
				},
			},
			wantName:  "Ollama (本地)",
			wantModel: "qwen3.5:9b",
		},
		{
			name: "默认本地+多个远程chat → 选名字排序最前的（确定性）",
			router: fakeRouter{
				defName: "Ollama (本地)",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
					"智谱 AI":        {BaseURL: zhipuBase, Model: "glm-5.1"},
					"DeepSeek":     {BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
				},
			},
			wantName:  "DeepSeek", // ASCII 'D' < 中文，sort.Strings 字节序在前
			wantModel: "deepseek-chat",
		},
		{
			name: "默认本地+远程不可用（无key）→ 退回本地默认",
			router: fakeRouter{
				defName: "Ollama (本地)",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
					"智谱 AI":        {BaseURL: zhipuBase, Model: "glm-5.1"},
				},
				available: map[string]bool{"智谱 AI": false},
			},
			wantName:  "Ollama (本地)",
			wantModel: "qwen3.5:9b",
		},
		{
			name: "未配置默认 → error",
			router: fakeRouter{
				defName: "",
				configs: map[string]config.LLMProviderConfig{},
			},
			wantName: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, model, err := pickCronCompileTarget(tc.router)
			if tc.wantName == "" {
				if err == nil {
					t.Fatalf("期望 error，得到 provider=%v model=%q", p, model)
				}
				return
			}
			if err != nil {
				t.Fatalf("非预期 error: %v", err)
			}
			if p == nil {
				t.Fatal("provider 不应为 nil")
			}
			if got := p.Name(); got != tc.wantName {
				t.Errorf("provider name = %q，期望 %q", got, tc.wantName)
			}
			if model != tc.wantModel {
				t.Errorf("model = %q，期望 %q", model, tc.wantModel)
			}
		})
	}
}
