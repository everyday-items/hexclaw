package builtin

// Known-backlog #1 (review): SummarySkill.Match used a bare prefix check on
// summary keywords, so a message like "总结这篇文章并加入知识库" was hijacked
// by the local extractive-summary fast path and the knowledge-base ingestion
// never happened. Match must yield to the LLM tool-calling path whenever the
// message also carries a KB-ingest intent marker.

import "testing"

func TestBug20260611_SummaryMatchYieldsToKBIngest(t *testing.T) {
	s := NewSummarySkill()

	// summarizableBody 超过回声阈值（summaryEchoRuneLimit）的真正文——
	// BUG-20260703 B4 后，只有携带此类正文的消息保留快路径。
	summarizableBody := "今天发布了新版本，修复了若干问题，性能提升明显，安装包体积下降，启动速度加快，内存占用降低，用户反馈整体正面，崩溃率明显下降，后续将继续优化细节体验，欢迎大家升级试用并积极反馈问题。"

	tests := []struct {
		input string
		want  bool
	}{
		// Summary requests carrying a real summarizable body keep the fast path.
		{"总结一下：" + summarizableBody, true},
		{"摘要 " + summarizableBody, true},
		// BUG-20260703 B4：短尾巴/代词指代上文的对话式请求让路 LLM
		// （本地抽取式算法对 ≤80 rune 输入只会回声，产出"摘要：<原文>"垃圾）。
		{"总结这篇文章", false},
		{"摘要 今天发布了新版本", false},
		{"summary this article", false},
		{"总结一下：今天发布了新版本，修复了若干问题。", false},
		// Messages that also ask for KB ingestion must NOT be hijacked.
		{"总结这篇文章并加入知识库", false},
		{"摘要后保存到知识库", false},
		{"总结今天的新闻并入库", false},
		{"概括这段内容然后收藏", false},
		{"summarize this and save to knowledge base", false},
		// KB-ingest intent yields even with a long body (ingest half must not be dropped).
		{"总结并加入知识库：" + summarizableBody, false},
		// Non-summary messages still don't match.
		{"今天天气怎么样", false},
	}
	for _, tt := range tests {
		if got := s.Match(tt.input); got != tt.want {
			t.Errorf("[BUG-20260611] Match(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
