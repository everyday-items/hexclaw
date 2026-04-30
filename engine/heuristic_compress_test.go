package engine

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

func TestHeuristicCompress_KeepsSystem(t *testing.T) {
	hist := []llm.Message{{Role: "system", Content: "you are an assistant"}, {Role: "user", Content: "x"}}
	got := HeuristicCompress(hist, HeuristicCompressOptions{})
	if len(got) != 2 || got[0].Role != "system" {
		t.Errorf("system 永不压缩；got %+v", got)
	}
}

func TestHeuristicCompress_DedupesUser(t *testing.T) {
	hist := []llm.Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi"},
		{Role: "user", Content: "hello"}, // case-insensitive 重复
	}
	got := HeuristicCompress(hist, HeuristicCompressOptions{})
	count := 0
	for _, m := range got {
		if strings.ToLower(string(m.Role)) == "user" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("user 应去重；got %d", count)
	}
}

func TestHeuristicCompress_TruncatesAssistant(t *testing.T) {
	long := strings.Repeat("a", 10000)
	hist := []llm.Message{{Role: "assistant", Content: long}}
	got := HeuristicCompress(hist, HeuristicCompressOptions{MaxAssistantChars: 1000})
	if len(got[0].Content) >= 10000 {
		t.Errorf("应截断；got %d", len(got[0].Content))
	}
	if !strings.Contains(got[0].Content, "truncated") {
		t.Error("应有 truncated 标记")
	}
}

func TestEstimateMessagesTokens_Approximate(t *testing.T) {
	hist := []llm.Message{{Role: "user", Content: strings.Repeat("a", 400)}}
	tok := EstimateMessagesTokens(hist)
	if tok < 90 || tok > 110 {
		t.Errorf("400 chars ≈ 100 token；got %d", tok)
	}
}

func TestHeuristicCompress_DropsOldToolPairs(t *testing.T) {
	hist := []llm.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []llm.ToolCallRef{{ID: "1"}}},
		{Role: "tool", Content: "r1"},
		{Role: "assistant", ToolCalls: []llm.ToolCallRef{{ID: "2"}}},
		{Role: "tool", Content: "r2"},
		{Role: "assistant", ToolCalls: []llm.ToolCallRef{{ID: "3"}}},
		{Role: "tool", Content: "r3"},
		{Role: "assistant", ToolCalls: []llm.ToolCallRef{{ID: "4"}}},
		{Role: "tool", Content: "r4"},
	}
	got := HeuristicCompress(hist, HeuristicCompressOptions{KeepRecentToolPairs: 2})
	// 应该保留 user + 最近 2 对 tool_call/result（4 条）= 至少 5 条
	toolCount := 0
	for _, m := range got {
		role := strings.ToLower(string(m.Role))
		if role == "tool" || (role == "assistant" && len(m.ToolCalls) > 0) {
			toolCount++
		}
	}
	if toolCount > 4 {
		t.Errorf("应只保留最近 2 对（4 条 tool 相关）；got %d", toolCount)
	}
}
