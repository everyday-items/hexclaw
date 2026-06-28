package security

import (
	"strings"
	"testing"
)

// TestSanitizeToolOutput_RuneSafeTruncate 钉死：工具输出 sanitize 的 maxLen 截断
// 按 rune 切，绝不在多字节中文/emoji 中间切裂出 U+FFFD（喂给 LLM 的内容须合法 UTF-8）。
func TestSanitizeToolOutput_RuneSafeTruncate(t *testing.T) {
	// 纯多字节内容（无 HTML/不可见字符），隔离出截断行为。
	s := strings.Repeat("天气🌡杭州", 200)
	out := SanitizeToolOutput(s, 100)
	if strings.ContainsRune(out, '�') {
		t.Fatalf("含 U+FFFD，截断在多字节字符中间切裂了")
	}
	if !strings.Contains(out, "[truncated]") {
		t.Fatalf("超长内容未被截断")
	}
}

func TestSanitizeToolOutput_ShortNoTruncate(t *testing.T) {
	s := "杭州 27°C 🦀"
	out := SanitizeToolOutput(s, 100)
	if strings.Contains(out, "[truncated]") {
		t.Fatalf("短内容被误截断: %q", out)
	}
}
