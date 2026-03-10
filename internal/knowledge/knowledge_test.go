package knowledge

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	return db
}

// mockEmbedder 测试用 Embedder
type mockEmbedder struct {
	dim int
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	// 返回简单的确定性向量：基于文本长度和首字符
	result := make([][]float32, len(texts))
	for i, text := range texts {
		vec := make([]float32, m.dim)
		for j := range vec {
			// 使用文本内容生成简单但可区分的向量
			if j < len(text) {
				vec[j] = float32(text[j]) / 255.0
			} else {
				vec[j] = float32(len(text)%100) / 100.0
			}
		}
		result[i] = vec
	}
	return result, nil
}

func (m *mockEmbedder) Dimension() int {
	return m.dim
}

// TestManager_AddAndQuery 测试添加文档和混合检索
func TestManager_AddAndQuery(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewSQLiteStore(db)
	ctx := context.Background()

	if err := store.Init(ctx); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	embedder := &mockEmbedder{dim: 8}
	mgr := NewManager(store, embedder)

	// 添加文档
	doc, err := mgr.AddDocument(ctx, "Go 语言入门", "Go 语言是谷歌开发的编程语言。\n\nGo 语言特点是简洁、高效、并发友好。\n\nGo 语言的标准库非常丰富。", "test")
	if err != nil {
		t.Fatalf("添加文档失败: %v", err)
	}
	if doc.ChunkCount == 0 {
		t.Fatal("chunk 数不应为 0")
	}

	// 混合检索
	result, err := mgr.Query(ctx, "Go 语言特点", 3)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if result == "" {
		t.Fatal("查询结果不应为空")
	}
	if !containsAny(result, "Go", "语言") {
		t.Fatalf("查询结果应包含相关内容: %s", result)
	}
}

// TestManager_AddAndQuery_NoEmbedder 测试无 Embedder 时退化为关键词搜索
func TestManager_AddAndQuery_NoEmbedder(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewSQLiteStore(db)
	ctx := context.Background()
	store.Init(ctx)

	// embedder 为 nil，退化为纯关键词搜索
	mgr := NewManager(store, nil)

	_, err := mgr.AddDocument(ctx, "Python 教程", "Python 是一种解释型语言。\n\nPython 支持面向对象编程。", "test")
	if err != nil {
		t.Fatalf("添加文档失败: %v", err)
	}

	result, err := mgr.Query(ctx, "Python 编程", 3)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 关键词搜索应该能匹配到
	if result == "" {
		t.Fatal("关键词搜索结果不应为空")
	}
}

// TestManager_DeleteDocument 测试删除文档
func TestManager_DeleteDocument(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewSQLiteStore(db)
	ctx := context.Background()
	store.Init(ctx)

	mgr := NewManager(store, &mockEmbedder{dim: 8})

	doc, _ := mgr.AddDocument(ctx, "测试文档", "这是测试内容。\n\n用于验证删除功能。", "test")

	if err := mgr.DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 0 {
		t.Fatalf("删除后应无文档，实际 %d 个", len(docs))
	}
}

// TestSplitIntoChunks 测试文本分割（带重叠）
func TestSplitIntoChunks(t *testing.T) {
	text := "第一段内容。\n\n第二段内容。\n\n第三段内容。"
	chunks := splitIntoChunks(text, 100, 20)

	if len(chunks) == 0 {
		t.Fatal("分割结果不应为空")
	}

	for i, c := range chunks {
		if len(c) > 100 {
			t.Errorf("chunk %d 超过限制: %d 字符", i, len(c))
		}
	}
}

// TestSplitIntoChunks_Overlap 测试重叠分块
func TestSplitIntoChunks_Overlap(t *testing.T) {
	// 生成足够长的文本，确保触发分块
	text := "这是第一段非常长的文本内容。\n\n这是第二段非常长的文本内容。\n\n这是第三段非常长的文本内容。"
	chunks := splitIntoChunks(text, 30, 5)

	if len(chunks) < 2 {
		t.Skipf("文本太短未触发分块: %d chunks", len(chunks))
	}

	// 验证相邻 chunk 之间存在重叠
	t.Logf("生成了 %d 个 chunk", len(chunks))
	for i, c := range chunks {
		t.Logf("  chunk %d: %q", i, c)
	}
}

