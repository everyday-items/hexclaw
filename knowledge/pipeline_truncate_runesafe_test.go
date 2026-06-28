package knowledge

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSimpleContextBuilder_TruncateRuneSafe 锁定 RAG context 截断的 rune 安全（hex-test F3 / AP-141）。
// 修复前 body[:MaxChars] 按字节切，CJK 落在边界即劈裂成非法 UTF-8（这正是 RED）；
// 修复后经 stringx.TruncateBytes 回退到 rune 边界，永远 valid UTF-8。
func TestSimpleContextBuilder_TruncateRuneSafe(t *testing.T) {
	// "你好世界你好" 每字 3 字节；MaxChars=7 的字节切点落在第 3 字（"世"）中间。
	b := SimpleContextBuilder{MaxChars: 7}
	out, err := b.Build([]SearchHit{{DocID: "d1", ChunkIndex: 0, Content: "你好世界你好"}})
	if err != nil {
		t.Fatalf("Build err: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("context 含非法 UTF-8（多字节被腰斩）：%q", out)
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		t.Fatalf("context 含替换符 U+FFFD：%q", out)
	}
	// 回退到边界：内容部分 = "你好"(6 字节) + "..."。
	if !strings.Contains(out, "你好...") {
		t.Fatalf("期望截断回退到 \"你好...\"，得到 %q", out)
	}
	// 不得出现被劈裂的 "世" 半字节（valid UTF-8 已保证，这里再正向断言完整字未混入）
	if strings.Contains(out, "世") {
		t.Fatalf("第 3 字应被整体截掉而非劈裂残留，得到 %q", out)
	}
}
