package llmrouter

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// BUG-20260625 F-4（hex-test 全量横扫 · 复用分层）：上层手搓的 byte-slice truncate
// （return s[:n] + "..."）对 CJK 等多字节字符在字节边界切断，产生非法 UTF-8 乱码。
// 应委托 toolkit/lang/stringx.Truncate（rune-safe）。本测试钉死「截断结果必须是合法 UTF-8」。
//
// 同类位置（穷举见 hex-test anti-patterns AP-049）：hexclaw engine/skill_judge.go、eval/eval.go、
// llmrouter/capabilities.go；hexagon rag/{citation,agentic,selfrag,corrective,extractor}；ai-core media/image。
func TestTruncate_CJK_NoMidByteCut(t *testing.T) {
	s := strings.Repeat("中", 100) // 100 runes / 300 bytes
	got := truncate(s, 50)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate 在字节边界切断 CJK 产生非法 UTF-8（乱码）: %q", got)
	}
	if utf8.RuneCountInString(got) > 60 {
		t.Fatalf("truncate 未按 rune 截断（长度失控）: %d runes", utf8.RuneCountInString(got))
	}
}
