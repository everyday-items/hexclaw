package engine

// BUG-20260709：回答语言必须跟随系统设置语言——包括默认中文。
//
// 旧契约：zh-CN/zh 返回空指令，「默认中文」靠 SOUL 隐式约定。真机取证（钉钉图片解题轮）：
// 英语倾向的推理模型（nemotron omni 等）在 SOUL 未被强调/自定义 SOUL 场景下会直接英文作答。
// 新契约：zh-CN/zh 也返回**显式**白名单中文指令（与 en/ug 对称），空串仅保留给「未设置」。
//
// 断言的是正确行为——未修复时 FAIL 即证明缺口存在。

import (
	"strings"
	"testing"
)

func TestBUG20260709_ZhLocale_GetsExplicitDirective(t *testing.T) {
	for _, locale := range []string{"zh-CN", "zh"} {
		got := localeOutputDirective(locale)
		if got == "" {
			t.Errorf("locale=%q: 系统语言为中文时应有显式输出语言指令（空指令→英语倾向模型漏英文，真机已复现）", locale)
			continue
		}
		if !strings.Contains(got, "中文") {
			t.Errorf("locale=%q: 指令应明确要求中文回答，got %q", locale, got)
		}
	}
	// 未设置（空）仍不追加——保持向后兼容与最小注入面
	if got := localeOutputDirective(""); got != "" {
		t.Errorf("locale 为空应不追加指令，got %q", got)
	}
}
