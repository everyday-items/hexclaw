package knowledge

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
)

// SQLiteStore SQLite 知识库存储
//
// 同时实现 DocumentRepository（写路径）和 ChunkSearcher（读路径）。
// 两个接口共享同一个 *sql.DB 连接，事务由 Repository 方法内部管理。
//
// 存储结构：
//   - kb_documents: 文档元信息
//   - kb_chunks: 文档片段 + 向量嵌入（BLOB）
//   - kb_chunks_fts: FTS5 全文索引虚拟表
//
// 向量存储采用 float32 序列化为 BLOB 的方式，
// 余弦相似度在 Go 层计算。对于个人知识库规模（< 10万 chunk），
// 这种方案性能完全够用，且避免了 CGO/sqlite-vec 的编译依赖。
type SQLiteStore struct {
	db *sql.DB
}

// 编译期接口满足性检查
var (
	_ DocumentRepository = (*SQLiteStore)(nil)
	_ ChunkSearcher      = (*SQLiteStore)(nil)
)

// NewSQLiteStore 创建 SQLite 知识库存储
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
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
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			status TEXT NOT NULL DEFAULT 'indexed',
			error_message TEXT NOT NULL DEFAULT '',
			source_type TEXT NOT NULL DEFAULT 'manual'
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
	migrations := []string{
		`ALTER TABLE kb_documents ADD COLUMN updated_at DATETIME DEFAULT CURRENT_TIMESTAMP`,
		`ALTER TABLE kb_documents ADD COLUMN status TEXT NOT NULL DEFAULT 'indexed'`,
		`ALTER TABLE kb_documents ADD COLUMN error_message TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE kb_documents ADD COLUMN source_type TEXT NOT NULL DEFAULT 'manual'`,
	}
	for _, stmt := range migrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("迁移知识库表失败: %w", err)
		}
	}
	return nil
}

// Add 添加文档及其 chunk（含向量和 FTS5 索引）
func (s *SQLiteStore) Add(ctx context.Context, doc *Document, chunks []*Chunk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 插入文档
	_, err = tx.ExecContext(ctx,
		`INSERT INTO kb_documents (id, title, content, source, chunk_count, created_at, updated_at, status, error_message, source_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.ID, doc.Title, doc.Content, doc.Source, doc.ChunkCount, doc.CreatedAt, doc.UpdatedAt, doc.Status, doc.ErrorMessage, doc.SourceType,
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
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_chunks_fts (content, chunk_id) VALUES (?, ?)`,
			chunk.Content, chunk.ID,
		); err != nil {
			return fmt.Errorf("fts5 索引插入失败: %w", err)
		}
	}

	return tx.Commit()
}

// Replace 使用同一文档 ID 重建索引
func (s *SQLiteStore) Replace(ctx context.Context, doc *Document, chunks []*Chunk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_chunks_fts WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id = ?)`,
		doc.ID,
	); err != nil {
		return fmt.Errorf("fts5 索引删除失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks WHERE doc_id = ?`, doc.ID); err != nil {
		return fmt.Errorf("删除旧 chunk 失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE kb_documents
		 SET title = ?, content = ?, source = ?, chunk_count = ?, updated_at = ?, status = ?, error_message = ?, source_type = ?
		 WHERE id = ?`,
		doc.Title, doc.Content, doc.Source, doc.ChunkCount, doc.UpdatedAt, doc.Status, doc.ErrorMessage, doc.SourceType, doc.ID,
	); err != nil {
		return fmt.Errorf("更新文档失败: %w", err)
	}

	for _, chunk := range chunks {
		var embBlob []byte
		if len(chunk.Embedding) > 0 {
			embBlob = encodeFloat32Slice(chunk.Embedding)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_chunks (id, doc_id, content, chunk_index, embedding, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			chunk.ID, chunk.DocID, chunk.Content, chunk.Index, embBlob, chunk.CreatedAt,
		); err != nil {
			return fmt.Errorf("插入重建 chunk 失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_chunks_fts (content, chunk_id) VALUES (?, ?)`,
			chunk.Content, chunk.ID,
		); err != nil {
			return fmt.Errorf("重建 fts5 索引失败: %w", err)
		}
	}

	return tx.Commit()
}

// Delete 删除文档及其 chunk + FTS5 索引
func (s *SQLiteStore) Delete(ctx context.Context, docID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 删除 FTS5 索引中的对应记录
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_chunks_fts WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id = ?)`,
		docID,
	); err != nil {
		return fmt.Errorf("fts5 索引删除失败: %w", err)
	}

	// 删除 chunk 和文档
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks WHERE doc_id = ?`, docID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_documents WHERE id = ?`, docID); err != nil {
		return err
	}

	return tx.Commit()
}

// List 列出所有文档
func (s *SQLiteStore) List(ctx context.Context) ([]*Document, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, source, chunk_count, created_at, updated_at, status, error_message, source_type
		 FROM kb_documents ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []*Document
	for rows.Next() {
		doc := &Document{}
		if err := rows.Scan(&doc.ID, &doc.Title, &doc.Source, &doc.ChunkCount, &doc.CreatedAt, &doc.UpdatedAt, &doc.Status, &doc.ErrorMessage, &doc.SourceType); err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

// Get 获取单个文档详情
func (s *SQLiteStore) Get(ctx context.Context, docID string) (*Document, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, title, content, source, chunk_count, created_at, updated_at, status, error_message, source_type
		 FROM kb_documents WHERE id = ?`,
		docID,
	)

	doc := &Document{}
	if err := row.Scan(&doc.ID, &doc.Title, &doc.Content, &doc.Source, &doc.ChunkCount, &doc.CreatedAt, &doc.UpdatedAt, &doc.Status, &doc.ErrorMessage, &doc.SourceType); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("文档不存在")
		}
		return nil, err
	}
	return doc, nil
}

