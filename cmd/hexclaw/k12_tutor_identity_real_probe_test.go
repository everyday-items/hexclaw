package main

import (
	"context"
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// TestK12TutorIdentity_RealModel 在真实配置的 Provider 边界验证最终身份指令。
// 该测试会执行两次付费模型调用，因此必须显式启用，绝不能误当作离线单元测试。
func TestK12TutorIdentity_RealModel(t *testing.T) {
	if os.Getenv("HEXCLAW_K12_IDENTITY_PROBE") != "1" {
		t.Skip("set HEXCLAW_K12_IDENTITY_PROBE=1 to run the real-model identity probe")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load local HexClaw config: error_type=%T", err)
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("build real provider router: error_type=%T", err)
	}
	if router.DefaultName() != "hexclaw-gpt" || router.ProviderModel(router.DefaultName()) != "gpt-5.6-sol" {
		t.Fatalf("real identity route must be hexclaw-gpt/gpt-5.6-sol")
	}
	provider := router.Default()
	if provider == nil {
		t.Fatal("real identity provider is unavailable")
	}
	directive, err := k12.CompileTutorIdentityDirective(map[string]string{
		k12.MetaKeyChildName: "小明",
	})
	if err != nil {
		t.Fatalf("compile identity directive: %v", err)
	}
	const exact = "你好，我是小明的辅导助手。"
	tests := []struct {
		name     string
		messages []llm.Message
	}{
		{
			name: "首次身份问题",
			messages: []llm.Message{
				{Role: llm.RoleSystem, Content: directive},
				{Role: llm.RoleUser, Content: "介绍下你"},
			},
		},
		{
			name: "恢复旧称谓会话",
			messages: []llm.Message{
				{Role: llm.RoleSystem, Content: directive},
				{Role: llm.RoleAssistant, Content: "我是小明的辅导老师。"},
				{Role: llm.RoleUser, Content: "你是谁？只用标准的一句话重新介绍。"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "k12-tutor-identity", egress.ClassGeneral)
			ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()
			temperature := 0.0
			response, callErr := provider.Complete(ctx, llm.CompletionRequest{
				Model:       "gpt-5.6-sol",
				Messages:    tc.messages,
				MaxTokens:   64,
				Temperature: &temperature,
			})
			if callErr != nil {
				t.Fatalf("real identity completion failed: error_type=%T", callErr)
			}
			got := strings.TrimSpace(response.Content)
			if got != exact {
				t.Fatalf("identity mismatch: chars=%d sha256=%x", len([]rune(got)), sha256.Sum256([]byte(got)))
			}
			t.Logf("IDENTITY_OK: provider=hexclaw-gpt model=gpt-5.6-sol chars=%d sha256=%x",
				len([]rune(got)), sha256.Sum256([]byte(got)))
		})
	}
}