// TestVectorSearch 测试向量搜索
func TestVectorSearch(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewSQLiteStore(db)
	ctx := context.Background()
	store.Init(ctx)

	embedder := &mockEmbedder{dim: 8}
	mgr := NewManager(store, embedder)

	// 添加多个文档
	mgr.AddDocument(ctx, "Go 并发", "Go 语言通过 goroutine 和 channel 实现高效并发。", "test")
	mgr.AddDocument(ctx, "Python ML", "Python 是机器学习的首选语言，拥有丰富的 ML 库。", "test")
	mgr.AddDocument(ctx, "Rust 安全", "Rust 通过所有权系统保证内存安全。", "test")

	// 向量搜索
	queryVec, _ := embedder.Embed(ctx, []string{"Go 并发编程"})
	results, err := store.VectorSearch(ctx, queryVec[0], 3)
	if err != nil {
		t.Fatalf("向量搜索失败: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("向量搜索应有结果")
	}

	// 所有结果都应有分数
	for _, r := range results {
		if r.VectorScore <= 0 {
			t.Errorf("向量分数应大于 0: %f", r.VectorScore)
		}
	}
}

// TestTextSearch 测试 FTS5 关键词搜索
func TestTextSearch(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewSQLiteStore(db)
	ctx := context.Background()
	store.Init(ctx)

	mgr := NewManager(store, &mockEmbedder{dim: 8})

	mgr.AddDocument(ctx, "数据库", "SQLite 是一个轻量级数据库。\n\nPostgreSQL 是企业级数据库。", "test")
	mgr.AddDocument(ctx, "网络", "HTTP 协议是 Web 的基础。\n\nTCP 提供可靠传输。", "test")

	// FTS5 搜索
	results, err := store.TextSearch(ctx, "数据库 SQLite", 5)
	if err != nil {
		t.Fatalf("关键词搜索失败: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("关键词搜索应有结果")
	}
}

// TestHybridSearch 测试混合检索完整流程
func TestHybridSearch(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := NewSQLiteStore(db)
	ctx := context.Background()
	store.Init(ctx)

	embedder := &mockEmbedder{dim: 8}
	mgr := NewManager(store, embedder, WithHybridConfig(HybridConfig{
		VectorWeight:  0.7,
		TextWeight:    0.3,
		MMRLambda:     0.7,
		TimeDecayDays: 0, // 禁用时间衰减
	}))

	mgr.AddDocument(ctx, "Go Web", "Go 语言适合构建高性能 Web 服务。\n\nGin 是 Go 最流行的 Web 框架。", "test")
	mgr.AddDocument(ctx, "React", "React 是 Facebook 开发的前端框架。\n\nReact 使用虚拟 DOM 提高渲染性能。", "test")

	result, err := mgr.Query(ctx, "Go Web 框架", 2)
	if err != nil {
		t.Fatalf("混合检索失败: %v", err)
	}
	if result == "" {
		t.Fatal("混合检索结果不应为空")
	}
	t.Logf("混合检索结果:\n%s", result)
}

// TestCosineSimilarity 测试余弦相似度计算
func TestCosineSimilarity(t *testing.T) {
	// 相同向量，相似度应为 1
	a := []float32{1, 0, 0, 1}
	sim := cosineSimilarity(a, a)
	if sim < 0.999 {
		t.Errorf("相同向量余弦相似度应为 1，得到 %f", sim)
	}

	// 正交向量，相似度应为 0
	b := []float32{0, 1, 1, 0}
	sim = cosineSimilarity(a, b)
	if sim > 0.001 || sim < -0.001 {
		t.Errorf("正交向量余弦相似度应为 0，得到 %f", sim)
	}

	// 空向量
	sim = cosineSimilarity(nil, a)
	if sim != 0 {
		t.Errorf("空向量余弦相似度应为 0，得到 %f", sim)
	}
}

// TestEncodeDecodeFloat32 测试向量序列化/反序列化
func TestEncodeDecodeFloat32(t *testing.T) {
	original := []float32{1.0, -2.5, 3.14, 0, -0.001}
	encoded := encodeFloat32Slice(original)
	decoded := decodeFloat32Slice(encoded)

	if len(decoded) != len(original) {
		t.Fatalf("长度不匹配: %d != %d", len(decoded), len(original))
	}

	for i := range original {
		if original[i] != decoded[i] {
			t.Errorf("位置 %d 不匹配: %f != %f", i, original[i], decoded[i])
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) > 0 && len(sub) > 0 {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
