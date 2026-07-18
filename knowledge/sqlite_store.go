package knowledge

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
	"github.com/hexagon-codes/toolkit/util/logger"
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
	_ SearchableCorpus   = (*SQLiteStore)(nil)
)

// NewSQLiteStore 创建 SQLite 知识库存储
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
}

// buildFilterClause 把 Filter 的「源 / 源类型」维度编译为下推到 SQL 的 AND 片段
// （不含前导 AND）+ 占位参数。docAlias 是 kb_documents 在 SQL 中的别名。
// 无源/类型约束时返回 ("", nil)，调用方据此走全量快路径。
//
// 维度内多值用 IN(...) 实现 OR，维度间用 AND 串联。
// 日期维度刻意不在此下推（见 Filter.matchesDate 注释——modernc 的 RFC3339 文本存储
// 在跨时区时字符串比较不可靠），由调用方在 Go 层对真实 time.Time 比较。
func buildFilterClause(f Filter, docAlias string) (string, []any) {
	f = f.normalize()
	var clauses []string
	var args []any
	if len(f.Sources) > 0 {
		ph, a := inPlaceholders(f.Sources)
		clauses = append(clauses, docAlias+".source IN ("+ph+")")
		args = append(args, a...)
	}
	if len(f.SourceTypes) > 0 {
		ph, a := inPlaceholders(f.SourceTypes)
		clauses = append(clauses, docAlias+".source_type IN ("+ph+")")
		args = append(args, a...)
	}
	return strings.Join(clauses, " AND "), args
}

