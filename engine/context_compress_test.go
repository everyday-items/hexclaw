package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	hexagon "github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

func TestSplitIndexes_SystemAtStart(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "do something"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", Content: "result1"},
		{Role: "assistant", Content: "done1"},
		{Role: "tool", Content: "result2"},
		{Role: "assistant", Content: "done2"},
		{Role: "user", Content: "more"},
		{Role: "assistant", Content: "final"},
	}

	sysEnd, keepStart := splitIndexes(msgs)
	if sysEnd != 1 {
		t.Errorf("systemEnd = %d, want 1", sysEnd)
	}
	if keepStart != len(msgs)-contextKeepRecent {
		t.Errorf("keepStart = %d, want %d", keepStart, len(msgs)-contextKeepRecent)
	}
}

func TestSplitIndexes_NoSystem(t *testing.T) {
	msgs := make([]llm.Message, 12)
	for i := range msgs {
		msgs[i] = llm.Message{Role: "user", Content: "msg"}
	}

	sysEnd, keepStart := splitIndexes(msgs)
	if sysEnd != 0 {
		t.Errorf("systemEnd = %d, want 0 (no system messages)", sysEnd)
	}
	if keepStart != 12-contextKeepRecent {
		t.Errorf("keepStart = %d, want %d", keepStart, 12-contextKeepRecent)
	}
}

func TestSplitIndexes_TooFewMessages(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}

	sysEnd, keepStart := splitIndexes(msgs)
	// keepStart should not go below systemEnd
	if keepStart < sysEnd {
		t.Errorf("keepStart (%d) < systemEnd (%d)", keepStart, sysEnd)
	}
}

func TestHeuristicToolSummary(t *testing.T) {
	msgs := []llm.Message{
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCallRef{
			{Name: "grep", Arguments: `{"pattern":"func main"}`},
		}},
		{Role: "tool", Content: "main.go:5:func main() {"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCallRef{
			{Name: "file_edit", Arguments: `{"file_path":"main.go"}`},
		}},
		{Role: "tool", Content: "Error executing tool \"file_edit\": old_string not found"},
	}

	summary := heuristicToolSummary(msgs)
	if !strings.Contains(summary, "2 tool calls") {
		t.Errorf("should mention 2 tool calls, got: %s", summary)
	}
	if !strings.Contains(summary, "grep") {
		t.Errorf("should mention grep, got: %s", summary)
	}
	if !strings.Contains(summary, "file_edit") {
		t.Errorf("should mention file_edit, got: %s", summary)
	}
	if !strings.Contains(summary, "Error") {
		t.Errorf("should preserve error info, got: %s", summary)
	}
}

func TestHeuristicToolSummary_NoTools(t *testing.T) {
	msgs := []llm.Message{
		{Role: "assistant", Content: "hi there"},
	}

	summary := heuristicToolSummary(msgs)
	if !strings.Contains(summary, "no interactions to summarize") {
		t.Errorf("should indicate no interactions, got: %s", summary)
	}
}

func TestHeuristicToolSummary_PreservesUserConstraints(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "不要修改数据库，只读排查"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCallRef{
			{Name: "grep", Arguments: `{"pattern":"SELECT"}`},
		}},
		{Role: "tool", Content: "found 3 matches"},
	}

	summary := heuristicToolSummary(msgs)
	if !strings.Contains(summary, "不要修改数据库") {
		t.Errorf("should preserve user constraint, got: %s", summary)
	}
	if !strings.Contains(summary, "User context:") {
		t.Errorf("should have User context section, got: %s", summary)
	}
}

func TestCompressContextIfNeeded_BelowThreshold(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "short message"},
		{Role: "assistant", Content: "short reply"},
	}

	result := compressContextIfNeeded(context.Background(), msgs, nil, "test-session")
	if len(result) != len(msgs) {
		t.Errorf("should not compress below threshold, got %d msgs (was %d)", len(result), len(msgs))
	}
}

func TestCompressContextIfNeeded_AboveThreshold(t *testing.T) {
	// Build messages that exceed the character threshold
	msgs := []llm.Message{
		{Role: "system", Content: "system prompt"},
	}

	// Add enough messages to exceed threshold
	bigContent := strings.Repeat("x", 5000) // 5K chars each
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			llm.Message{Role: "assistant", Content: bigContent, ToolCalls: []llm.ToolCallRef{
				{Name: "grep", Arguments: `{"pattern":"test"}`},
			}},
			llm.Message{Role: "tool", Content: bigContent},
		)
	}

	// Total: 1 system + 40 tool messages = 41 messages, ~200K chars
	result := compressContextIfNeeded(context.Background(), msgs, nil, "test-session")

	// Should be compressed: system + summary + last 8
	if len(result) >= len(msgs) {
		t.Errorf("should compress, got %d msgs (was %d)", len(result), len(msgs))
	}

	// First message should still be system
	if result[0].Role != "system" {
		t.Errorf("first message should be system, got %s", result[0].Role)
	}

	// Should have a summary message
	hasSummary := false
	for _, m := range result {
		if strings.Contains(m.Content, "Prior tool interactions summary") {
			hasSummary = true
			break
		}
	}
	if !hasSummary {
		t.Error("compressed messages should contain a summary")
	}

	// Last messages should be preserved
	lastOriginal := msgs[len(msgs)-1]
	lastCompressed := result[len(result)-1]
	if lastOriginal.Content != lastCompressed.Content {
		t.Error("last message should be preserved exactly")
	}
}

