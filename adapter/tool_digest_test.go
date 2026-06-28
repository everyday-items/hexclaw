package adapter

import (
	"strings"
	"testing"
)

func TestToolCallDigest_IMFriendly(t *testing.T) {
	out := ToolCallDigest([]ToolCall{
		{Name: "weather", Result: "🌍 **杭州 天气**\n🌡 温度: 27°C", Status: "success"},
		{Name: "mcp__svc__search", Result: "找到 3 条结果", Status: "success"},
		{Name: "glob", Result: "Error: path rejected", Status: "error"},
	})
	// 本地化名（裸 id 不泄露；MCP 前缀剥离）
	if !strings.Contains(out, "天气查询") || !strings.Contains(out, "网络搜索") {
		t.Fatalf("工具名未本地化: %q", out)
	}
	if strings.Contains(out, "weather") || strings.Contains(out, "mcp__") {
		t.Fatalf("泄露裸 id: %q", out)
	}
	// 状态字形
	if !strings.Contains(out, "✓") || !strings.Contains(out, "✗") {
		t.Fatalf("缺状态字形: %q", out)
	}
	// 摘要：markdown 星号被剥离、单行
	if strings.Contains(out, "**") {
		t.Fatalf("摘要未去 markdown: %q", out)
	}
	if !strings.Contains(out, "温度: 27°C") {
		t.Fatalf("摘要缺关键信息: %q", out)
	}
}

func TestToolCallDigest_Empty(t *testing.T) {
	if ToolCallDigest(nil) != "" {
		t.Fatal("空调用应返回空串")
	}
	if ToolCallDigest([]ToolCall{}) != "" {
		t.Fatal("空切片应返回空串")
	}
}

func TestToolResultSummary_RuneSafe(t *testing.T) {
	// 恰在 emoji 处溢出，不得切裂出 U+FFFD
	s := toolResultSummary(strings.Repeat("天", 46)+"🌍🌡 尾", 48)
	if strings.ContainsRune(s, '�') {
		t.Fatalf("摘要切裂 emoji: %q", s)
	}
	if !strings.HasSuffix(s, "…") {
		t.Fatalf("超长摘要应带省略号: %q", s)
	}
}
