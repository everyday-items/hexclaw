package builtin

// Known-backlog #1 (review): SummarySkill.Match used a bare prefix check on
// summary keywords, so a message like "总结这篇文章并加入知识库" was hijacked
// by the local extractive-summary fast path and the knowledge-base ingestion
// never happened. Match must yield to the LLM tool-calling path whenever the
// message also carries a KB-ingest intent marker.

import "testing"

func TestBug20260611_SummaryMatchYieldsToKBIngest(t *testing.T) {
	s := NewSummarySkill()

	tests := []struct {
		input string
		want  bool
	}{
		// Plain summary requests keep the fast path.
		{"总结这篇文章", true},
		{"摘要 今天发布了新版本", true},
		{"summary this article", true},
		{"总结一下：今天发布了新版本，修复了若干问题。", true},
		// Messages that also ask for KB ingestion must NOT be hijacked.
		{"总结这篇文章并加入知识库", false},
		{"摘要后保存到知识库", false},
		{"总结今天的新闻并入库", false},
		{"概括这段内容然后收藏", false},
		{"summarize this and save to knowledge base", false},
		// Non-summary messages still don't match.
		{"今天天气怎么样", false},
	}
	for _, tt := range tests {
		if got := s.Match(tt.input); got != tt.want {
			t.Errorf("[BUG-20260611] Match(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
