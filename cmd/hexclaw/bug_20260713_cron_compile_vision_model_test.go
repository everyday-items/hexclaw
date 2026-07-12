package main

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// BUG-20260713：定时任务(cron)编译落到视觉模型 → 智谱强制 max_tokens≤1024，编译器发 4096 → 400 建任务失败。
//
// 根因：默认 provider(智谱 AI)的默认 model 是视觉模型 glm-4v-flash（K12 识题 RouteForVision 故意复用
// 「默认 provider model」当视觉模型）。cron 编译 pickCronCompileTarget 取默认 provider 的默认 model →
// 也拿到 glm-4v-flash。治本：cron 编译优先用配置的 reasoning provider+model（智谱/glm-4.5，真 chat 模型）。
func TestPickCronCompileTarget_ReasoningModel(t *testing.T) {
	tests := []struct {
		name              string
		router            fakeRouter
		reasoningProvider string
		reasoningModel    string
		wantName          string
		wantModel         string
	}{
		{
			name: "默认provider默认model=视觉glm-4v-flash + 配reasoning=glm-4.5 → 用glm-4.5(核心场景)",
			router: fakeRouter{
				defName: "智谱 AI",
				configs: map[string]config.LLMProviderConfig{
					"智谱 AI": {BaseURL: zhipuBase, Model: "glm-4v-flash"},
				},
			},
			reasoningProvider: "智谱 AI",
			reasoningModel:    "glm-4.5",
			wantName:          "智谱 AI",
			wantModel:         "glm-4.5", // 不能是 glm-4v-flash
		},
		{
			name: "reasoning provider 与默认不同 → 走 reasoning 的 provider+model",
			router: fakeRouter{
				defName: "智谱 AI",
				configs: map[string]config.LLMProviderConfig{
					"智谱 AI":    {BaseURL: zhipuBase, Model: "glm-4v-flash"},
					"DeepSeek": {BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
				},
			},
			reasoningProvider: "DeepSeek",
			reasoningModel:    "deepseek-reasoner",
			wantName:          "DeepSeek",
			wantModel:         "deepseek-reasoner",
		},
		{
			name: "配了 reasoning provider 但未配 reasoning model → 回退该 provider 的默认 model(仍过 chat 校验)",
			router: fakeRouter{
				defName: "Ollama (本地)",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
					"DeepSeek":    {BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
				},
			},
			reasoningProvider: "DeepSeek",
			reasoningModel:    "",
			wantName:          "DeepSeek",
			wantModel:         "deepseek-chat",
		},
		{
			name: "reasoning model 是视觉模型 → 拒绝 reasoning，回退现有远程chat优先逻辑",
			router: fakeRouter{
				defName: "Ollama (本地)",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
					"智谱 AI":       {BaseURL: zhipuBase, Model: "glm-4.5"},
				},
			},
			reasoningProvider: "智谱 AI",
			reasoningModel:    "glm-4v-flash", // 非法：视觉
			wantName:          "智谱 AI",        // 回退逻辑挑远程 chat → 智谱默认 model glm-4.5
			wantModel:         "glm-4.5",
		},
		{
			name: "未配 reasoning → 完全走原有远程chat优先逻辑(无回归)",
			router: fakeRouter{
				defName: "Ollama (本地)",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
					"智谱 AI":       {BaseURL: zhipuBase, Model: "glm-5.1"},
				},
			},
			reasoningProvider: "",
			reasoningModel:    "",
			wantName:          "智谱 AI",
			wantModel:         "glm-5.1",
		},
		{
			name: "reasoning provider 不可用(无key/创建失败) → 回退现有逻辑",
			router: fakeRouter{
				defName: "Ollama (本地)",
				configs: map[string]config.LLMProviderConfig{
					"Ollama (本地)": {BaseURL: ollamaBase, Model: "qwen3.5:9b"},
					"智谱 AI":       {BaseURL: zhipuBase, Model: "glm-4.5"},
				},
				available: map[string]bool{"智谱 AI": false},
			},
			reasoningProvider: "智谱 AI",
			reasoningModel:    "glm-4.5",
			wantName:          "Ollama (本地)", // 远程不可用 → 退回本地默认
			wantModel:         "qwen3.5:9b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, model, err := pickCronCompileTarget(tc.router, tc.reasoningProvider, tc.reasoningModel)
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

// TestIsNonChatModel_CoversVisionModels 钉死：视觉/图像/嵌入/语音等非 chat 模型被识别为 true；
// 正常 chat 模型（含名字里带 v/含 4 的）不被误伤为 true。
func TestIsNonChatModel_CoversVisionModels(t *testing.T) {
	nonChat := []string{
		"glm-4v-flash", "glm-4v", "GLM-4V-PLUS",
		"qwen-vl-max", "qwen3-vl", "qwen2.5-vl-72b-instruct",
		"llava:13b", "llava-1.6",
		"minicpm-v", "MiniCPM-V-2.6",
		"llama3.2-vision", "llama-3.2-90b-vision-instruct",
		"gpt-4-vision-preview",
		"cogview-4", "dall-e-3", "text-embedding-3-small", "whisper-1", "cogvideo",
	}
	for _, m := range nonChat {
		if !isNonChatModel(m) {
			t.Errorf("isNonChatModel(%q) = false，期望 true（非 chat 模型应被拦截）", m)
		}
	}

	chat := []string{
		"glm-4.5", "glm-4-flash", "glm-5.1", "glm-4-plus",
		"qwen3.5:9b", "qwen-max", "qwen2.5-72b-instruct",
		"deepseek-chat", "deepseek-reasoner",
		"claude-sonnet-4-6", "gpt-4o", "gpt-4-turbo",
		"moonshot-v1-8k", // 名字含 "v1" 但是 chat；不能因 "v" 误伤
	}
	for _, m := range chat {
		if isNonChatModel(m) {
			t.Errorf("isNonChatModel(%q) = true，期望 false（正常 chat 模型被误伤）", m)
		}
	}
}
