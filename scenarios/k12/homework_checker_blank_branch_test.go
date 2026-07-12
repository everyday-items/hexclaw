package k12

import (
	"io/fs"
	"strings"
	"testing"
)

// TestHomeworkChecker_HasBlankSolveBranch IM 提示词结构锁：homework-checker 必须含
// 「空白 vs 已答」显式分支——空白/未作答题给完整解法（不批改、不编造 student_answer）。
// RED（治本前）：纯批改口径，空白卷时 LLM 编答案或报错。
func TestHomeworkChecker_HasBlankSolveBranch(t *testing.T) {
	b, err := fs.ReadFile(BundledSkillsFS(), "skills/homework-checker.md")
	if err != nil {
		t.Fatalf("读取 homework-checker.md 失败: %v", err)
	}
	md := string(b)
	must := []string{"空白", "未作答", "编造", "解法"}
	for _, kw := range must {
		if !strings.Contains(md, kw) {
			t.Errorf("homework-checker 缺空白分支关键词 %q", kw)
		}
	}
	// 显式指令：空白题 student_answer 留空触发求解分叉。
	if !strings.Contains(md, "留空") {
		t.Error("应显式指示空白题把 student_answer 留空（走求解分叉）")
	}
}
