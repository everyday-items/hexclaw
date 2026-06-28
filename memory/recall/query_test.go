package recall

import (
	"strings"
	"testing"
)

func TestSynonymExpander(t *testing.T) {
	e := SynonymExpander{Synonyms: DefaultSynonyms()}

	out := e.Expand("主题偏好")
	if !strings.Contains(out, "外观") {
		t.Fatalf("应并入「主题」近义词外观，得 %q", out)
	}
	if !strings.Contains(out, "喜欢") {
		t.Fatalf("应并入「偏好」近义词喜欢，得 %q", out)
	}
	if !strings.HasPrefix(out, "主题偏好") {
		t.Fatalf("应保留原 query 在前，得 %q", out)
	}
	// 确定性：多次调用结果一致（不受 map 迭代序影响）。
	if e.Expand("主题偏好") != out {
		t.Fatal("Expand 应确定性")
	}
	// 无命中 key → 原样。
	if got := e.Expand("随便聊聊天气"); got != "随便聊聊天气" {
		t.Fatalf("无命中应原样返回，得 %q", got)
	}
	// 空 → 空。
	if got := e.Expand("  "); got != "" {
		t.Fatalf("空 query 应返回空，得 %q", got)
	}
}
