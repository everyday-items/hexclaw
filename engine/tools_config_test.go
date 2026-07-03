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
		{"自动-本地默认开启", config.LLMToolsConfig{Enabled: "auto"}, true, true},
		{"空值等同auto-云端", config.LLMToolsConfig{}, false, true},
		{"空值等同auto-本地", config.LLMToolsConfig{}, true, true},
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

func TestResolveToolsEnabledForMessage(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.LLMToolsConfig
		metadata map[string]string
		expected bool
	}{
		{
			name:     "request off overrides global on",
			cfg:      config.LLMToolsConfig{Enabled: "on"},
			metadata: map[string]string{"tools_enabled": "off"},
			expected: false,
		},
		{
			name:     "request false disables tools",
			cfg:      config.LLMToolsConfig{Enabled: "on"},
			metadata: map[string]string{"tools_enabled": "false"},
			expected: false,
		},
		{
			name:     "request on overrides global off",
			cfg:      config.LLMToolsConfig{Enabled: "off"},
			metadata: map[string]string{"tools_enabled": "on"},
			expected: true,
		},
		{
			name:     "empty metadata follows global",
			cfg:      config.LLMToolsConfig{Enabled: "off"},
			metadata: nil,
			expected: false,
		},
		{
			name:     "unknown metadata follows global",
			cfg:      config.LLMToolsConfig{Enabled: "auto"},
			metadata: map[string]string{"tools_enabled": "maybe"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveToolsEnabledForMessage(tt.cfg, false, tt.metadata)
			if got != tt.expected {
				t.Errorf("resolveToolsEnabledForMessage(%+v, metadata=%v) = %v, want %v",
					tt.cfg, tt.metadata, got, tt.expected)
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
