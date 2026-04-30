package adapter

import "testing"

func TestStripThinking(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "hello world", "hello world"},
		{"think paired", "答 <think>推理</think> 案", "答  案"},
		{"thinking paired", "<thinking>x</thinking>结果", "结果"},
		{"reasoning paired", "<reasoning>r</reasoning>final", "final"},
		{"multi-line think", "前\n<think>多\n行\n推理</think>\n后", "前\n\n后"},
		{"multiple blocks", "<think>a</think>中<thinking>b</thinking>", "中"},
		{"unclosed tail (stream-safe)", "答案 <think>还没说完", "答案"},
		{"only think block", "<think>纯思考</think>", ""},
		{"trim outer whitespace only", "   答案   ", "答案"},
		{"preserve inner blank lines (code/yaml)", "line1\n\n\nline2", "line1\n\n\nline2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripThinking(c.in); got != c.want {
				t.Errorf("StripThinking(%q)=%q want=%q", c.in, got, c.want)
			}
		})
	}
}