// F.1 验证: 多 ToolCalls 时工具名和结果配对正确
func TestHeuristicToolSummary_MultipleToolCallsPerMessage(t *testing.T) {
	msgs := []llm.Message{
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCallRef{
			{Name: "grep", Arguments: `{"pattern":"main"}`},
			{Name: "glob", Arguments: `{"pattern":"*.go"}`},
		}},
		{Role: "tool", Content: "main.go:1:package main"},
		{Role: "tool", Content: "Found 3 files"},
	}

	summary := heuristicToolSummary(msgs)

	// 验证 grep 结果配对在 grep 行
	if !strings.Contains(summary, "- grep: main.go:1:package main") {
		t.Errorf("grep should be paired with its result, got:\n%s", summary)
	}
	// 验证 glob 结果配对在 glob 行
	if !strings.Contains(summary, "- glob: Found 3 files") {
		t.Errorf("glob should be paired with its result, got:\n%s", summary)
	}
}

// F.5 验证: UTF-8 安全截断 (via stringx.TruncateWithSuffix)
func TestTruncateWithSuffix(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "he..."},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := stringx.TruncateWithSuffix(tt.input, tt.n, "...")
		if got != tt.want {
			t.Errorf("TruncateWithSuffix(%q, %d, \"...\") = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}

func TestTruncateWithSuffix_UTF8Safe(t *testing.T) {
	// 中文: 每个汉字 3 bytes, "你好世界" = 4 runes
	input := "你好世界再见"
	got := stringx.TruncateWithSuffix(input, 4, "...")
	// TruncateWithSuffix 会在 maxLen 内放入 suffix，所以 4 个 rune 位: 1 个汉字 + "..."
	want := "你..."
	if got != want {
		t.Errorf("TruncateWithSuffix(%q, 4, \"...\") = %q, want %q", input, got, want)
	}

	// emoji: 每个 emoji 4 bytes
	emojiInput := "🦀🔧📝✅Done"
	got2 := stringx.TruncateWithSuffix(emojiInput, 6, "...")
	want2 := "🦀🔧📝..."
	if got2 != want2 {
		t.Errorf("TruncateWithSuffix(%q, 6, \"...\") = %q, want %q", emojiInput, got2, want2)
	}
}

// 未验证项: LLM 摘要成功路径 (mock provider)
type mockSummaryProvider struct {
	response string
	err      error
}

func (p *mockSummaryProvider) Name() string { return "mock" }
func (p *mockSummaryProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock"}}
}
func (p *mockSummaryProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }
func (p *mockSummaryProvider) Stream(_ context.Context, _ hexagon.CompletionRequest) (*llm.Stream, error) {
	return nil, fmt.Errorf("not implemented")
}
func (p *mockSummaryProvider) Complete(_ context.Context, _ hexagon.CompletionRequest) (*llm.CompletionResponse, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &llm.CompletionResponse{Content: p.response}, nil
}

func TestLlmToolSummary_Success(t *testing.T) {
	provider := &mockSummaryProvider{response: "Searched for main function, found in main.go. Edited config file successfully."}

	msgs := []llm.Message{
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCallRef{
			{Name: "grep", Arguments: `{"pattern":"func main"}`},
		}},
		{Role: "tool", Content: "main.go:5:func main() {"},
	}

	summary, err := llmToolSummary(context.Background(), msgs, provider)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != provider.response {
		t.Errorf("got %q, want %q", summary, provider.response)
	}
}

func TestLlmToolSummary_FallbackOnError(t *testing.T) {
	provider := &mockSummaryProvider{err: fmt.Errorf("API rate limited")}

	msgs := []llm.Message{
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCallRef{
			{Name: "grep", Arguments: `{"pattern":"test"}`},
		}},
		{Role: "tool", Content: "found 5 matches"},
	}

	_, err := llmToolSummary(context.Background(), msgs, provider)
	if err == nil {
		t.Fatal("expected error from failing provider")
	}
}

func TestCompressContextIfNeeded_LLMSummaryPath(t *testing.T) {
	provider := &mockSummaryProvider{response: "Previously: searched codebase and edited 3 files."}

	msgs := []llm.Message{
		{Role: "system", Content: "system prompt"},
	}
	bigContent := strings.Repeat("x", 5000)
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			llm.Message{Role: "assistant", Content: bigContent, ToolCalls: []llm.ToolCallRef{
				{Name: "grep", Arguments: `{"pattern":"test"}`},
			}},
			llm.Message{Role: "tool", Content: bigContent},
		)
	}

	result := compressContextIfNeeded(context.Background(), msgs, provider, "test-session")

	// 应该使用 LLM 摘要
	hasSummary := false
	for _, m := range result {
		if strings.Contains(m.Content, "Previously: searched codebase") {
			hasSummary = true
			break
		}
	}
	if !hasSummary {
		t.Error("should use LLM-generated summary when provider available")
	}
}
