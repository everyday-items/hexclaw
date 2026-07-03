package builtin

// BUG-20260703 B4：摘要 skill 关键词抢答。用户在多轮会话里发"总结下乐知这家公司"
// （承接上文已搜到的公司信息），被 summary 关键词快路径劫持，skill 把触发词后的
// 尾巴"下乐知这家公司"当待摘要正文，吐出垃圾"摘要：下乐知这家公司"，完全没接
// 会话上下文。
//
// 根因：Match 裸前缀匹配触发词，不区分「总结 <一段可压缩的正文>」（真·待摘要）
// 与「总结<话题/代词指代>」（对话式指令）。而 summarizeText 对 ≤80 rune 的输入只会
// 原样回吐（"摘要：<输入>"）——对短尾巴，快路径注定产出回声垃圾。
//
// 契约：剥掉触发词后的正文不超过回声阈值（summaryEchoRuneLimit）的请求一律让路
// LLM 主路径（带会话上下文作答）；只有携带真正可摘要正文的消息保留快路径。
import (
	"strings"
	"testing"
)

func TestBug20260703_B4_SummaryMatchYieldsOnConversationalRequest(t *testing.T) {
	s := NewSummarySkill()

	longArticle := strings.Repeat("今天发布了新版本，修复了若干问题，性能提升明显，用户反馈整体正面。", 4) // >80 runes 的真正文

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// 实机 bug 原型：对话式后续指令，宾语指代上文 → 必须让路 LLM。
		{"实机原型", "总结下乐知这家公司", false},
		{"纯代词", "总结这家公司", false},
		{"指代上文", "总结一下上面的内容", false},
		{"话题请求无正文", "总结一下乐知的财报表现", false},
		{"英文短请求", "summarize this company", false},
		// 短尾巴回声类：快路径只会吐"摘要：<原文>"，零价值 → 让路。
		{"短尾巴回声", "摘要 今天发布了新版本", false},
		// 携带真正可摘要正文（超过回声阈值）→ 保留快路径。
		{"真正文保留快路径", "总结一下：" + longArticle, true},
	}
	for _, tt := range tests {
		if got := s.Match(tt.input); got != tt.want {
			t.Errorf("[BUG-20260703 B4] %s: Match(%q) = %v, want %v", tt.name, tt.input, got, tt.want)
		}
	}
}