// inPlaceholders 为字符串多值生成 "?,?,?" 占位串与对应参数。
func inPlaceholders(vals []string) (string, []any) {
	ph := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		ph[i] = "?"
		args[i] = v
	}
	return strings.Join(ph, ","), args
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
		// Backs GetBySourceTitle's upsert-hit lookup (review M3). Non-unique on
		// purpose: the production UNIQUE(source,title) constraint lives in
		// storage/migrate (it dedupes first); a plain index here is safe to add
		// to any existing store without a dedup pass.
		`CREATE INDEX IF NOT EXISTS idx_kb_documents_source_title ON kb_documents(source, title)`,
		// v0.4.0 E3：复合 UNIQUE 防同 doc 同位置 chunk 累积（与 storage/migrate v4 一致）
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_chunks_doc_index ON kb_chunks(doc_id, chunk_index)`,

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

	// 启动时清理孤儿 FTS5 记录（chunk 已删除但 FTS5 索引残留）
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM kb_chunks_fts WHERE chunk_id NOT IN (SELECT id FROM kb_chunks)`,
	); err != nil {
		logger.Error("[knowledge] 清理孤儿 FTS5 记录失败", "error", err)
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

		// v0.4.0 E3：kb_chunks (doc_id, chunk_index) UNIQUE 收口；
		// ingestion retry / 重新分块时同位置覆盖而非累积（防 v0.3.12 故障复发）
		_, err = tx.ExecContext(ctx,
			`INSERT INTO kb_chunks (id, doc_id, content, chunk_index, embedding, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(doc_id, chunk_index) DO UPDATE SET
			   id = excluded.id,
			   content = excluded.content,
			   embedding = excluded.embedding,
			   created_at = excluded.created_at`,
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

// HasSearchableDocuments uses an indexed existence query instead of loading
// document metadata on every chat turn.
func (s *SQLiteStore) HasSearchableDocuments(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kb_chunks LIMIT 1)`).Scan(&exists)
	return exists, err
}

// GetBySourceTitle 按 (source, title) 查询单个文档（不含正文）。
// 命中 idx_kb_documents_unique(source, title) 索引，避免 List 全表扫描（review M3）。
// 不存在返回 (nil, nil)，让调用方区分"未命中"与"查询出错"。
func (s *SQLiteStore) GetBySourceTitle(ctx context.Context, source, title string) (*Document, error) {
	if title == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, title, source, chunk_count, created_at, updated_at, status, error_message, source_type
		 FROM kb_documents WHERE source = ? AND title = ? LIMIT 1`,
		source, title,
	)
	doc := &Document{}
	if err := row.Scan(&doc.ID, &doc.Title, &doc.Source, &doc.ChunkCount, &doc.CreatedAt, &doc.UpdatedAt, &doc.Status, &doc.ErrorMessage, &doc.SourceType); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return doc, nil
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
// VectorSearch 做全量精确余弦扫描，不再用 LIMIT 静默截断。
//
// 纯 Go 驱动（modernc.org/sqlite，无 CGO）下，对个人/桌面知识库做全量精确扫描
// 即业界最佳实践：向量数 < ~10万 时暴力精确检索 recall=1.0，反而比 ANN 更准
// （ANN 是近似召回）。本实现逐行解码 embedding blob、算完相似度后立即丢弃，
// 只保留 {id, sim}（内存 O(n) 仅数 MB），不存在 OOM 风险——
// 旧 Fix 6 的 maxVectorScanRows=10000 上限会在大库时悄悄丢召回，已移除。
//
// 当扫描规模超过 vectorScanWarnThreshold 时打 WARN（不静默），
// 提示后续引入 pure-Go ANN（如 HNSW）以维持延迟。
const vectorScanWarnThreshold = 100000

func (s *SQLiteStore) VectorSearch(ctx context.Context, queryVec []float32, topK int, filter Filter) ([]*SearchResult, error) {
	// 元数据过滤在「全量打分前」生效，确保不被 topK 截断吞掉：
	//   - 源/源类型：下推 SQL（JOIN kb_documents + IN，纯字符串相等，跨时区无歧义）；
	//   - 日期：取 d.created_at 在 Go 层按真实 time.Time 比较（见 Filter.matchesDate）。
	// 无任何过滤时走原快路径（不 JOIN，零回归）。
	clause, fargs := buildFilterClause(filter, "d")
	needDate := filter.hasDateBound()
	var query string
	var args []any
	switch {
	case clause == "" && !needDate:
		query = `SELECT c.id, c.doc_id, c.chunk_index, c.embedding FROM kb_chunks c WHERE c.embedding IS NOT NULL`
	default:
		sel := "c.id, c.doc_id, c.chunk_index, c.embedding"
		if needDate {
			sel += ", d.created_at"
		}
		query = "SELECT " + sel + " FROM kb_chunks c JOIN kb_documents d ON d.id = c.doc_id WHERE c.embedding IS NOT NULL"
		if clause != "" {
			query += " AND " + clause
			args = fargs
		}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
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
	scanned := 0

	for rows.Next() {
		// 每 1000 行检查 context，避免长时间扫描不可取消
		if scanned%1000 == 0 {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		scanned++

		var s scored
		var embBlob []byte
		var createdAt time.Time
		if needDate {
			err = rows.Scan(&s.id, &s.docID, &s.chunkIndex, &embBlob, &createdAt)
		} else {
			err = rows.Scan(&s.id, &s.docID, &s.chunkIndex, &embBlob)
		}
		if err != nil {
			logger.Error("[knowledge] VectorSearch scan 失败", "error", err)
			continue
		}
		// 日期过滤在打分/排序前生效（Go 层按真实时刻比较）。
		if needDate && !filter.matchesDate(createdAt) {
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
	if scanned > vectorScanWarnThreshold {
		logger.Warn("[knowledge] 向量全表精确扫描规模较大，建议后续引入 ANN 索引以维持延迟",
			"scanned", scanned, "threshold", vectorScanWarnThreshold)
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
		logger.Error("[knowledge] VectorSearch 加载 chunks 失败", "error", chunkErr)
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
func (s *SQLiteStore) TextSearch(ctx context.Context, query string, topK int, filter Filter) ([]*SearchResult, error) {
	// 构建 FTS5 查询：将查询分词后用 OR 连接
	keywords := splitter.SearchTokenize(query)
	if len(keywords) == 0 {
		return nil, nil
	}

	// FTS5 查询语法：用 OR 连接多个关键词
	ftsQuery := strings.Join(keywords, " OR ")

	// 元数据过滤生效于 LIMIT/截断之前：源/源类型下推 SQL（JOIN kb_documents + IN）；
	// 日期取 d.created_at 在 Go 层按真实时刻比较。带日期过滤时不能用 SQL LIMIT（否则日期
	// 匹配项可能因 bm25 排序落在 LIMIT 之外被漏召回），改为按 score 顺序扫描、Go 过滤后取 topK。
	clause, fargs := buildFilterClause(filter, "d")
	needDate := filter.hasDateBound()
	needJoin := clause != "" || needDate

	sel := "f.chunk_id, f.content, bm25(kb_chunks_fts) as score"
	if needDate {
		sel += ", d.created_at"
	}
	from := "kb_chunks_fts f"
	if needJoin {
		from = `kb_chunks_fts f
			 JOIN kb_chunks c ON c.id = f.chunk_id
			 JOIN kb_documents d ON d.id = c.doc_id`
	}
	where := "kb_chunks_fts MATCH ?"
	args := []any{ftsQuery}
	if clause != "" {
		where += " AND " + clause
		args = append(args, fargs...)
	}
	sqlQuery := "SELECT " + sel + " FROM " + from + " WHERE " + where + " ORDER BY score"
	if !needDate {
		sqlQuery += " LIMIT ?"
		args = append(args, topK)
	}

	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		// FTS5 查询失败（可能是特殊字符），降级到 LIKE 搜索
		return s.fallbackTextSearch(ctx, keywords, topK, filter)
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
		var createdAt time.Time
		if needDate {
			if err := rows.Scan(&r.chunkID, &r.content, &r.score, &createdAt); err != nil {
				return nil, err
			}
			// 日期过滤在 Go 层（rows 已按 score 排序，过滤后取前 topK 即最相关者）。
			if !filter.matchesDate(createdAt) {
				continue
			}
		} else if err := rows.Scan(&r.chunkID, &r.content, &r.score); err != nil {
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
		if needDate && len(raw) >= topK {
			break // 已按 score 收满 topK 个日期匹配项
		}
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
			logger.Error("[knowledge] TextSearch 加载 chunks 失败", "error", chunkLoadErr)
		}
	}

	// 归一化 BM25 分数到 0-1
	scoreRange := maxScore - minScore
	for _, r := range raw {
		// Fix 7: 当 scoreRange == 0（所有结果分数相同，包括只有 1 个结果），
		// 设为 1.0（都是最佳匹配），而非 0.5 导致完美匹配被低估。
		normalizedScore := 1.0
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

	// FTS5 返回空时降级到 LIKE 搜索（解决中文 tokenizer 不匹配的问题）
	if len(results) == 0 {
		return s.fallbackTextSearch(ctx, keywords, topK, filter)
	}
	return results, nil
}

// fallbackTextSearch FTS5 不可用或结果为空时的降级搜索（LIKE 匹配）。
// 与主路径一致地把元数据过滤下推到 LIMIT 之前（JOIN kb_documents）。
func (s *SQLiteStore) fallbackTextSearch(ctx context.Context, keywords []string, topK int, filter Filter) ([]*SearchResult, error) {
	var query strings.Builder
	var args []any

	clause, fargs := buildFilterClause(filter, "d")
	needDate := filter.hasDateBound()
	needJoin := clause != "" || needDate

	// Fix 15: 不查询 embedding 列，文本降级搜索无需加载向量 BLOB。
	// 统一以别名 c 引用 kb_chunks，便于在有过滤时 JOIN kb_documents。
	// chunk.CreatedAt 取 c.created_at（片段时间，供时间衰减/展示）；日期过滤用 d.created_at
	// （文档时间，与主路径语义一致），故 needDate 时额外多取一列。
	if needJoin {
		query.WriteString("SELECT c.id, c.doc_id, c.content, c.chunk_index, c.created_at")
		if needDate {
			query.WriteString(", d.created_at")
		}
		query.WriteString(" FROM kb_chunks c JOIN kb_documents d ON d.id = c.doc_id WHERE (")
	} else {
		query.WriteString("SELECT c.id, c.doc_id, c.content, c.chunk_index, c.created_at FROM kb_chunks c WHERE (")
	}
	for i, kw := range keywords {
		if i > 0 {
			query.WriteString(" OR ")
		}
		query.WriteString("c.content LIKE ? ESCAPE '\\'")
		args = append(args, "%"+sqliteutil.EscapeLike(kw)+"%")
	}
	query.WriteString(")")
	if clause != "" {
		query.WriteString(" AND ")
		query.WriteString(clause)
		args = append(args, fargs...)
	}
	// 带日期过滤时不能用 SQL LIMIT（日期在 Go 层裁，匹配项可能排在 LIMIT 之外）。
	if !needDate {
		query.WriteString(" LIMIT ?")
		args = append(args, topK)
	}

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*SearchResult
	for rows.Next() {
		chunk := &Chunk{}
		var docCreatedAt time.Time
		// Fix 15: Scan 与 SELECT 对齐（已移除 embedding 列）
		if needDate {
			if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.Index, &chunk.CreatedAt, &docCreatedAt); err != nil {
				logger.Error("[knowledge] fallbackTextSearch scan 失败", "error", err)
				continue
			}
			if !filter.matchesDate(docCreatedAt) {
				continue
			}
		} else if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.Index, &chunk.CreatedAt); err != nil {
			logger.Error("[knowledge] fallbackTextSearch scan 失败", "error", err)
			continue
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
		if needDate && len(results) >= topK {
			break
		}
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
	query.WriteString(`SELECT c.id, c.doc_id, d.title, d.source, d.source_type, d.chunk_count, c.content, c.chunk_index, c.embedding, c.created_at
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
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.DocTitle, &chunk.Source, &chunk.SourceType, &chunk.ChunkCount, &chunk.Content, &chunk.Index, &embBlob, &chunk.CreatedAt); err != nil {
			logger.Error("[knowledge] scan chunk", "id", chunk.ID, "error", err)
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

// 搜索分词已迁移到 hexagon/rag/splitter.SearchTokenize
// 提供 CJK bigram + 原词保留 + 去重，供 FTS5 查询和 LIKE 降级搜索使用。
