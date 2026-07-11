package engine

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
)

// U9：知识库命中标签恒 false 的根因是——引擎自动 RAG 注入把命中喂给了 LLM，却从不把
// 命中回填到响应（Reply/ReplyChunk 的 Metadata 是 map[string]string，结构上无法承载对象
// 数组）。本测试钉死修复后的结构化契约：一次带知识库命中的响应，其 Reply / Done chunk 的
// 结构化 KnowledgeHits 字段必须被填充（含 doc_title/content，对齐前端 ChatView 消费路径）。

// TestU9_KnowledgeHitsInReply 非流式 Process：Reply.KnowledgeHits 被填充。
func TestU9_KnowledgeHitsInReply(t *testing.T) {
	const doc = "jiuhe company address: hangzhou west lake district cloud town"
	const query = "jiuhe company address"

	capture := &capturingProviderB8{}
	eng, kb := newEngineWithKB(t, capture, map[string][]float32{
		query: {1, 0, 0, 0},
		doc:   {1, 0, 0, 0}, // cos=1 → 过 0.55 地板，强命中
	})
	if _, err := kb.AddDocument(context.Background(), "九河科技介绍", doc, "test-source"); err != nil {
		t.Fatal(err)
	}

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-u9-sync", Platform: adapter.PlatformAPI, UserID: "u-1", SessionID: "sess-u9-sync",
		Content: query,
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reply == nil {
		t.Fatal("应有回复")
	}
	if len(reply.KnowledgeHits) == 0 {
		t.Fatalf("U9: 带知识库命中的响应，Reply.KnowledgeHits 未被填充（命中标签恒 false 的根因）")
	}
	h := reply.KnowledgeHits[0]
	if h.DocTitle == "" && h.Content == "" {
		t.Fatalf("U9: KnowledgeHit 结构缺 doc_title/content，前端无法渲染详情: %+v", h)
	}
}

// TestU9_KnowledgeHitsInDoneChunk 流式 ProcessStream：Done chunk 的 KnowledgeHits 被填充。
func TestU9_KnowledgeHitsInDoneChunk(t *testing.T) {
	const doc = "acme rocket fuel formula: liquid hydrogen and oxygen mixture"
	const query = "acme rocket fuel formula"

	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "配方如上。", Usage: hexagon.Usage{TotalTokens: 4}}, nil
		})
	eng, kb := newEngineWithKB(t, provider, map[string][]float32{
		query: {1, 0, 0, 0},
		doc:   {1, 0, 0, 0},
	})
	if _, err := kb.AddDocument(context.Background(), "Acme 燃料", doc, "test-source"); err != nil {
		t.Fatal(err)
	}

	ch, err := eng.ProcessStream(context.Background(), &adapter.Message{
		ID: "msg-u9-stream", Platform: adapter.PlatformAPI, UserID: "u-1", SessionID: "sess-u9-stream",
		Content: query,
	})
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	var done *adapter.ReplyChunk
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("流式错误: %v", chunk.Error)
		}
		if chunk.Done {
			copied := *chunk
			done = &copied
		}
	}
	if done == nil {
		t.Fatal("未收到 done chunk")
	}
	if len(done.KnowledgeHits) == 0 {
		t.Fatalf("U9: 带知识库命中的流式响应，Done chunk.KnowledgeHits 未被填充")
	}
}
