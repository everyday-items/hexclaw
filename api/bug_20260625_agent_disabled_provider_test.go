package api

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/router"
)

// BUG-20260625 §3-2：注册/更新 Agent 时选了「已禁用」的 LLM provider 不被拒。
// validateAgentLLMConfig → findLLMProviderKey 只查 provider 是否存在，不查 Enabled，
// 导致 Agent 绑定到 Enabled=false 的 provider，运行期才以底层错误失败，无清晰回退/拒绝。
// 正确不变量：provider 存在但被禁用 → 注册校验应直接报错。
func TestValidateAgentLLMConfig_RejectsDisabledProvider(t *testing.T) {
	disabled := false
	s := &Server{
		cfg: &config.Config{
			LLM: config.LLMConfig{
				Providers: map[string]config.LLMProviderConfig{
					"openai": {Model: "gpt-4o", Enabled: &disabled},
				},
			},
		},
	}
	cfg := &router.AgentConfig{Provider: "openai", Model: "gpt-4o"}
	if err := s.validateAgentLLMConfig(cfg); err == nil {
		t.Fatal("绑定到已禁用 provider 的 Agent 应被校验拒绝，但通过了")
	}
}

// 反向保证：启用的 provider（Enabled=nil 视为启用 / Enabled=true）正常通过。
func TestValidateAgentLLMConfig_AllowsEnabledProvider(t *testing.T) {
	enabled := true
	for name, en := range map[string]*bool{"nil-默认启用": nil, "显式启用": &enabled} {
		s := &Server{
			cfg: &config.Config{
				LLM: config.LLMConfig{
					Providers: map[string]config.LLMProviderConfig{
						"openai": {Model: "gpt-4o", Enabled: en},
					},
				},
			},
		}
		cfg := &router.AgentConfig{Provider: "openai", Model: "gpt-4o"}
		if err := s.validateAgentLLMConfig(cfg); err != nil {
			t.Fatalf("%s 的 provider 应通过校验，却报错: %v", name, err)
		}
	}
}
