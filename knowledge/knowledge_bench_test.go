package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"testing"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	_ "modernc.org/sqlite"
)

func benchSetup(b *testing.B, numDocs int) (*Manager, *SQLiteStore, context.Context) {
	b.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })

	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		b.Fatal(err)
	}

	embedder := &mockEmbedder{dim: 128}
	sp := splitter.NewRecursiveSplitter(
		splitter.WithRecursiveChunkSize(400),
		splitter.WithRecursiveChunkOverlap(80),
	)
	mgr := NewManager(store, store, embedder, WithSplitter(sp), WithHybridConfig(HybridConfig{
		VectorWeight:  0.7,
		TextWeight:    0.3,
		MMRLambda:     0.7,
		TimeDecayDays: 0,
	}))

	for i := 0; i < numDocs; i++ {
		content := fmt.Sprintf("这是第 %d 篇文档的内容。\n\n包含多个段落用于测试分块性能。\n\n段落三提供更多上下文信息。", i)
		if _, err := mgr.AddDocument(ctx, fmt.Sprintf("文档-%d", i), content, "bench"); err != nil {
			b.Fatal(err)
		}
	}

	return mgr, store, ctx
}

// BenchmarkVectorSearch_100docs 100 篇文档的向量搜索
func BenchmarkVectorSearch_100docs(b *testing.B) {
	_, store, ctx := benchSetup(b, 100)
	queryVec := make([]float32, 128)
	for i := range queryVec {
		queryVec[i] = rand.Float32()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.VectorSearch(ctx, queryVec, 5)
	}
}

// BenchmarkHybridSearch_100docs 100 篇文档的混合检索
func BenchmarkHybridSearch_100docs(b *testing.B) {
	mgr, _, ctx := benchSetup(b, 100)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mgr.Search(ctx, "文档 段落 性能", 5)
	}
}

// BenchmarkCosineSimilarity_1536dim 1536维余弦相似度计算
func BenchmarkCosineSimilarity_1536dim(b *testing.B) {
	a := make([]float32, 1536)
	c := make([]float32, 1536)
	for i := range a {
		a[i] = rand.Float32()
		c[i] = rand.Float32()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cosineSimilarity(a, c)
	}
}
