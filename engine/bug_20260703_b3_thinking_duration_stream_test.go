package engine

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	hexagon "github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// BUG-20260703 B3：思考时间在 runtime 流式路径丢失。
//
// 桌面 WS 聊天走 ProcessStream → runtime → finalizeRuntimeStreamResult，该路径的
// SaveAssistantReply 从不传 ThinkingDuration（legacy 流式路径 react.go 有计时，
// runtime 路径没有）——后端 meta 里根本没写过 thinking_duration，前端 A2 修复
// （loadMessages 补读 metaExt.thinking_duration）读回自然是空，切会话/重启后
// 思考时长徽标消失。
//
// 契约：reasoning 增量流式期间的真实耗时必须 ①随 done chunk 的 metadata 透出
// （live 一致性）②持久化进助手消息 meta（重载不丢）。
func TestBug20260703_B3_ThinkingDurationPersistedOnRuntimeStream(t *testing.T) {
	provider := &pacedReasoningStreamProvider{
		reasoningToContentDelay: 1100 * time.Millisecond, // 保证整秒粒度下 duration >= 1
	}

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
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {Model: "mock-model"},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{
		"test": provider,
	})
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	msg := &adapter.Message{
		ID:        "msg-b3-thinking-duration",
		Platform:  adapter.PlatformAPI,
		UserID:    "user-001",
		SessionID: "sess-b3-thinking",
		Content:   "算一下 1+1",
		Metadata:  map[string]string{"thinking": "on"},
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

	// ① live 一致性：done chunk metadata 携带 thinking_duration（前端 finalize 可回退取用）。
	durStr := done.Metadata["thinking_duration"]
	if durStr == "" {
		t.Fatalf("B3: done chunk metadata 缺 thinking_duration, metadata=%#v", done.Metadata)
	}
	if n, convErr := strconv.Atoi(durStr); convErr != nil || n < 1 {
		t.Fatalf("B3: thinking_duration 应 >= 1 秒（reasoning→content 间隔 1.1s），got %q", durStr)
	}

	// ② 持久化：助手消息 meta 含 thinking_duration，切会话/重启重载不丢。
	msgs, err := store.ListMessages(context.Background(), msg.SessionID, 10, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	found := false
	for _, m := range msgs {
		// SaveAssistantReply 的扩展元数据落 metadata 列；meta 列为兼容读取路径一并接受。
		if m.Role == "assistant" && (strings.Contains(m.Metadata, "thinking_duration") || strings.Contains(m.Meta, "thinking_duration")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("B3: 助手消息元数据未持久化 thinking_duration（重载即丢）。messages=%d", len(msgs))
	}
}

// pacedReasoningStreamProvider 流式先吐 reasoning 增量，停顿后再吐 content 增量，
// 模拟真实推理模型「思考 N 秒再作答」的时序。
type pacedReasoningStreamProvider struct {
	reasoningToContentDelay time.Duration
}

func (p *pacedReasoningStreamProvider) Name() string { return "test" }

func (p *pacedReasoningStreamProvider) Complete(context.Context, hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	return &hexagon.CompletionResponse{Content: "2", Usage: hexagon.Usage{TotalTokens: 4}}, nil
}

func (p *pacedReasoningStreamProvider) Stream(context.Context, hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	segments := []pacedSegment{
		{data: `data: {"choices":[{"index":0,"delta":{"reasoning":"用户在问 1+1，先想想。"}}]}` + "\n\n"},
		{delay: p.reasoningToContentDelay, data: `data: {"choices":[{"index":0,"delta":{"content":"1+1=2"}}]}` + "\n\n"},
		{data: `data: [DONE]` + "\n\n"},
	}
	return llm.NewStream(&pacedReader{segments: segments}, llm.StreamOpenAIFormat), nil
}

func (p *pacedReasoningStreamProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock Model"}}
}

func (p *pacedReasoningStreamProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

type pacedSegment struct {
	delay time.Duration
	data  string
}

// pacedReader 逐段吐 SSE 数据，段前可注入真实延迟（驱动计时逻辑）。
type pacedReader struct {
	segments []pacedSegment
	idx      int
	rest     string
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if r.rest == "" {
		if r.idx >= len(r.segments) {
			return 0, io.EOF
		}
		seg := r.segments[r.idx]
		r.idx++
		if seg.delay > 0 {
			time.Sleep(seg.delay)
		}
		r.rest = seg.data
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}
