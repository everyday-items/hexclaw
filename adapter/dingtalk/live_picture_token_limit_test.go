package dingtalk

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

func TestLiveVisionMaxTokens_ZhipuGLM4VFlashIsCapped(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{"智谱 AI", "zhipu"} {
		if got := liveVisionMaxTokens(provider, "glm-4v-flash", ""); got != 1024 {
			t.Errorf("provider=%q glm-4v-flash max tokens = %d, want 1024", provider, got)
		}
	}
}

func TestLiveVisionMaxTokens_OtherModelsKeepWholePageDefault(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		provider string
		model    string
	}{
		{provider: "智谱 AI", model: "glm-4.5"},
		{provider: "openrouter", model: "qwen/qwen3-vl-235b-a22b-instruct"},
	} {
		if got := liveVisionMaxTokens(tc.provider, tc.model, ""); got != 4000 {
			t.Errorf("provider=%q model=%q max tokens = %d, want 4000", tc.provider, tc.model, got)
		}
	}
}

func TestLiveVisionMaxTokens_ExplicitOverrideMustBePositiveAndObeysModelCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		model    string
		override string
		want     int
	}{
		{name: "positive", provider: "openrouter", model: "vision-model", override: "768", want: 768},
		{name: "whitespace", provider: "openrouter", model: "vision-model", override: " 512 ", want: 512},
		{name: "zero ignored", provider: "openrouter", model: "vision-model", override: "0", want: 4000},
		{name: "negative ignored", provider: "openrouter", model: "vision-model", override: "-1", want: 4000},
		{name: "not integer ignored", provider: "openrouter", model: "vision-model", override: "many", want: 4000},
		{name: "known cap wins", provider: "智谱 AI", model: "glm-4v-flash", override: "4000", want: 1024},
		{name: "lower override wins", provider: "智谱 AI", model: "glm-4v-flash", override: "800", want: 800},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := liveVisionMaxTokens(tc.provider, tc.model, tc.override); got != tc.want {
				t.Fatalf("liveVisionMaxTokens(%q, %q, %q) = %d, want %d", tc.provider, tc.model, tc.override, got, tc.want)
			}
		})
	}
}

func TestValidateLiveVisionCompletion_RejectsTruncatedOutput(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"length", "LENGTH", " max_tokens ", "MAX_TOKENS"} {
		reason := reason
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			err := validateLiveVisionCompletion(&llm.CompletionResponse{
				Content:      "最后一道尚未输出完整",
				FinishReason: reason,
			})
			if err == nil {
				t.Fatal("截断响应必须拒绝发送")
			}
			if got := strings.ToLower(err.Error()); !strings.Contains(got, "finish_reason") || !strings.Contains(got, strings.TrimSpace(strings.ToLower(reason))) {
				t.Fatalf("错误应明确包含 finish_reason 和实际原因，got %q", err)
			}
		})
	}
}

func TestValidateLiveVisionCompletion_AllowsCompatibleCompleteReasons(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"", "stop", "STOP"} {
		if err := validateLiveVisionCompletion(&llm.CompletionResponse{
			Content:      "完整解答",
			FinishReason: reason,
		}); err != nil {
			t.Errorf("finish_reason=%q 应兼容继续，got %v", reason, err)
		}
	}
}
