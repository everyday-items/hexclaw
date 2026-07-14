package knowledge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
)

// BUG-20260714：百页 PDF 会产生数百个 chunk。旧实现仍逐块调用 LLM（最多 200 次），
// 导致一次上传在“后端正在解析并建索引”停留数分钟。大文档应保留确定性定位前缀，
// 但跳过逐块 LLM 增强，让摄取耗时随文本分块近似线性、而不是随模型调用次数增长。
func TestBUG20260714_LargeDocumentSkipsPerChunkLLMButKeepsLocation(t *testing.T) {
	cfg := DefaultHybridConfig()
	llm := &rpFakeLLM{reply: "不应调用"}
	mgr := ctxMgr(t, cfg, llm)
	docs := make([]hexagon.Document, maxInlineContextualLLMChunks+1)
	for i := range docs {
		docs[i] = hexagon.Document{
			ID:       "chunk",
			Content:  "教材正文",
			Metadata: map[string]any{"header_path": "第五单元 > 第三课"},
		}
	}

	mgr.contextualize(context.Background(), &Document{Title: "数学五年级下册", Content: strings.Repeat("教材正文", 2000)}, docs)

	if len(llm.calls) != 0 {
		t.Fatalf("大文档不应逐 chunk 调 LLM，实际调用 %d 次", len(llm.calls))
	}
	for i, doc := range docs {
		if !strings.Contains(doc.Content, "【定位】数学五年级下册 › 第五单元 > 第三课") {
			t.Fatalf("chunk %d 应保留确定性定位前缀，got %q", i, doc.Content)
		}
		if strings.Contains(doc.Content, "【情境】") {
			t.Fatalf("chunk %d 不应带 LLM 情境，got %q", i, doc.Content)
		}
	}
}

type deadlineAwareEmbedder struct{}

func (deadlineAwareEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (deadlineAwareEmbedder) EmbedOne(ctx context.Context, _ string) ([]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (deadlineAwareEmbedder) Dimension() int { return 768 }

type fixedCountSplitter struct{ count int }

func (s fixedCountSplitter) Split(_ context.Context, _ []hexagon.Document) ([]hexagon.Document, error) {
	docs := make([]hexagon.Document, s.count)
	for i := range docs {
		docs[i] = hexagon.Document{ID: "chunk", Content: "教材正文"}
	}
	return docs, nil
}

func (fixedCountSplitter) Name() string { return "fixed-count" }

type delayedCompleteEmbedder struct{ delay time.Duration }

func (e delayedCompleteEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	timer := time.NewTimer(e.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e delayedCompleteEmbedder) EmbedOne(ctx context.Context, _ string) ([]float32, error) {
	result, err := e.Embed(ctx, []string{"one"})
	if err != nil {
		return nil, err
	}
	return result[0], nil
}

func (delayedCompleteEmbedder) Dimension() int { return 3 }

// 本地 nomic-embed-text 按 100 个 chunk 一批处理。百页教材通常需要三批以上，
// 文档预算必须随批次数增长；否则第一批虽已算完，第二批命中固定 60 秒总预算后，
// 缓存/批处理层会把前面结果连同失败一起丢弃，最终静默落成 0 向量。
func TestBUG20260714_LargeDocumentEmbeddingBudgetCoversAllBatches(t *testing.T) {
	previous := documentEmbeddingTimeout
	documentEmbeddingTimeout = 30 * time.Millisecond
	t.Cleanup(func() { documentEmbeddingTimeout = previous })

	cfg := DefaultHybridConfig()
	cfg.ContextualEnabled = false
	mgr := NewManager(rpFakeRepo{}, &rpFakeSearcher{}, delayedCompleteEmbedder{delay: 50 * time.Millisecond},
		WithHybridConfig(cfg), WithSplitter(fixedCountSplitter{count: 201}))

	chunks, err := mgr.buildChunks(context.Background(), &Document{
		ID: "doc-large-vector", Title: "数学五年级下册", Content: "教材正文",
	}, time.Now())
	if err != nil {
		t.Fatalf("buildChunks: %v", err)
	}
	if len(chunks) != 201 {
		t.Fatalf("chunks=%d, want 201", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk.Embedding) != 3 {
			t.Fatalf("chunk %d embedding dim=%d, want 3；大文档不能因固定单批预算静默丢失全部向量", i, len(chunk.Embedding))
		}
	}
}

// nomic-embed-text 尚在下载/启动时，嵌入 HTTP 可能长期无响应。摄取应在独立预算耗尽后
// 降级为 FTS 文本索引，而不是吃完整个上传 5 分钟总超时、最终一条 chunk 都不落库。
func TestBUG20260714_DocumentEmbeddingTimeoutFallsBackToTextChunks(t *testing.T) {
	previous := documentEmbeddingTimeout
	documentEmbeddingTimeout = 10 * time.Millisecond
	t.Cleanup(func() { documentEmbeddingTimeout = previous })

	cfg := DefaultHybridConfig()
	cfg.ContextualEnabled = false
	mgr := NewManager(rpFakeRepo{}, &rpFakeSearcher{}, deadlineAwareEmbedder{},
		WithHybridConfig(cfg), WithSplitter(testSplitter()))
	doc := &Document{ID: "doc-large", Title: "数学五年级下册", Content: strings.Repeat("长方体的体积公式。", 200)}

	start := time.Now()
	chunks, err := mgr.buildChunks(context.Background(), doc, time.Now())
	if err != nil {
		t.Fatalf("embedding 超时应降级为文本 chunk，got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("embedding 降级不应长时间阻塞，elapsed=%s", elapsed)
	}
	if len(chunks) == 0 {
		t.Fatal("降级后仍应生成文本 chunks")
	}
	for i, chunk := range chunks {
		if len(chunk.Embedding) != 0 {
			t.Fatalf("chunk %d 超时降级后不应伪造 embedding", i)
		}
	}
}
