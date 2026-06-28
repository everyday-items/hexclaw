package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// 存储 / 向量检索 / 召回精度的确定性深度测试（无网络、可复现）。
// 用确定性向量直接灌库，绕开 embedder，专测 SQLite 存储层 + brute-force 向量扫描的正确性。

func deepStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewSQLiteStore(db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// unitVec 由 seed 确定性生成单位向量（不同 seed → 不同方向）。
func unitVec(seed, dim int) []float32 {
	v := make([]float32, dim)
	var n float64
	for i := 0; i < dim; i++ {
		x := math.Sin(float64(seed)*0.137 + float64(i)*1.13 + 0.5)
		v[i] = float32(x)
		n += x * x
	}
	n = math.Sqrt(n)
	if n == 0 {
		n = 1
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v
}

func chunkWith(docID string, idx int, content string, emb []float32) *Chunk {
	return &Chunk{
		ID: fmt.Sprintf("%s-c%d", docID, idx), DocID: docID, DocTitle: "T",
		Source: "deep", ChunkCount: 1, Content: content, Index: idx,
		Embedding: emb, CreatedAt: time.Now(),
	}
}

func idxList(rs []*SearchResult) []int {
	out := make([]int, len(rs))
	for i, r := range rs {
		out[i] = r.Chunk.Index
	}
	return out
}

// ① 向量全量扫描：第 10500 个 chunk（超旧 10000 静默上限）仍应被检索到 → 守护 cap 修复。
func TestStorageDeep_VectorScanBeyond10kCap(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	const dim, total, needle = 16, 10600, 10500
	doc := &Document{ID: "D", Title: "T", Source: "deep", ChunkCount: total, CreatedAt: time.Now(), Status: "indexed"}
	chunks := make([]*Chunk, total)
	for i := 0; i < total; i++ {
		seed := i + 1
		if i == needle {
			seed = 7777777 // needle 独特方向
		}
		chunks[i] = chunkWith("D", i, fmt.Sprintf("c%d", i), unitVec(seed, dim))
	}
	if err := s.Add(ctx, doc, chunks); err != nil {
		t.Fatalf("add %d chunks: %v", total, err)
	}
	res, err := s.VectorSearch(ctx, unitVec(7777777, dim), 5, Filter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) == 0 || res[0].Chunk.Index != needle {
		t.Fatalf("第 %d 个 needle（超旧 1 万上限）未 rank-1 命中 → 静默上限回归！top=%v", needle, idxList(res))
	}
	t.Logf("✓ 全量精确扫描 %d chunk，第 %d 个 needle rank-1 命中（无静默上限，self-sim=%.4f）", total, needle, res[0].VectorScore)
}

// ② float32 BLOB 往返精确 + brute-force 排序严格按 cos 降序。
func TestStorageDeep_BlobRoundTripAndExactRank(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	const dim = 32
	doc := &Document{ID: "D", Title: "T", Source: "deep", ChunkCount: 6, CreatedAt: time.Now()}
	var chunks []*Chunk
	for i := 0; i < 6; i++ {
		chunks = append(chunks, chunkWith("D", i, fmt.Sprintf("c%d", i), unitVec(i*100+1, dim)))
	}
	if err := s.Add(ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	res, err := s.VectorSearch(ctx, unitVec(2*100+1, dim), 6, Filter{}) // query == chunk2 的向量
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Chunk.Index != 2 {
		t.Fatalf("query==chunk2 向量应命中 chunk2 第一，got idx=%d", res[0].Chunk.Index)
	}
	if res[0].VectorScore < 0.999 {
		t.Errorf("BLOB 往返/自相似应≈1.0（精度无损），got %.5f", res[0].VectorScore)
	}
	for i := 1; i < len(res); i++ {
		if res[i-1].VectorScore < res[i].VectorScore-1e-9 {
			t.Errorf("brute-force 排序应严格 cos 降序，违反于 %d", i)
		}
	}
	t.Logf("✓ float32 BLOB 往返精确（self-sim=%.6f），brute-force 排序严格降序", res[0].VectorScore)
}

// ③ 持久化：关库重开后向量 + FTS 数据完整。
func TestStorageDeep_Persistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kb.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSQLiteStore(db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	doc := &Document{ID: "D", Title: "T", Source: "deep", ChunkCount: 1, CreatedAt: time.Now()}
	if err := s.Add(ctx, doc, []*Chunk{chunkWith("D", 0, "光合作用持久化关键词", unitVec(42, 16))}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close() // 关库

	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	s2 := NewSQLiteStore(db2)
	vres, _ := s2.VectorSearch(ctx, unitVec(42, 16), 3, Filter{})
	if len(vres) == 0 || vres[0].Chunk.Index != 0 {
		t.Fatalf("重开后向量数据未持久化")
	}
	tres, _ := s2.TextSearch(ctx, "持久化关键词", 3, Filter{})
	if len(tres) == 0 {
		t.Fatalf("重开后 FTS 索引未持久化")
	}
	t.Logf("✓ 关库重开后向量 + FTS5 数据完整持久化")
}

// ④ 删除级联：删文档后 chunk + FTS 索引都清除。
func TestStorageDeep_DeleteCascade(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	doc := &Document{ID: "D", Title: "T", Source: "deep", ChunkCount: 1, CreatedAt: time.Now()}
	if err := s.Add(ctx, doc, []*Chunk{chunkWith("D", 0, "唯一关键词zzz", unitVec(9, 16))}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.VectorSearch(ctx, unitVec(9, 16), 3, Filter{}); len(r) == 0 {
		t.Fatal("删除前应可检索")
	}
	if err := s.Delete(ctx, "D"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.VectorSearch(ctx, unitVec(9, 16), 3, Filter{}); len(r) != 0 {
		t.Errorf("删除后向量仍可检索（chunk 未级联删）")
	}
	if r, _ := s.TextSearch(ctx, "唯一关键词zzz", 3, Filter{}); len(r) != 0 {
		t.Errorf("删除后 FTS 仍可检索（FTS 未级联删）")
	}
	t.Logf("✓ 删除文档级联清除 chunk + FTS5 索引")
}

// ⑤ Replace 原子替换：旧 chunk 不残留为孤儿，新 chunk 生效。
func TestStorageDeep_ReplaceNoOrphan(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	doc := &Document{ID: "D", Title: "T", Source: "deep", ChunkCount: 3, CreatedAt: time.Now()}
	if err := s.Add(ctx, doc, []*Chunk{
		chunkWith("D", 0, "旧片段old0", unitVec(1, 16)),
		chunkWith("D", 1, "旧片段old1", unitVec(2, 16)),
		chunkWith("D", 2, "旧片段old2唯一", unitVec(3, 16)),
	}); err != nil {
		t.Fatal(err)
	}
	doc.ChunkCount = 2
	if err := s.Replace(ctx, doc, []*Chunk{
		chunkWith("D", 0, "新片段new0", unitVec(10, 16)),
		chunkWith("D", 1, "新片段new1", unitVec(11, 16)),
	}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.TextSearch(ctx, "old2唯一", 5, Filter{}); len(r) != 0 {
		t.Errorf("Replace 后旧 chunk 残留为孤儿")
	}
	if r, _ := s.TextSearch(ctx, "new0", 5, Filter{}); len(r) == 0 {
		t.Errorf("Replace 后新 chunk 缺失")
	}
	t.Logf("✓ Replace 原子替换 chunk，无孤儿残留")
}
