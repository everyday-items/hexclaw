package engine

import (
	"strings"
	"testing"
)

// TestStripAllThinking_Fix_v0_3_12_H4 回归测试：
// 修复前 extractThinkTags 只识别 content 开头的思考标签；若模型把 <think> 放中间
// （某些本地 Qwen/DeepSeek 模型会这样），会漏过滤，家长直接看到原始思考过程。
// 修复后 StripAllThinking 移除任意位置 + 任意数量的 <think>/<thinking>/<reasoning> 块。
func TestStripAllThinking_Fix_v0_3_12_H4(t *testing.T) {
	t.Run("before_fix_behavior_extract_only_handles_leading_tag", func(t *testing.T) {
		// 修复前：content 中间嵌入的 <think> 不会被 extractThinkTags 处理
		inline := "回答是 42 <think>计算过程</think> 希望对你有帮助"
		clean, _ := extractThinkTags(inline)
		// extractThinkTags 对非开头位置没反应
		if !strings.Contains(clean, "<think>") {
			t.Error("test premise failed: extractThinkTags 不应处理中间位置")
		}
		t.Logf("修复前：content 中间嵌入的 <think> 残留在输出：%q", clean)
	})

	t.Run("after_fix_strips_leading_block", func(t *testing.T) {
		in := "<think>思考中...</think>\n最终答案：42"
		got := StripAllThinking(in)
		want := "最终答案：42"
		if got != want {
			t.Errorf("got=%q want=%q", got, want)
		}
	})

	t.Run("after_fix_strips_middle_block", func(t *testing.T) {
		in := "回答是 42 <think>计算过程</think> 希望有帮助"
		got := StripAllThinking(in)
		if strings.Contains(got, "<think>") || strings.Contains(got, "计算过程") {
			t.Errorf("中间 <think> 块未被剥离：%q", got)
		}
	})

	t.Run("after_fix_strips_multiple_blocks", func(t *testing.T) {
		in := "<think>第一步</think>中间<reasoning>第二步</reasoning>结尾<thinking>第三步</thinking>"
		got := StripAllThinking(in)
		if strings.Contains(got, "<think>") || strings.Contains(got, "<reasoning>") || strings.Contains(got, "<thinking>") {
			t.Errorf("多标签未全部剥离：%q", got)
		}
		if !strings.Contains(got, "中间") || !strings.Contains(got, "结尾") {
			t.Errorf("正文被误删：%q", got)
		}
	})

	t.Run("after_fix_strips_reasoning_tag_alternative", func(t *testing.T) {
		in := "答案 <reasoning>\n推理\n链条\n</reasoning> 结束"
		got := StripAllThinking(in)
		if strings.Contains(got, "<reasoning>") || strings.Contains(got, "推理") {
			t.Errorf("<reasoning> 未剥离：%q", got)
		}
	})

	t.Run("after_fix_strips_multiline_block", func(t *testing.T) {
		in := `回答：
<think>
让我想想，
一步步来
</think>
42`
		got := StripAllThinking(in)
		if strings.Contains(got, "让我想想") {
			t.Errorf("多行 <think> 内容未清除：%q", got)
		}
		if !strings.Contains(got, "42") {
			t.Errorf("正文丢失：%q", got)
		}
	})

	t.Run("after_fix_handles_streaming_unclosed_tail", func(t *testing.T) {
		// 流式分包情况下可能收到 "<think>..." 而没有对应 </think>
		in := "回答：42 <think>还没想完..."
		got := StripAllThinking(in)
		if strings.Contains(got, "<think>") {
			t.Errorf("未闭合 <think> 残段未截断：%q", got)
		}
		if !strings.Contains(got, "42") {
			t.Errorf("正文丢失：%q", got)
		}
	})

	t.Run("after_fix_no_thinking_passes_through", func(t *testing.T) {
		in := "普通回答，没有思考标签"
		got := StripAllThinking(in)
		if got != in {
			t.Errorf("正常内容不应被修改：got=%q want=%q", got, in)
		}
	})

	t.Run("after_fix_empty_input_safe", func(t *testing.T) {
		if StripAllThinking("") != "" {
			t.Error("空串应返回空串")
		}
	})

	t.Run("g3_preserves_code_block_consecutive_newlines", func(t *testing.T) {
		// G3 修复：原本 squeezeBlankLines 会把 \n\n\n+ 压缩为 \n\n，但这会误伤代码块
		// 内的连续空行（Python / YAML / 多行字符串）。修复后不压缩正文连续空行。
		in := "```python\ndef f():\n    x = 1\n\n\n    y = 2\n```"
		got := StripAllThinking(in)
		if !strings.Contains(got, "x = 1\n\n\n    y = 2") {
			t.Errorf("代码块内连续空行被误压缩：%q", got)
		}
	})

	t.Run("g3_trims_leading_trailing_only", func(t *testing.T) {
		// 仅首尾做 trim；去掉思考标签后如果留下的空行在开头/结尾会被 trim
		in := "\n\n<think>foo</think>\n\n答案\n\n"
		got := StripAllThinking(in)
		if got != "答案" {
			t.Errorf("首尾 trim 失败：got=%q want=%q", got, "答案")
		}
	})

	t.Run("after_fix_does_not_match_angle_brackets_in_text", func(t *testing.T) {
		// 正文里出现 < 或 > 不应误匹配（只匹配完整标签名）
		in := "当 x > 10 且 y < 20 时，答案是 30"
		got := StripAllThinking(in)
		if got != in {
			t.Errorf("正常 <> 字符被误伤：%q", got)
		}
	})
}
