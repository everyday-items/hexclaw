package release

import (
	"strings"
	"testing"
)

// TestTruncate_RuneSafe 钉死：发版检查日志截断按 rune 切，不切裂多字节字符。
func TestTruncate_RuneSafe(t *testing.T) {
	s := strings.Repeat("检查项🌍", 100)
	out := truncate(s, 50)
	if strings.ContainsRune(out, '�') {
		t.Fatalf("含 U+FFFD，多字节字符被切裂")
	}
	if !strings.HasSuffix(out, "[truncated]") {
		t.Fatalf("缺截断后缀")
	}
}
