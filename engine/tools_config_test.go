package engine

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestResolveToolsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.LLMToolsConfig
		isLocal  bool
		expected bool
	}{
		{"全局开启-云端", config.LLMToolsConfig{Enabled: "on"}, false, true},
		{"全局开启-本地", config.LLMToolsConfig{Enabled: "on"}, true, true},
		{"全局关闭-云端", config.LLMToolsConfig{Enabled: "off"}, false, false},
		{"全局关闭-本地", config.LLMToolsConfig{Enabled: "off"}, true, false},
		{"自动-云端默认开启", config.LLMToolsConfig{Enabled: "auto"}, false, true},
		{"自动-本地默认关闭", config.LLMToolsConfig{Enabled: "auto"}, true, false},
		{"空值等同auto-云端", config.LLMToolsConfig{}, false, true},
		{"空值等同auto-本地", config.LLMToolsConfig{}, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveToolsEnabled(tt.cfg, tt.isLocal)
			if got != tt.expected {
				t.Errorf("resolveToolsEnabled(%+v, isLocal=%v) = %v, want %v",
					tt.cfg, tt.isLocal, got, tt.expected)
			}
		})
	}
}

func TestIsLocalProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		expected bool
	}{
		{"Ollama (本地)", "Ollama (本地)", true},
		{"ollama", "ollama", true},
		{"My Local LLM", "My Local LLM", true},
		{"OpenAI", "OpenAI", false},
		{"智谱 AI", "智谱 AI", false},
		{"DeepSeek", "DeepSeek", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLocalProvider(tt.provider)
			if got != tt.expected {
				t.Errorf("isLocalProvider(%q) = %v, want %v", tt.provider, got, tt.expected)
			}
		})
	}
}
