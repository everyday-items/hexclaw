package engine

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAuditCJKTruncation_SummarizeArgs 钉死不变量：审批/审计行参数摘要截断（用户可见）
// 必须 rune-safe，纯中文值跨越字节边界时不得腰斩产生乱码 (AP-141)。
func TestAuditCJKTruncation_SummarizeArgs(t *testing.T) {
	// 60 个中文字 = 180 字节；[:47] 落在第 16 个字（字节 45/46/47）中间 → 腰斩。
	args := map[string]any{"path": strings.Repeat("界", 60)}
	got := summarizeArgs(args)

	if !utf8.ValidString(got) {
		t.Fatalf("summarizeArgs 产生非法 UTF-8（CJK 腰斩）: %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("summarizeArgs 结果含替换字符 \\uFFFD: %q", got)
	}
}

// TestAuditCJKTruncation_FormatVal 钉死不变量：skill hook formatVal 截断必须 rune-safe。
func TestAuditCJKTruncation_FormatVal(t *testing.T) {
	// 60 个中文字 = 180 字节；[:80] 落在第 27 个字（字节 78/79/80）中间 → 腰斩。
	got := formatVal(strings.Repeat("好", 60))

	if !utf8.ValidString(got) {
		t.Fatalf("formatVal 产生非法 UTF-8（CJK 腰斩）: %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("formatVal 结果含替换字符 \\uFFFD: %q", got)
	}
}
