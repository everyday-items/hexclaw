package recall

import "testing"

func TestLexicalReranker_PhraseMatchWins(t *testing.T) {
	rr := LexicalReranker{}
	// 两条基础 score 相等，但 b 整体命中 query 短语 → 精排应把 b 提到第一。
	results := []Result{
		{Entry: Entry{ID: "a", Content: "关于性能优化的一些零散笔记"}, Score: 1.0},
		{Entry: Entry{ID: "b", Content: "深色主题的设置方法"}, Score: 1.0},
	}
	out := rr.Rerank("深色主题", results)
	if out[0].ID != "b" {
		t.Fatalf("短语命中应精排第一，得 %v", out[0].ID)
	}
}

func TestLexicalReranker_CoverageBreaksTie(t *testing.T) {
	rr := LexicalReranker{}
	// 均不含完整短语，但 b 字面覆盖更高 → 排前。
	results := []Result{
		{Entry: Entry{ID: "a", Content: "天气与股市闲聊"}, Score: 1.0},
		{Entry: Entry{ID: "b", Content: "数据库连接池配置"}, Score: 1.0},
	}
	out := rr.Rerank("数据库配置", results)
	if out[0].ID != "b" {
		t.Fatalf("字面覆盖高者应排前，得 %v", out[0].ID)
	}
}
