package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAuditCJKTruncation_MCPDesc 钉死不变量：MCP 描述表格截断必须 rune-safe，
// 纯中文描述跨越字节边界时不得腰斩产生乱码 (AP-141)。
func TestAuditCJKTruncation_MCPDesc(t *testing.T) {
	// 60 个中文字 = 180 字节；[:47] 落在第 16 个字（字节 45/46/47）中间 → 腰斩。
	got := truncateDesc(strings.Repeat("界", 60))

	if !utf8.ValidString(got) {
		t.Fatalf("truncateDesc 产生非法 UTF-8（CJK 腰斩）: %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("truncateDesc 结果含替换字符 \\uFFFD: %q", got)
	}
}
