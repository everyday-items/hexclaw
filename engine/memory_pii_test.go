package engine

import (
	"context"
	"strings"
	"testing"
)

// 写链守卫：敏感事实即便被抽出也不落库；同批正常事实正常写入。
// （LooksSensitive 单测在 memory/pii_test.go；此处验端到端写链拦截。）
func TestIngest_DropsSensitive(t *testing.T) {
	fm := newFileMem(t, 200)
	eng := engineWithFileMem(t, fm)

	facts := parseExtractedFacts(
		"用户住在杭州\n" +
			"用户的密码是 hunter2xyz\n" +
			"[职业] 用户是 Go 工程师\n" +
			"api_key: sk-ABCDEFGHIJKLMNOP9999")
	n := eng.ingestExtractedFacts(context.Background(), facts, "")
	if n != 2 {
		t.Fatalf("应只写入 2 条非敏感事实，得 %d", n)
	}
	for _, e := range eng.fileMem.ParseEntries() {
		if strings.Contains(e.Content, "密码") || strings.Contains(e.Content, "sk-") {
			t.Fatalf("敏感内容泄漏进记忆: %q", e.Content)
		}
	}
}
