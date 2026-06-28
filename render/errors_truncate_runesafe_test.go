package render

import (
	"strings"
	"testing"
)

// TestTruncateStderr_RuneSafe 钉死：stderr 截断按 rune 切，绝不在多字节中文/emoji
// 中间切裂产生 U+FFFD（�）。
func TestTruncateStderr_RuneSafe(t *testing.T) {
	// 远超 1KB 的全多字节内容，逼迫在多字节边界截断。
	s := strings.Repeat("错误：渲染失败🌍", 500)
	out := truncateStderr(s)
	if strings.ContainsRune(out, '�') {
		t.Fatalf("含替换字符 U+FFFD，说明在多字节字符中间切裂了")
	}
	if !strings.HasSuffix(out, "...[truncated]") {
		t.Fatalf("缺截断后缀: %q", out[len(out)-20:])
	}
}

func TestTruncateStderr_ShortPassThrough(t *testing.T) {
	s := "错误：找不到引擎 🦀"
	if got := truncateStderr(s); got != s {
		t.Fatalf("短内容被改动: %q", got)
	}
}
