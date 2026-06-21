package engine

import "testing"

// TestSystemPromptDefaultBehavior 钉死「默认助理人设」零回归：
// SOUL.md 为空时，默认分支产物 == 重构前的内置默认行为（含 locale 指令），
// 且自定义 SOUL 在无装饰元数据时原样透传。
func TestSystemPromptDefaultBehavior(t *testing.T) {
	if DefaultSystemPrompt() != defaultSystemPrompt {
		t.Fatal("DefaultSystemPrompt() 应返回内置默认人设原文")
	}

	// 无 model / 无 locale → 内置默认原样返回（默认分支零回归）
	if got := systemPrompt(nil); got != defaultSystemPrompt {
		t.Fatalf("无 metadata 时默认人设应原样返回，got len=%d", len(got))
	}

	// 自定义 SOUL 无装饰元数据 → 原样透传
	custom := "你是定制小蟹。规则：简洁。"
	if got := decorateSystemPrompt(custom, nil); got != custom {
		t.Fatalf("自定义 SOUL 无装饰时应原样返回，got=%q", got)
	}

	// locale 指令仍按重构前逻辑追加到末尾
	if suffix := localeOutputDirective("en"); suffix != "" {
		want := defaultSystemPrompt + "\n\n" + suffix
		if got := systemPrompt(map[string]string{"user_locale": "en"}); got != want {
			t.Fatalf("locale 指令未按预期追加")
		}
	}
}
