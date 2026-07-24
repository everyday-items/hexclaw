package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

func assertAgentPromptDecoratedOnce(
	t *testing.T,
	systemPrompt string,
	locale string,
	wantBase string,
) {
	t.Helper()

	if !strings.Contains(systemPrompt, wantBase) {
		t.Fatalf("agent base prompt missing %q:\n%s", wantBase, systemPrompt)
	}

	localeDirective := localeOutputDirective(locale)
	if count := strings.Count(systemPrompt, localeDirective); count != 1 {
		t.Fatalf("locale directive must appear exactly once, got %d:\n%s", count, systemPrompt)
	}

	const modelIdentity = "当前搭载 hexclaw-gpt 的 gpt-5.6-sol 作为语言引擎"
	if count := strings.Count(systemPrompt, modelIdentity); count != 1 {
		t.Fatalf("model identity must appear exactly once, got %d:\n%s", count, systemPrompt)
	}

	guard := strings.TrimSpace(agentAntiRecitationGuard)
	if count := strings.Count(systemPrompt, guard); count != 1 {
		t.Fatalf("anti-recitation guard must appear exactly once, got %d:\n%s", count, systemPrompt)
	}
	if localeIndex, guardIndex := strings.Index(systemPrompt, localeDirective), strings.Index(systemPrompt, guard); localeIndex >= guardIndex {
		t.Fatalf("locale/model decoration must precede anti-recitation guard:\n%s", systemPrompt)
	}
}

func TestBuildStreamMessages_DecoratesEveryAgentPromptOverrideOnce(t *testing.T) {
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{"test": mockllm.NewLLMProvider("test")},
		map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}},
		"test",
	)

	dispatcher := agentrouter.New()
	const registeredAgent = "registered-researcher"
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name:         registeredAgent,
		SystemPrompt: "Registered agent base prompt.",
	}); err != nil {
		t.Fatalf("register agent: %v", err)
	}
	eng.SetAgentRouter(dispatcher)

	t.Run("factory role keeps zh-CN reasoning directive", func(t *testing.T) {
		meta := map[string]string{
			"user_locale": "zh-CN",
			"provider":    "hexclaw-gpt",
			"model":       "gpt-5.6-sol",
		}
		messages := eng.buildStreamMessages(context.Background(), "researcher", nil, "", "计算", meta, nil)
		systemPrompt := messages[0].Content

		assertAgentPromptDecoratedOnce(t, systemPrompt, "zh-CN", "高级研究分析师")
		if !strings.Contains(systemPrompt, "不要输出英文思考过程") {
			t.Fatalf("zh-CN factory role must prohibit English reasoning:\n%s", systemPrompt)
		}
	})

	t.Run("registered agent keeps English locale directive", func(t *testing.T) {
		meta := map[string]string{
			"user_locale": "en",
			"provider":    "hexclaw-gpt",
			"model":       "gpt-5.6-sol",
		}
		messages := eng.buildStreamMessages(context.Background(), registeredAgent, nil, "", "calculate", meta, nil)

		assertAgentPromptDecoratedOnce(t, messages[0].Content, "en", "Registered agent base prompt.")
	})

	t.Run("metadata agent_prompt keeps decoration without duplication", func(t *testing.T) {
		meta := map[string]string{
			"agent_prompt": "Metadata agent base prompt.",
			"user_locale":  "zh-CN",
			"provider":     "hexclaw-gpt",
			"model":        "gpt-5.6-sol",
		}
		messages := eng.buildStreamMessages(context.Background(), "", nil, "", "计算", meta, nil)

		assertAgentPromptDecoratedOnce(t, messages[0].Content, "zh-CN", "Metadata agent base prompt.")
	})
}
