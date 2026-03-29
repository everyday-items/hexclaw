package engine

import (
	"testing"

	"github.com/hexagon-codes/ai-core/tokenizer"
	"github.com/hexagon-codes/hexagon"
)

func TestProviderToTokenizerModel(t *testing.T) {
	tests := []struct {
		provider string
		want     tokenizer.Model
	}{
		{"openai", tokenizer.GPT4o},
		{"anthropic", tokenizer.Claude3},
		{"google", tokenizer.Gemini},
		{"deepseek", tokenizer.DeepSeek},
		{"qwen", tokenizer.Qwen},
		{"dashscope", tokenizer.Qwen},
		{"unknown-provider", ""},
	}
	for _, tt := range tests {
		if got := providerToTokenizerModel(tt.provider); got != tt.want {
			t.Errorf("providerToTokenizerModel(%q) = %q, want %q", tt.provider, got, tt.want)
		}
	}
}

func TestEstimateResponseUsage(t *testing.T) {
	messages := []hexagon.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello, how are you?"},
	}
	completion := "I'm doing great, thank you for asking!"

	prompt, comp, total := estimateResponseUsage("openai", messages, completion)

	if prompt <= 0 {
		t.Errorf("expected positive prompt tokens, got %d", prompt)
	}
	if comp <= 0 {
		t.Errorf("expected positive completion tokens, got %d", comp)
	}
	if total != prompt+comp {
		t.Errorf("total (%d) should equal prompt (%d) + completion (%d)", total, prompt, comp)
	}
}

func TestEstimateResponseUsage_UnknownProvider(t *testing.T) {
	messages := []hexagon.Message{
		{Role: "user", Content: "Hello"},
	}
	prompt, comp, total := estimateResponseUsage("some-unknown-provider", messages, "Hi there")

	if total <= 0 {
		t.Errorf("unknown provider should still produce estimates, got total=%d", total)
	}
	if total != prompt+comp {
		t.Errorf("total (%d) != prompt (%d) + completion (%d)", total, prompt, comp)
	}
}

func TestEstimateResponseUsage_EmptyCompletion(t *testing.T) {
	messages := []hexagon.Message{
		{Role: "user", Content: "Hello"},
	}
	prompt, comp, total := estimateResponseUsage("openai", messages, "")

	if prompt <= 0 {
		t.Errorf("expected positive prompt tokens even with empty completion, got %d", prompt)
	}
	if comp != 0 {
		t.Errorf("expected 0 completion tokens for empty content, got %d", comp)
	}
	if total != prompt {
		t.Errorf("total (%d) should equal prompt (%d) when completion is empty", total, prompt)
	}
}

func TestEstimateResponseUsage_NilMessages(t *testing.T) {
	// Streaming paths may call with nil messages
	_, comp, total := estimateResponseUsage("anthropic", nil, "Some response content here")

	if comp <= 0 {
		t.Errorf("expected positive completion tokens, got %d", comp)
	}
	// With nil messages, prompt should be minimal (just the 3-token reply overhead)
	if total < comp {
		t.Errorf("total (%d) should be >= completion (%d)", total, comp)
	}
}

func TestEstimateResponseUsage_ChineseContent(t *testing.T) {
	messages := []hexagon.Message{
		{Role: "user", Content: "你好，请介绍一下自己"},
	}
	prompt, comp, total := estimateResponseUsage("deepseek", messages, "我是一个人工智能助手，很高兴为您服务。")

	if prompt <= 0 || comp <= 0 || total <= 0 {
		t.Errorf("Chinese content should produce positive estimates: prompt=%d, comp=%d, total=%d", prompt, comp, total)
	}
}
