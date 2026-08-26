package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// BUG-20260703 B5（跨仓集成锁）：hexagon 根修「终态回答进块流」必须穿过 hexclaw
// 引擎完整透出——工具轮之后的最终回答，①在 ProcessStream 的 done chunk .Blocks
// 尾部以 text 块出现（live 渲染真相）②持久化进助手消息元数据的 blocks（重载真相）。
//
// 这是 hexagon runner 修复与 hexclaw 引擎/落库之间的「跨仓缝」：hexagon 单元测试
// 绿 ≠ 这条缝通（dev 下 go.work 生效，发版后靠版本锁定，本测试两态都守）。
func TestBug20260703_B5_StreamDoneChunkBlocksEndWithFinalText(t *testing.T) {
	const finalAnswer = "杭州今天 27°C，适合外出。"
	// mockllm 的 Stream 只合成 content、不带 ToolCalls，无法驱动流式工具轮——
	// 用自定义 SSE provider：首轮吐 tool_calls delta，次轮吐最终正文。
	provider := &toolThenTextStreamProvider{finalAnswer: finalAnswer}

	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": provider})
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	msg := &adapter.Message{
		ID: "msg-b5-stream-blocks", Platform: adapter.PlatformAPI,
		UserID: "user-001", SessionID: "sess-b5-stream",
		Content: "杭州天气如何",
	}
	ch, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream 失败: %v", err)
	}
	var done *adapter.ReplyChunk
	for chunk := range ch {
		if chunk.Done {
			copied := *chunk
			done = &copied
		}
	}
	if done == nil {
		t.Fatal("未收到 done chunk")
	}

	// ① live 真相：done chunk 块流末位是最终回答 text 块
	if len(done.Blocks) == 0 {
		t.Fatalf("B5: done chunk 无块流（多步工具轮应产出有序内容块）")
	}
	last := done.Blocks[len(done.Blocks)-1]
	if last.Type != "text" || !strings.Contains(last.Text, finalAnswer) {
		t.Fatalf("B5: done chunk 块流末位应为最终回答 text 块（否则正文蒸发）, blocks=%+v", done.Blocks)
	}
	hasToolUse := false
	for _, b := range done.Blocks {
		if b.Type == "tool_use" {
			hasToolUse = true
		}
	}
	if !hasToolUse {
		t.Fatalf("B5: 块流应保留工具块（交错序）, blocks=%+v", done.Blocks)
	}

	// ② 重载真相：落库元数据的 blocks 含最终回答（切会话/重启后不丢）
	msgs, err := store.ListMessages(context.Background(), msg.SessionID, 10, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	persisted := false
	for _, m := range msgs {
		if m.Role == "assistant" &&
			(strings.Contains(m.Metadata, "blocks") || strings.Contains(m.Meta, "blocks")) &&
			(strings.Contains(m.Metadata, finalAnswer) || strings.Contains(m.Meta, finalAnswer)) {
			persisted = true
		}
	}
	if !persisted {
		t.Fatalf("B5: 落库元数据未含带最终回答的 blocks（重载后正文蒸发）。messages=%d", len(msgs))
	}
}

// toolThenTextStreamProvider 流式先发一次工具调用（turn 1），工具结果回填后吐最终
// 正文（turn 2）——离线复刻「web_search 后作答」的多步流式工具轮。
type toolThenTextStreamProvider struct {
	firstContent string
	finalAnswer  string
}

func (p *toolThenTextStreamProvider) Name() string { return "test" }

func (p *toolThenTextStreamProvider) Complete(_ context.Context, req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			return &hexagon.CompletionResponse{Content: p.finalAnswer, Usage: hexagon.Usage{TotalTokens: 8}}, nil
		}
	}
	return &hexagon.CompletionResponse{
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "web_search", Arguments: `{"q":"杭州天气"}`}},
		Usage:     hexagon.Usage{TotalTokens: 6},
	}, nil
}

func (p *toolThenTextStreamProvider) Stream(_ context.Context, req hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	hasToolResult := false
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			hasToolResult = true
		}
	}
	var body string
	if hasToolResult {
		body = `data: {"choices":[{"index":0,"delta":{"content":"` + p.finalAnswer + `"},"finish_reason":"stop"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n"
	} else {
		if p.firstContent != "" {
			body = `data: {"choices":[{"index":0,"delta":{"content":"` + p.firstContent + `"}}]}` + "\n\n"
		}
		body += `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"q\":\"杭州天气\"}"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n"
	}
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func (p *toolThenTextStreamProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock Model"}}
}

func (p *toolThenTextStreamProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

func TestProcessStreamPersistsAllVisibleMultiTurnContent(t *testing.T) {
	const (
		firstContent = "I will check first. "
		finalAnswer  = "The verified answer is 12."
	)
	provider := &toolThenTextStreamProvider{
		firstContent: firstContent,
		finalAnswer:  finalAnswer,
	}
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("initialize store: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.Knowledge.Enabled = false
	cfg.FileMemory.Enabled = false
	cfg.FileMemory.AutoMemory = "off"
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": provider})
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	msg := &adapter.Message{
		ID:        "request-multi-turn-visible-content",
		Platform:  adapter.PlatformAPI,
		UserID:    "user-multi-turn-visible-content",
		SessionID: "session-multi-turn-visible-content",
		Content:   "Use the tool and answer.",
		Metadata: map[string]string{
			"request_id": "request-multi-turn-visible-content",
		},
	}
	stream, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	var visible strings.Builder
	var terminal *adapter.ReplyChunk
	for chunk := range stream {
		if chunk == nil {
			continue
		}
		visible.WriteString(chunk.Content)
		if chunk.Done {
			copy := *chunk
			terminal = &copy
		}
	}
	if terminal == nil || terminal.Error != nil {
		t.Fatalf("terminal chunk: %+v", terminal)
	}
	if got, want := visible.String(), firstContent+finalAnswer; got != want {
		t.Fatalf("visible content=%q want=%q", got, want)
	}
	persisted, err := store.GetMessage(context.Background(), terminal.AssistantMessageID)
	if err != nil {
		t.Fatalf("get persisted assistant: %v", err)
	}
	if persisted.Content != visible.String() {
		t.Fatalf("persisted content=%q visible content=%q", persisted.Content, visible.String())
	}
}
