package knowledge

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// SQLiteStore SQLite 知识库存储（FTS5 + 向量）
//
// 存储结构：
//   - kb_documents: 文档元信息
//   - kb_chunks: 文档片段 + 向量嵌入（BLOB）
//   - kb_chunks_fts: FTS5 全文索引虚拟表（自动同步 kb_chunks）
//
// 向量存储采用 float32 序列化为 BLOB 的方式，
// 余弦相似度在 Go 层计算。对于个人知识库规模（< 10万 chunk），
// 这种方案性能完全够用，且避免了 CGO/sqlite-vec 的编译依赖。
//
// FTS5 使用 SQLite 内置的全文搜索引擎，支持 BM25 排名。
type SQLiteStore struct {
	db        *sql.DB
	chunkSize int
}

// NewSQLiteStore 创建 SQLite 知识库存储
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{
		db:        db,
		chunkSize: 400,
	}
}

// Init 初始化知识库表 + FTS5 索引
func (s *SQLiteStore) Init(ctx context.Context) error {
	queries := []string{
		// 文档表
		`CREATE TABLE IF NOT EXISTS kb_documents (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			content TEXT NOT NULL,
			source TEXT DEFAULT '',
			chunk_count INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,

		// Chunk 表（含向量嵌入 BLOB）
		`CREATE TABLE IF NOT EXISTS kb_chunks (
			id TEXT PRIMARY KEY,
			doc_id TEXT NOT NULL,
			content TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			embedding BLOB,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (doc_id) REFERENCES kb_documents(id) ON DELETE CASCADE
		)`,

		`CREATE INDEX IF NOT EXISTS idx_kb_chunks_doc ON kb_chunks(doc_id)`,

		// FTS5 全文索引
		// 存储 chunk 内容和 chunk_id，用于关键词搜索
		`CREATE VIRTUAL TABLE IF NOT EXISTS kb_chunks_fts USING fts5(
			content,
			chunk_id UNINDEXED
		)`,
	}

	for _, q := range queries {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("初始化知识库表失败: %w", err)
		}
	}
	return nil
}

// AddDocument 添加文档及其 chunk（含向量和 FTS5 索引）
func (s *SQLiteStore) AddDocument(ctx context.Context, doc *Document, chunks []*Chunk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 插入文档
	_, err = tx.ExecContext(ctx,
		`INSERT INTO kb_documents (id, title, content, source, chunk_count, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		doc.ID, doc.Title, doc.Content, doc.Source, doc.ChunkCount, doc.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("插入文档失败: %w", err)
	}

	// 插入 chunk + FTS5 索引
	for _, chunk := range chunks {
		// 序列化向量为 BLOB
		var embBlob []byte
		if len(chunk.Embedding) > 0 {
			embBlob = encodeFloat32Slice(chunk.Embedding)
		}

		_, err = tx.ExecContext(ctx,
			`INSERT INTO kb_chunks (id, doc_id, content, chunk_index, embedding, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			chunk.ID, chunk.DocID, chunk.Content, chunk.Index, embBlob, chunk.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("插入 chunk 失败: %w", err)
		}

		// 同步到 FTS5 索引
		tx.ExecContext(ctx,
			`INSERT INTO kb_chunks_fts (content, chunk_id) VALUES (?, ?)`,
			chunk.Content, chunk.ID,
		)
	}

	return tx.Commit()
}

// DeleteDocument 删除文档及其 chunk + FTS5 索引
func (s *SQLiteStore) DeleteDocument(ctx context.Context, docID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 删除 FTS5 索引中的对应记录
	tx.ExecContext(ctx,
		`DELETE FROM kb_chunks_fts WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id = ?)`,
		docID,
	)

	// 删除 chunk 和文档
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks WHERE doc_id = ?`, docID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_documents WHERE id = ?`, docID); err != nil {
		return err
	}

	return tx.Commit()
}

// ListDocuments 列出所有文档
func (s *SQLiteStore) ListDocuments(ctx context.Context) ([]*Document, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, source, chunk_count, created_at FROM kb_documents ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []*Document
	for rows.Next() {
		doc := &Document{}
		if err := rows.Scan(&doc.ID, &doc.Title, &doc.Source, &doc.ChunkCount, &doc.CreatedAt); err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

// VectorSearch 向量搜索
//
// 加载所有 chunk 的向量，在 Go 层计算余弦相似度，
// 返回相似度最高的 topK 个结果。
//
// 对于个人知识库（通常 < 10万 chunk），这种全扫描方式
// 性能完全够用（10万个 1536 维向量约需 ~100ms）。
func (s *SQLiteStore) VectorSearch(ctx context.Context, queryVec []float32, topK int) ([]*SearchResult, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, doc_id, content, chunk_index, embedding, created_at FROM kb_chunks WHERE embedding IS NOT NULL`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 计算所有 chunk 与查询向量的余弦相似度
	type scored struct {
		chunk *Chunk
		sim   float64
	}
	var all []scored

	for rows.Next() {
		chunk := &Chunk{}
		var embBlob []byte
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.Index, &embBlob, &chunk.CreatedAt); err != nil {
			return nil, err
		}

		if len(embBlob) > 0 {
			chunk.Embedding = decodeFloat32Slice(embBlob)
			sim := cosineSimilarity(queryVec, chunk.Embedding)
			// 余弦相似度归一化到 0-1（原始范围 -1 到 1）
			normalizedSim := (sim + 1) / 2
			all = append(all, scored{chunk: chunk, sim: normalizedSim})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 按相似度降序排序
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].sim > all[j-1].sim; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}

	// 取 topK
	if len(all) > topK {
		all = all[:topK]
	}

	results := make([]*SearchResult, len(all))
	for i, s := range all {
		results[i] = &SearchResult{
			Chunk:       s.chunk,
			VectorScore: s.sim,
		}
	}
	return results, nil
}

// TextSearch FTS5 关键词搜索
//
// 使用 SQLite FTS5 的 BM25 排名算法进行全文搜索。
// BM25 分数越小（负数绝对值越大）越相关，需要归一化到 0-1。
func (s *SQLiteStore) TextSearch(ctx context.Context, query string, topK int) ([]*SearchResult, error) {
	// 构建 FTS5 查询：将查询分词后用 OR 连接
	keywords := tokenize(query)
	if len(keywords) == 0 {
		return nil, nil
	}

	// FTS5 查询语法：用 OR 连接多个关键词
	ftsQuery := strings.Join(keywords, " OR ")

	rows, err := s.db.QueryContext(ctx,
		`SELECT f.chunk_id, f.content, bm25(kb_chunks_fts) as score
		 FROM kb_chunks_fts f
		 WHERE kb_chunks_fts MATCH ?
		 ORDER BY score
		 LIMIT ?`,
		ftsQuery, topK,
	)
	if err != nil {
		// FTS5 查询失败（可能是特殊字符），降级到 LIKE 搜索
		return s.fallbackTextSearch(ctx, keywords, topK)
	}
	defer rows.Close()

	var results []*SearchResult
	var minScore, maxScore float64
	minScore = math.Inf(1)
	maxScore = math.Inf(-1)

	type rawResult struct {
		chunkID string
		content string
		score   float64
	}
	var raw []rawResult

	for rows.Next() {
		var r rawResult
		if err := rows.Scan(&r.chunkID, &r.content, &r.score); err != nil {
			return nil, err
		}
		// BM25 返回负数，绝对值越大越相关
		absScore := math.Abs(r.score)
		if absScore < minScore {
			minScore = absScore
		}
		if absScore > maxScore {
			maxScore = absScore
		}
		raw = append(raw, rawResult{chunkID: r.chunkID, content: r.content, score: absScore})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 归一化 BM25 分数到 0-1
	scoreRange := maxScore - minScore
	for _, r := range raw {
		normalizedScore := 0.5 // 只有一个结果时
		if scoreRange > 0 {
			normalizedScore = (r.score - minScore) / scoreRange
		}

		// 从 kb_chunks 获取完整信息
		chunk := s.getChunkByID(ctx, r.chunkID)
		if chunk == nil {
			chunk = &Chunk{
				ID:      r.chunkID,
				Content: r.content,
			}
		}

		results = append(results, &SearchResult{
			Chunk:     chunk,
			TextScore: normalizedScore,
		})
	}

	return results, nil
}

// fallbackTextSearch FTS5 不可用时的降级搜索（LIKE 匹配）
func (s *SQLiteStore) fallbackTextSearch(ctx context.Context, keywords []string, topK int) ([]*SearchResult, error) {
	var conditions []string
	var args []any
	for _, kw := range keywords {
		conditions = append(conditions, "content LIKE ?")
		args = append(args, "%"+kw+"%")
	}

	whereClause := strings.Join(conditions, " OR ")
	q := fmt.Sprintf(
		`SELECT id, doc_id, content, chunk_index, embedding, created_at FROM kb_chunks WHERE %s LIMIT ?`,
		whereClause,
	)
	args = append(args, topK)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*SearchResult
	for rows.Next() {
		chunk := &Chunk{}
		var embBlob []byte
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.Index, &embBlob, &chunk.CreatedAt); err != nil {
			return nil, err
		}
		if len(embBlob) > 0 {
			chunk.Embedding = decodeFloat32Slice(embBlob)
		}

		// 简单评分：匹配关键词数 / 总关键词数
		matchCount := 0
		for _, kw := range keywords {
			if strings.Contains(strings.ToLower(chunk.Content), strings.ToLower(kw)) {
				matchCount++
			}
		}

		results = append(results, &SearchResult{
			Chunk:     chunk,
			TextScore: float64(matchCount) / float64(len(keywords)),
		})
	}

	return results, rows.Err()
}

// getChunkByID 根据 ID 获取完整的 chunk 信息
func (s *SQLiteStore) getChunkByID(ctx context.Context, chunkID string) *Chunk {
	chunk := &Chunk{}
	var embBlob []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, doc_id, content, chunk_index, embedding, created_at FROM kb_chunks WHERE id = ?`,
		chunkID,
	).Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.Index, &embBlob, &chunk.CreatedAt)
	if err != nil {
		return nil
	}
	if len(embBlob) > 0 {
		chunk.Embedding = decodeFloat32Slice(embBlob)
	}
	return chunk
}

// --- 向量序列化/反序列化 ---

// encodeFloat32Slice 将 float32 切片编码为字节序列（小端序）
func encodeFloat32Slice(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeFloat32Slice 将字节序列解码为 float32 切片
func decodeFloat32Slice(buf []byte) []float32 {
	if len(buf)%4 != 0 {
		return nil
	}
	v := make([]float32, len(buf)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return v
}

// --- 分词 ---

// tokenize 简单分词
//
// 按空格和常见标点分割，过滤短词（< 2 字符）。
// 用于 FTS5 查询和降级搜索。
func tokenize(text string) []string {
	replacer := strings.NewReplacer(
		"，", " ", "。", " ", "？", " ", "！", " ",
		",", " ", ".", " ", "?", " ", "!", " ",
		"、", " ", "：", " ", "；", " ",
		"\"", " ", "'", " ", "(", " ", ")", " ",
	)
	text = replacer.Replace(text)

	words := strings.Fields(text)
	var result []string
	for _, w := range words {
		w = strings.TrimSpace(w)
		if len(w) >= 2 {
			result = append(result, w)
		}
	}
	return result
}
