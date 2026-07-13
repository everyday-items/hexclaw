package engineadapter

import (
	"strings"
	"testing"
)

// BUG-20260714（真机截图）：前端虽然已用 MarkdownRenderer，但轻量出题模型返回的仍是
// “问题：/解答：/答案：”普通文本；没有 Markdown 标题和列表，弹层看起来仍像大段正文。
func TestNormalizeRetryMarkdown_PlainModelAnswerBecomesGitHubMarkdown(t *testing.T) {
	raw := `问题：
有12个杯子全部正放，每次选择任意6个杯子进行翻转，最少要用多少次才能让所有杯子全部倒扣？

解答：
每次翻转6个杯子，相当于改变了这些杯子的状态。
第一次翻转：选择6个杯子进行翻转。
第二次翻转：再选择另外6个杯子进行翻转。
第三次翻转：再次选择另外6个杯子进行翻转。
因此，最少需要3次翻转。

答案：
3次`

	got := normalizeRetryMarkdown(raw)
	for _, want := range []string{
		"## 问题",
		"## 解答",
		"1. 第一次翻转：选择6个杯子进行翻转。",
		"2. 第二次翻转：再选择另外6个杯子进行翻转。",
		"3. 第三次翻转：再次选择另外6个杯子进行翻转。",
		"## 答案",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 GitHub Markdown 结构 %q\n全文：\n%s", want, got)
		}
	}
	for _, plain := range []string{"\n问题：\n", "\n解答：\n", "\n答案：\n"} {
		if strings.Contains("\n"+got+"\n", plain) {
			t.Errorf("普通标签行 %q 未转换\n全文：\n%s", plain, got)
		}
	}
}

func TestNormalizeRetryMarkdown_AlreadyStructuredIsIdempotent(t *testing.T) {
	in := "## 问题\n\n计算 4.2×3。\n\n## 解答\n\n1. 先算 4×3。\n2. 再算 0.2×3。\n\n## 答案\n\n**12.6**"
	if got := normalizeRetryMarkdown(in); got != in {
		t.Fatalf("已结构化 Markdown 不应被重复改写\ngot:\n%s\nwant:\n%s", got, in)
	}
}