// VectorSearch 向量搜索
//
// 加载 chunk 的向量，在 Go 层计算余弦相似度，
// 返回相似度最高的 topK 个结果。
//
// 限制最多扫描 maxVectorScanRows 行，防止知识库过大时 OOM。
// 对于个人知识库（通常 < 10万 chunk），这种全扫描方式
// 性能完全够用（10万个 1536 维向量约需 ~100ms）。
const maxVectorScanRows = 100000

func (s *SQLiteStore) VectorSearch(ctx context.Context, queryVec []float32, topK int) ([]*SearchResult, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, doc_id, chunk_index, embedding FROM kb_chunks WHERE embedding IS NOT NULL LIMIT ?`,
		maxVectorScanRows,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 计算所有 chunk 与查询向量的余弦相似度
	type scored struct {
		id         string
		docID      string
		chunkIndex int
		sim        float64
	}
	var all []scored

	for rows.Next() {
		// 每 1000 行检查 context，避免长时间扫描不可取消
		if len(all)%1000 == 0 {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}

		var s scored
		var embBlob []byte
		if err := rows.Scan(&s.id, &s.docID, &s.chunkIndex, &embBlob); err != nil {
			log.Printf("[knowledge] VectorSearch scan 失败: %v", err)
			continue
		}

		if len(embBlob) > 0 {
			embedding := decodeFloat32Slice(embBlob)
			sim := cosineSimilarity(queryVec, embedding)
			s.sim = (sim + 1) / 2 // 归一化到 0-1
			all = append(all, s)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// O(n log n) 排序替代 O(n²) 插入排序
	sort.Slice(all, func(i, j int) bool {
		return all[i].sim > all[j].sim
	})

	// 取 topK
	if len(all) > topK {
		all = all[:topK]
	}

	// 批量加载 topK 条完整 chunk（而不是全部）
	ids := make([]string, len(all))
	for i, s := range all {
		ids[i] = s.id
	}
	chunkMap, chunkErr := s.getChunksByIDs(ctx, ids)
	if chunkErr != nil {
		log.Printf("[knowledge] VectorSearch 加载 chunks 失败: %v", chunkErr)
	}

	results := make([]*SearchResult, len(all))
	for i, s := range all {
		chunk := chunkMap[s.id]
		if chunk == nil {
			chunk = &Chunk{ID: s.id, DocID: s.docID, Index: s.chunkIndex}
		}
		results[i] = &SearchResult{
			Chunk:       chunk,
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

	// 批量获取 chunk 完整信息（避免 N+1 查询）
	chunkMap := make(map[string]*Chunk, len(raw))
	if len(raw) > 0 {
		ids := make([]string, len(raw))
		for i, r := range raw {
			ids[i] = r.chunkID
		}
		var chunkLoadErr error
		chunkMap, chunkLoadErr = s.getChunksByIDs(ctx, ids)
		if chunkLoadErr != nil {
			log.Printf("[knowledge] TextSearch 加载 chunks 失败: %v", chunkLoadErr)
		}
	}

	// 归一化 BM25 分数到 0-1
	scoreRange := maxScore - minScore
	for _, r := range raw {
		normalizedScore := 0.5 // 只有一个结果时
		if scoreRange > 0 {
			normalizedScore = (r.score - minScore) / scoreRange
		}

		chunk := chunkMap[r.chunkID]
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
	var query strings.Builder
	var args []any

	query.WriteString("SELECT id, doc_id, content, chunk_index, embedding, created_at FROM kb_chunks WHERE ")
	for i, kw := range keywords {
		if i > 0 {
			query.WriteString(" OR ")
		}
		query.WriteString("content LIKE ? ESCAPE '\\'")
		args = append(args, "%"+sqliteutil.EscapeLike(kw)+"%")
	}
	query.WriteString(" LIMIT ?")
	args = append(args, topK)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*SearchResult
	for rows.Next() {
		chunk := &Chunk{}
		var embBlob []byte
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.Index, &embBlob, &chunk.CreatedAt); err != nil {
			log.Printf("[knowledge] fallbackTextSearch scan 失败: %v", err)
			continue
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

// getChunksByIDs 批量获取 chunk 信息（避免 N+1 查询）
func (s *SQLiteStore) getChunksByIDs(ctx context.Context, ids []string) (map[string]*Chunk, error) {
	result := make(map[string]*Chunk, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	var query strings.Builder
	query.WriteString(`SELECT c.id, c.doc_id, d.title, d.source, d.chunk_count, c.content, c.chunk_index, c.embedding, c.created_at
		 FROM kb_chunks c
		 JOIN kb_documents d ON d.id = c.doc_id
		 WHERE c.id IN (`)
	query.WriteString(strings.Join(placeholders, ","))
	query.WriteString(")")

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("批量获取 chunks 失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		chunk := &Chunk{}
		var embBlob []byte
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.DocTitle, &chunk.Source, &chunk.ChunkCount, &chunk.Content, &chunk.Index, &embBlob, &chunk.CreatedAt); err != nil {
			log.Printf("[knowledge] scan chunk %s 失败: %v", chunk.ID, err)
			continue
		}
		if len(embBlob) > 0 {
			chunk.Embedding = decodeFloat32Slice(embBlob)
		}
		result[chunk.ID] = chunk
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("遍历 chunks 行时出错: %w", err)
	}
	return result, nil
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
