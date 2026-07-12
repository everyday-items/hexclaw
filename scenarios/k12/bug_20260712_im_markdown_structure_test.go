package k12

import (
	"io/fs"
	"strings"
	"testing"
)

// TestHomeworkChecker_MarkdownStructureContract（prompt 收口 · RED→GREEN）：
//
// 背景：钉钉/微信出站已支持 sampleMarkdown（标题/加粗/有序无序列表/题号原生渲染）+ NormalizeMathText
// （LaTeX→Unicode 兜底），但模型的长解答仍是一大段纯文本、可读性差。治本=在 homework-checker skill
// 明确「最终解答」的 markdown 结构输出口径：分节标题 / 每小题成行 / 解题步骤有序列表 / 最终答案加粗 /
// 数学用 Unicode 不产 LaTeX / 短答不强加结构；内联回显抬头（📷 + ===最终解答===）保留。
func TestHomeworkChecker_MarkdownStructureContract(t *testing.T) {
	raw, err := fs.ReadFile(BundledSkillsFS(), "skills/homework-checker.md")
	if err != nil {
		t.Fatalf("读取 homework-checker.md 失败: %v", err)
	}
	body := string(raw)

	mustContain := map[string]string{
		"markdown":   "缺 markdown 结构输出总纲",
		"分节标题":       "缺多大题分节标题指令（## 一、…）",
		"有序列表":       "缺解题步骤用有序列表指令",
		"**答案：":      "缺最终答案加粗（**答案：X**）指令",
		"Unicode":    "缺数学符号用 Unicode 指令",
		"LaTeX":      "缺「不产 LaTeX」约束",
		"短答":         "缺短答不强加结构的克制指令",
		"===最终解答===": "markdown 结构应锚定在「===最终解答===」之后的解答正文",
	}
	for sub, why := range mustContain {
		if !strings.Contains(body, sub) {
			t.Errorf("markdown 结构输出契约缺失「%s」：%s", sub, why)
		}
	}
}
