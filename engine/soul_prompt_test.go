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

	// en locale 现在走原生 EN 人设（不再是 zh 人设 + 翻译指令）+ 运行手册 + locale 指令
	if suffix := localeOutputDirective("en"); suffix != "" {
		want := soulWithManual(defaultSoulEN) + "\n\n" + suffix
		if got := systemPrompt(map[string]string{"user_locale": "en"}); got != want {
			t.Fatalf("en locale 应下发原生 EN 人设 + 运行手册 + locale 指令")
		}
	}
}
