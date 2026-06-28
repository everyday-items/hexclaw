package knowledge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/splitter"
)

// recordingEmbedder 记录所有被嵌入的文本，用于断言 query/doc 前缀是否真的作用到 embedding 输入。
type recordingEmbedder struct {
	dim int
	all []string
}

func (r *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.all = append(r.all, texts...)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func (r *recordingEmbedder) EmbedOne(_ context.Context, t string) ([]float32, error) {
	r.all = append(r.all, t)
	return []float32{1, 0, 0}, nil
}

func (r *recordingEmbedder) Dimension() int { return r.dim }

func (r *recordingEmbedder) sawPrefix(p string) bool {
	for _, s := range r.all {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// #12 query/doc 嵌入非对称：查询侧嵌入带 query 前缀、文档侧嵌入带 doc 前缀；
// 且前缀只作用于 embedding 输入，不污染存储内容（Chunk.Content）。
func TestEmbedAsymmetry_PrefixesApplied(t *testing.T) {
	rec := &recordingEmbedder{dim: 3}
	cfg := DefaultHybridConfig()
	cfg.RerankEnabled, cfg.ExpandEnabled, cfg.ContextualEnabled = false, false, false
	cfg.EmbedQueryPrefix, cfg.EmbedDocPrefix = "search_query: ", "search_document: "
	sp := splitter.NewRecursiveSplitter(splitter.WithRecursiveChunkSize(400), splitter.WithRecursiveChunkOverlap(40))
	mgr := NewManager(rpFakeRepo{}, &rpFakeSearcher{}, rec, WithHybridConfig(cfg), WithSplitter(sp))

	// 文档侧：入库分块嵌入应带 doc 前缀，但 Chunk.Content 仍是原文（无前缀）
	chunks, err := mgr.buildChunks(context.Background(), &Document{ID: "d1", Title: "标题", Content: "这是一段知识库正文内容"}, time.Now())
	if err != nil {
		t.Fatalf("buildChunks: %v", err)
	}
	if !rec.sawPrefix("search_document: ") {
		t.Errorf("文档嵌入应带 doc 前缀 search_document: ，实际嵌入文本=%v", rec.all)
	}
	for _, c := range chunks {
		if strings.HasPrefix(c.Content, "search_document: ") {
			t.Errorf("Chunk.Content 不应被前缀污染（前缀只作用于 embedding 输入），got %q", c.Content)
		}
	}

	// 查询侧：检索时查询嵌入应带 query 前缀
	rec.all = nil
	if _, err := mgr.Search(context.Background(), "我的查询词", 3); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !rec.sawPrefix("search_query: ") {
		t.Errorf("查询嵌入应带 query 前缀 search_query: ，实际嵌入文本=%v", rec.all)
	}
}
