package knowledge

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
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
//   - kb_chunks_fts_v2: 预分词的 CJK bigram FTS5 索引
//
// 向量存储采用 float32 序列化为 BLOB 的方式，
// 余弦相似度在 Go 层计算。对于个人知识库规模（< 10万 chunk），
// 这种方案性能完全够用，且避免了 CGO/sqlite-vec 的编译依赖。
type SQLiteStore struct {
	db                *sql.DB
	semanticMutations *sqliteSemanticMutationScope
	// SQLite permits one physical writer. Serialize this store's multi-table
	// document transactions; RetryOnBusy remains the bounded fallback for
	// writers using another store or process.
	writeMu sync.Mutex
}

const cjkFTSIndexVersion = 2

type SQLiteStoreOption func(*SQLiteStore)

// WithSQLiteSemanticMutations binds document writes to one explicit
// owner/corpus. The hook runs inside the same SQLite transaction as the legacy
// document/chunk/FTS write, so a control-plane failure rolls everything back.
func WithSQLiteSemanticMutations(ownerID, corpusID string) SQLiteStoreOption {
	return func(store *SQLiteStore) {
		store.semanticMutations = &sqliteSemanticMutationScope{ownerID: ownerID, corpusID: corpusID}
	}
}

// 编译期接口满足性检查
var (
	_ DocumentRepository = (*SQLiteStore)(nil)
	_ ChunkSearcher      = (*SQLiteStore)(nil)
	_ SearchableCorpus   = (*SQLiteStore)(nil)
)

// NewSQLiteStore 创建 SQLite 知识库存储
func NewSQLiteStore(db *sql.DB, options ...SQLiteStoreOption) *SQLiteStore {
	store := &SQLiteStore{db: db}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store
}

// semanticScopeClause returns a fail-closed owner+corpus predicate for reads.
// The public API uses the stable corpus alias ("default"), while documents
// persist the immutable internal corpus UID.
func (s *SQLiteStore) semanticScopeClause(documentAlias string) (string, []any) {
	if s.semanticMutations == nil {
		return "", nil
	}
	return documentAlias + `.corpus_uid=(
		SELECT c.corpus_uid FROM kb_semantic_corpora c
		WHERE c.owner_id=? AND c.corpus_alias=?
	)`, []any{s.semanticMutations.ownerID, s.semanticMutations.corpusID}
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

// buildRevisionFilterClause extends the shared document metadata clause with
// an exact document-generation pair predicate. The caller must pass the
// semantic binding alias so the predicate is applied by SQL before LIMIT/topK.
func buildRevisionFilterClause(
	f Filter,
	docAlias, bindingAlias, chunkAlias string,
) (string, []any) {
	f = f.normalize()
	clause, args := buildFilterClause(f, docAlias)
	var clauses []string
	if clause != "" {
		clauses = append(clauses, clause)
	}
	if len(f.DocumentGenerations) > 0 {
		pairs := make([]string, 0, len(f.DocumentGenerations))
		for _, ref := range f.DocumentGenerations {
			pairs = append(pairs,
				"("+docAlias+".id=? AND "+bindingAlias+".content_generation=?)")
			args = append(args, ref.DocumentID, ref.DocumentGeneration)
		}
		clauses = append(clauses, "("+strings.Join(pairs, " OR ")+")")
	}
	if len(f.ChunkIDs) > 0 {
		placeholders, chunkArgs := inPlaceholders(f.ChunkIDs)
		clauses = append(clauses, chunkAlias+".id IN ("+placeholders+")")
		args = append(args, chunkArgs...)
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

func nullablePositiveInt(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

func nullableOffset(start, end int64, wantEnd bool) any {
	if start < 0 || end <= start {
		return nil
	}
	if wantEnd {
		return end
	}
	return start
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
			deleted INTEGER NOT NULL DEFAULT 0,
			error_message TEXT NOT NULL DEFAULT '',
			source_type TEXT NOT NULL DEFAULT 'manual',
			corpus_uid TEXT
		)`,

		// Chunk 表（含向量嵌入 BLOB）
		`CREATE TABLE IF NOT EXISTS kb_chunks (
			id TEXT PRIMARY KEY,
			doc_id TEXT NOT NULL,
			content TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			embedding BLOB,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			page_start INTEGER,
			page_end INTEGER,
			source_digest TEXT NOT NULL DEFAULT '',
			source_offset_start INTEGER,
			source_offset_end INTEGER,
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
		// v2 separates immutable raw content from retrieval tokens. SQLite's
		// built-in unicode61 tokenizer treats a contiguous Chinese sentence as
		// one token; indexing deterministic CJK bigrams makes Chinese BM25 a real
		// FTS lane instead of an accidental LIKE full scan.
		`CREATE VIRTUAL TABLE IF NOT EXISTS kb_chunks_fts_v2 USING fts5(
			tokens,
			chunk_id UNINDEXED
		)`,
		`CREATE TABLE IF NOT EXISTS kb_search_index_metadata (
			index_name TEXT PRIMARY KEY,
			version INTEGER NOT NULL,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TRIGGER IF NOT EXISTS kb_chunks_cjk_fts_v2_dirty_insert
			AFTER INSERT ON kb_chunks BEGIN
				DELETE FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts';
			END`,
		`CREATE TRIGGER IF NOT EXISTS kb_chunks_cjk_fts_v2_dirty_update
			AFTER UPDATE OF id, doc_id, content ON kb_chunks
			WHEN OLD.id IS NOT NEW.id OR OLD.doc_id IS NOT NEW.doc_id OR OLD.content IS NOT NEW.content
			BEGIN
				DELETE FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts';
			END`,
		`CREATE TRIGGER IF NOT EXISTS kb_chunks_cjk_fts_v2_dirty_delete
			AFTER DELETE ON kb_chunks BEGIN
				DELETE FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts';
			END`,
		`CREATE TRIGGER IF NOT EXISTS kb_documents_cjk_fts_v2_dirty_lifecycle
			AFTER UPDATE OF deleted ON kb_documents
			WHEN OLD.deleted IS NOT NEW.deleted
			BEGIN
				DELETE FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts';
			END`,
	}

	for _, q := range queries {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("初始化知识库表失败: %w", err)
		}
	}
	migrations := []string{
		`ALTER TABLE kb_documents ADD COLUMN updated_at DATETIME DEFAULT CURRENT_TIMESTAMP`,
		`ALTER TABLE kb_documents ADD COLUMN status TEXT NOT NULL DEFAULT 'indexed'`,
		`ALTER TABLE kb_documents ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE kb_documents ADD COLUMN error_message TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE kb_documents ADD COLUMN source_type TEXT NOT NULL DEFAULT 'manual'`,
		`ALTER TABLE kb_documents ADD COLUMN corpus_uid TEXT`,
		`ALTER TABLE kb_chunks ADD COLUMN page_start INTEGER`,
		`ALTER TABLE kb_chunks ADD COLUMN page_end INTEGER`,
		`ALTER TABLE kb_chunks ADD COLUMN source_digest TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE kb_chunks ADD COLUMN source_offset_start INTEGER`,
		`ALTER TABLE kb_chunks ADD COLUMN source_offset_end INTEGER`,
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
	if err := s.ensureCJKFTSIndex(ctx); err != nil {
		return fmt.Errorf("初始化 CJK FTS5 v2 索引失败: %w", err)
	}

	return nil
}

// ensureCJKFTSIndex upgrades an existing corpus in one bounded-memory SQLite
// transaction. The version marker is committed only after every active chunk
// has been indexed, so a crash leaves the old marker and the next startup
// deterministically rebuilds instead of serving a partially published index.
func (s *SQLiteStore) ensureCJKFTSIndex(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	versionErr := tx.QueryRowContext(ctx,
		`SELECT version FROM kb_search_index_metadata WHERE index_name='chunks_cjk_fts'`,
	).Scan(&version)
	if versionErr != nil && versionErr != sql.ErrNoRows {
		return versionErr
	}
	var indexedCount, distinctIndexedCount, activeCount int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*),COUNT(DISTINCT chunk_id) FROM kb_chunks_fts_v2`,
	).Scan(&indexedCount, &distinctIndexedCount); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunks c
		JOIN kb_documents d ON d.id=c.doc_id
		WHERE d.deleted=0`).Scan(&activeCount); err != nil {
		return err
	}
	if version == cjkFTSIndexVersion &&
		indexedCount == activeCount && distinctIndexedCount == activeCount {
		var identityDrift int
		if err := tx.QueryRowContext(ctx, `
			SELECT CASE WHEN EXISTS (
				SELECT 1 FROM kb_chunks_fts_v2 f
				LEFT JOIN kb_chunks c ON c.id=f.chunk_id
				LEFT JOIN kb_documents d ON d.id=c.doc_id
				WHERE c.id IS NULL OR d.id IS NULL OR d.deleted<>0
				LIMIT 1
			)
			THEN 1 ELSE 0 END`).Scan(&identityDrift); err != nil {
			return err
		}
		// The FTS ids form a distinct subset of the active chunk ids. Equal set
		// cardinality therefore proves exact membership without an O(N²) reverse
		// join against the UNINDEXED FTS column.
		if identityDrift == 0 {
			return tx.Commit()
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks_fts_v2`); err != nil {
		return err
	}
	const batchSize int64 = 256
	var afterRowID int64
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT c.rowid,c.id,c.content
			FROM kb_chunks c
			JOIN kb_documents d ON d.id=c.doc_id
			WHERE d.deleted=0 AND c.rowid>?
			ORDER BY c.rowid
			LIMIT ?`, afterRowID, batchSize)
		if err != nil {
			return err
		}
		type indexRecord struct {
			rowID   int64
			chunkID string
			content string
		}
		batch := make([]indexRecord, 0, batchSize)
		for rows.Next() {
			var record indexRecord
			if err := rows.Scan(&record.rowID, &record.chunkID, &record.content); err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, record)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, record := range batch {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO kb_chunks_fts_v2(tokens,chunk_id) VALUES(?,?)`,
				cjkFTSIndexText(record.content), record.chunkID,
			); err != nil {
				return err
			}
		}
		afterRowID = batch[len(batch)-1].rowID
	}
	if err := markCJKFTSCurrentTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func markCJKFTSCurrentTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO kb_search_index_metadata(index_name,version,updated_at)
		VALUES('chunks_cjk_fts',?,CURRENT_TIMESTAMP)
		ON CONFLICT(index_name) DO UPDATE SET
		  version=excluded.version,updated_at=excluded.updated_at`, cjkFTSIndexVersion)
	if err != nil {
		return err
	}
	return nil
}

func cjkFTSProjectionCurrentTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var version int
	err := tx.QueryRowContext(ctx, `SELECT version FROM kb_search_index_metadata
		WHERE index_name='chunks_cjk_fts'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return version == cjkFTSIndexVersion, nil
}

func restoreCJKFTSCurrentTx(ctx context.Context, tx *sql.Tx, wasCurrent bool) error {
	if !wasCurrent {
		return nil
	}
	return markCJKFTSCurrentTx(ctx, tx)
}

func cjkFTSIndexText(content string) string {
	return strings.Join(splitter.SearchTokenize(content), " ")
}

func cjkFTSQuery(tokens []string) string {
	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == "" {
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(token, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// Add 添加文档及其 chunk（含向量和 FTS5 索引）
func (s *SQLiteStore) Add(ctx context.Context, doc *Document, chunks []*Chunk) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return sqliteutil.RetryOnBusy(ctx, func() error {
		return s.addOnce(ctx, doc, chunks)
	})
}

func (s *SQLiteStore) addOnce(ctx context.Context, doc *Document, chunks []*Chunk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	projectionWasCurrent, err := cjkFTSProjectionCurrentTx(ctx, tx)
	if err != nil {
		return err
	}

	// A scoped document owns its corpus before any binding/job row is created;
	// this makes the database uniqueness boundary authoritative even between
	// concurrent writers. Legacy/unscoped stores retain a NULL corpus UID.
	if s.semanticMutations != nil {
		state, scopeErr := loadSemanticPolicyState(ctx, tx,
			s.semanticMutations.ownerID, s.semanticMutations.corpusID)
		if scopeErr != nil {
			return scopeErr
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO kb_documents (id, title, content, source, chunk_count, created_at, updated_at, status, error_message, source_type, corpus_uid)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			doc.ID, doc.Title, doc.Content, doc.Source, doc.ChunkCount, doc.CreatedAt,
			doc.UpdatedAt, doc.Status, doc.ErrorMessage, doc.SourceType, state.corpusUID,
		)
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO kb_documents (id, title, content, source, chunk_count, created_at, updated_at, status, error_message, source_type)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			doc.ID, doc.Title, doc.Content, doc.Source, doc.ChunkCount, doc.CreatedAt,
			doc.UpdatedAt, doc.Status, doc.ErrorMessage, doc.SourceType,
		)
	}
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
			`INSERT INTO kb_chunks (id, doc_id, content, chunk_index, embedding, created_at,
			 page_start,page_end,source_digest,source_offset_start,source_offset_end)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(doc_id, chunk_index) DO UPDATE SET
			   id = excluded.id,
			   content = excluded.content,
			   embedding = excluded.embedding,
			   created_at = excluded.created_at,
			   page_start = excluded.page_start,
			   page_end = excluded.page_end,
			   source_digest = excluded.source_digest,
			   source_offset_start = excluded.source_offset_start,
			   source_offset_end = excluded.source_offset_end`,
			chunk.ID, chunk.DocID, chunk.Content, chunk.Index, embBlob, chunk.CreatedAt,
			nullablePositiveInt(chunk.PageStart), nullablePositiveInt(chunk.PageEnd), chunk.SourceDigest,
			nullableOffset(chunk.SourceOffsetStart, chunk.SourceOffsetEnd, false),
			nullableOffset(chunk.SourceOffsetStart, chunk.SourceOffsetEnd, true),
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
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_chunks_fts_v2 (tokens, chunk_id) VALUES (?, ?)`,
			cjkFTSIndexText(chunk.Content), chunk.ID,
		); err != nil {
			return fmt.Errorf("CJK fts5 v2 索引插入失败: %w", err)
		}
	}
	if s.semanticMutations != nil {
		if err := s.semanticMutations.documentAddedTx(ctx, tx, doc, chunks); err != nil {
			return fmt.Errorf("更新语义索引任务失败: %w", err)
		}
	}
	if err := restoreCJKFTSCurrentTx(ctx, tx, projectionWasCurrent); err != nil {
		return fmt.Errorf("发布 CJK fts5 v2 版本失败: %w", err)
	}

	return tx.Commit()
}

// Replace 使用同一文档 ID 重建索引
func (s *SQLiteStore) Replace(ctx context.Context, doc *Document, chunks []*Chunk) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return sqliteutil.RetryOnBusy(ctx, func() error {
		return s.replaceOnce(ctx, doc, chunks)
	})
}

func (s *SQLiteStore) replaceOnce(ctx context.Context, doc *Document, chunks []*Chunk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	projectionWasCurrent, err := cjkFTSProjectionCurrentTx(ctx, tx)
	if err != nil {
		return err
	}

	var scopeUID string
	if s.semanticMutations != nil {
		state, scopeErr := loadSemanticPolicyState(ctx, tx,
			s.semanticMutations.ownerID, s.semanticMutations.corpusID)
		if scopeErr != nil {
			return scopeErr
		}
		scopeUID = state.corpusUID
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_chunks_fts WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id = ?)`,
		doc.ID,
	); err != nil {
		return fmt.Errorf("fts5 索引删除失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_chunks_fts_v2 WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id = ?)`,
		doc.ID,
	); err != nil {
		return fmt.Errorf("CJK fts5 v2 索引删除失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks WHERE doc_id = ?`, doc.ID); err != nil {
		return fmt.Errorf("删除旧 chunk 失败: %w", err)
	}
	updateSQL := `UPDATE kb_documents
		 SET title = ?, content = ?, source = ?, chunk_count = ?, updated_at = ?, status = ?, deleted = 0, error_message = ?, source_type = ?
		 WHERE id = ?`
	updateArgs := []any{doc.Title, doc.Content, doc.Source, doc.ChunkCount, doc.UpdatedAt,
		doc.Status, doc.ErrorMessage, doc.SourceType, doc.ID}
	if scopeUID != "" {
		updateSQL += ` AND corpus_uid = ?`
		updateArgs = append(updateArgs, scopeUID)
	}
	res, err := tx.ExecContext(ctx, updateSQL, updateArgs...)
	if err != nil {
		return fmt.Errorf("更新文档失败: %w", err)
	}
	if scopeUID != "" {
		if affected, _ := res.RowsAffected(); affected != 1 {
			return ErrSemanticIndexNotFound
		}
	}

	for _, chunk := range chunks {
		var embBlob []byte
		if len(chunk.Embedding) > 0 {
			embBlob = encodeFloat32Slice(chunk.Embedding)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_chunks (id, doc_id, content, chunk_index, embedding, created_at,
			 page_start,page_end,source_digest,source_offset_start,source_offset_end)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			chunk.ID, chunk.DocID, chunk.Content, chunk.Index, embBlob, chunk.CreatedAt,
			nullablePositiveInt(chunk.PageStart), nullablePositiveInt(chunk.PageEnd), chunk.SourceDigest,
			nullableOffset(chunk.SourceOffsetStart, chunk.SourceOffsetEnd, false),
			nullableOffset(chunk.SourceOffsetStart, chunk.SourceOffsetEnd, true),
		); err != nil {
			return fmt.Errorf("插入重建 chunk 失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_chunks_fts (content, chunk_id) VALUES (?, ?)`,
			chunk.Content, chunk.ID,
		); err != nil {
			return fmt.Errorf("重建 fts5 索引失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_chunks_fts_v2 (tokens, chunk_id) VALUES (?, ?)`,
			cjkFTSIndexText(chunk.Content), chunk.ID,
		); err != nil {
			return fmt.Errorf("重建 CJK fts5 v2 索引失败: %w", err)
		}
	}
	if s.semanticMutations != nil {
		if err := s.semanticMutations.documentReplacedTx(ctx, tx, doc, chunks); err != nil {
			return fmt.Errorf("更新语义索引任务失败: %w", err)
		}
	}
	if err := restoreCJKFTSCurrentTx(ctx, tx, projectionWasCurrent); err != nil {
		return fmt.Errorf("发布 CJK fts5 v2 版本失败: %w", err)
	}

	return tx.Commit()
}

// Delete 删除文档及其 chunk + FTS5 索引
func (s *SQLiteStore) Delete(ctx context.Context, docID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return sqliteutil.RetryOnBusy(ctx, func() error {
		return s.deleteOnce(ctx, docID)
	})
}

func (s *SQLiteStore) deleteOnce(ctx context.Context, docID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	projectionWasCurrent, err := cjkFTSProjectionCurrentTx(ctx, tx)
	if err != nil {
		return err
	}

	// 删除 FTS5 索引中的对应记录
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_chunks_fts WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id = ?)`,
		docID,
	); err != nil {
		return fmt.Errorf("fts5 索引删除失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_chunks_fts_v2 WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id = ?)`,
		docID,
	); err != nil {
		return fmt.Errorf("CJK fts5 v2 索引删除失败: %w", err)
	}

	if s.semanticMutations != nil {
		if err := s.semanticMutations.documentDeletedTx(ctx, tx, docID); err != nil {
			return fmt.Errorf("更新语义索引删除状态失败: %w", err)
		}
		if err := restoreCJKFTSCurrentTx(ctx, tx, projectionWasCurrent); err != nil {
			return fmt.Errorf("发布 CJK fts5 v2 版本失败: %w", err)
		}
		return tx.Commit()
	}

	// 未启用语义 revision 运行时时保留旧版物理删除语义。
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks WHERE doc_id = ?`, docID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_documents WHERE id = ?`, docID); err != nil {
		return err
	}
	if err := restoreCJKFTSCurrentTx(ctx, tx, projectionWasCurrent); err != nil {
		return fmt.Errorf("发布 CJK fts5 v2 版本失败: %w", err)
	}

	return tx.Commit()
}

// List 列出所有文档
func (s *SQLiteStore) List(ctx context.Context) ([]*Document, error) {
	query := `SELECT id, title, source, chunk_count, created_at, updated_at, status, error_message, source_type
		 FROM kb_documents d WHERE d.deleted=0`
	args := []any{}
	if clause, scopeArgs := s.semanticScopeClause("d"); clause != "" {
		query += " AND " + clause
		args = append(args, scopeArgs...)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
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
	query := `SELECT EXISTS(SELECT 1 FROM kb_chunks c
		JOIN kb_documents d ON d.id=c.doc_id WHERE d.deleted=0`
	args := []any{}
	if clause, scopeArgs := s.semanticScopeClause("d"); clause != "" {
		query += " AND " + clause
		args = append(args, scopeArgs...)
	}
	query += ` LIMIT 1)`
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&exists)
	return exists, err
}

// GetBySourceTitle 按 (source, title) 查询单个文档（不含正文）。
// 命中 idx_kb_documents_unique(source, title) 索引，避免 List 全表扫描（review M3）。
// 不存在返回 (nil, nil)，让调用方区分"未命中"与"查询出错"。
func (s *SQLiteStore) GetBySourceTitle(ctx context.Context, source, title string) (*Document, error) {
	if title == "" {
		return nil, nil
	}
	deletedClause := " AND deleted=0"
	if s.semanticMutations != nil {
		// Semantic deletes are tombstones because immutable revision history
		// still references the document row. Return that row to the Manager's
		// upsert path so re-upload revives it as a new content generation instead
		// of colliding with the production UNIQUE(source,title) index.
		deletedClause = ""
	}
	query := `SELECT id, title, source, chunk_count, created_at, updated_at, status, error_message, source_type
		 FROM kb_documents d WHERE source = ? AND title = ?` + deletedClause
	args := []any{source, title}
	if clause, scopeArgs := s.semanticScopeClause("d"); clause != "" {
		query += " AND " + clause
		args = append(args, scopeArgs...)
	}
	query += ` LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, args...)
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
	query := `SELECT id, title, content, source, chunk_count, created_at, updated_at, status, error_message, source_type
		 FROM kb_documents d WHERE id = ? AND deleted=0`
	args := []any{docID}
	if clause, scopeArgs := s.semanticScopeClause("d"); clause != "" {
		query += " AND " + clause
		args = append(args, scopeArgs...)
	}
	row := s.db.QueryRowContext(ctx, query, args...)

	doc := &Document{}
	if err := row.Scan(&doc.ID, &doc.Title, &doc.Content, &doc.Source, &doc.ChunkCount, &doc.CreatedAt, &doc.UpdatedAt, &doc.Status, &doc.ErrorMessage, &doc.SourceType); err != nil {
		if err == sql.ErrNoRows {
			if s.semanticMutations != nil {
				return nil, fmt.Errorf("%w: 文档不存在", ErrSemanticIndexNotFound)
			}
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
	filter = filter.normalize()
	clause, fargs := buildRevisionFilterClause(filter, "d", "b", "c")
	scopeClause, scopeArgs := s.semanticScopeClause("d")
	needGeneration := len(filter.DocumentGenerations) > 0
	needDate := filter.hasDateBound()
	var query string
	var args []any
	switch {
	case clause == "" && scopeClause == "" && !needDate:
		query = `SELECT c.id, c.doc_id, c.chunk_index, c.embedding FROM kb_chunks c WHERE c.embedding IS NOT NULL`
	default:
		sel := "c.id, c.doc_id, c.chunk_index, c.embedding"
		if needDate {
			sel += ", d.created_at"
		}
		query = "SELECT " + sel + " FROM kb_chunks c JOIN kb_documents d ON d.id = c.doc_id WHERE c.embedding IS NOT NULL AND d.deleted=0"
		if needGeneration {
			query = "SELECT " + sel + " FROM kb_chunks c JOIN kb_documents d ON d.id = c.doc_id JOIN kb_semantic_document_bindings b ON b.document_id=d.id AND b.lifecycle_state='active' WHERE c.embedding IS NOT NULL AND d.deleted=0"
		}
		if clause != "" {
			query += " AND " + clause
			args = append(args, fargs...)
		}
		if scopeClause != "" {
			query += " AND " + scopeClause
			args = append(args, scopeArgs...)
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

	// Quote each token before composing the OR expression. Token text is data,
	// never FTS syntax; this also avoids punctuation-driven parser failures.
	ftsQuery := cjkFTSQuery(keywords)
	if ftsQuery == "" {
		return nil, nil
	}

	// 元数据过滤生效于 LIMIT/截断之前：源/源类型下推 SQL（JOIN kb_documents + IN）；
	// 日期取 d.created_at 在 Go 层按真实时刻比较。带日期过滤时不能用 SQL LIMIT（否则日期
	// 匹配项可能因 bm25 排序落在 LIMIT 之外被漏召回），改为按 score 顺序扫描、Go 过滤后取 topK。
	filter = filter.normalize()
	clause, fargs := buildRevisionFilterClause(filter, "d", "b", "c")
	scopeClause, scopeArgs := s.semanticScopeClause("d")
	needGeneration := len(filter.DocumentGenerations) > 0
	needDate := filter.hasDateBound()

	sel := "f.chunk_id, c.content, bm25(kb_chunks_fts_v2) as score"
	if needDate {
		sel += ", d.created_at"
	}
	// Always join the document tombstone boundary. Semantic deletes retain
	// immutable chunks for revision history; neither FTS nor LIKE fallback may
	// surface those chunks after d.deleted becomes true.
	from := `kb_chunks_fts_v2 f
		 JOIN kb_chunks c ON c.id = f.chunk_id
		 JOIN kb_documents d ON d.id = c.doc_id`
	if needGeneration {
		from += ` JOIN kb_semantic_document_bindings b
		 ON b.document_id=d.id AND b.lifecycle_state='active'`
	}
	where := "d.deleted=0 AND kb_chunks_fts_v2 MATCH ?"
	args := []any{ftsQuery}
	if clause != "" {
		where += " AND " + clause
		args = append(args, fargs...)
	}
	if scopeClause != "" {
		where += " AND " + scopeClause
		args = append(args, scopeArgs...)
	}
	sqlQuery := "SELECT " + sel + " FROM " + from + " WHERE " + where + " ORDER BY score"
	if !needDate {
		sqlQuery += " LIMIT ?"
		args = append(args, topK)
	}

	ftsStarted := time.Now()
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		// FTS5 查询失败（可能是特殊字符），降级到 LIKE 搜索
		observeRetrievalLane(ctx, RetrievalLaneFTS, time.Since(ftsStarted), 0, err, true)
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
				observeRetrievalLane(ctx, RetrievalLaneFTS, time.Since(ftsStarted), 0, err, false)
				return nil, err
			}
			// 日期过滤在 Go 层（rows 已按 score 排序，过滤后取前 topK 即最相关者）。
			if !filter.matchesDate(createdAt) {
				continue
			}
		} else if err := rows.Scan(&r.chunkID, &r.content, &r.score); err != nil {
			observeRetrievalLane(ctx, RetrievalLaneFTS, time.Since(ftsStarted), 0, err, false)
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
		observeRetrievalLane(ctx, RetrievalLaneFTS, time.Since(ftsStarted), 0, err, false)
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
		observeRetrievalLane(ctx, RetrievalLaneFTS, time.Since(ftsStarted), 0, nil, true)
		return s.fallbackTextSearch(ctx, keywords, topK, filter)
	}
	observeRetrievalLane(ctx, RetrievalLaneFTS, time.Since(ftsStarted), len(results), nil, false)
	return results, nil
}

// fallbackTextSearch FTS5 不可用或结果为空时的降级搜索（LIKE 匹配）。
// 与主路径一致地把元数据过滤下推到 LIMIT 之前（JOIN kb_documents）。
func (s *SQLiteStore) fallbackTextSearch(ctx context.Context, keywords []string, topK int, filter Filter) ([]*SearchResult, error) {
	likeStarted := time.Now()
	var query strings.Builder
	var args []any

	filter = filter.normalize()
	clause, fargs := buildRevisionFilterClause(filter, "d", "b", "c")
	scopeClause, scopeArgs := s.semanticScopeClause("d")
	needGeneration := len(filter.DocumentGenerations) > 0
	needDate := filter.hasDateBound()

	// Fix 15: 不查询 embedding 列，文本降级搜索无需加载向量 BLOB。
	// 统一以别名 c 引用 kb_chunks，便于在有过滤时 JOIN kb_documents。
	// chunk.CreatedAt 取 c.created_at（片段时间，供时间衰减/展示）；日期过滤用 d.created_at
	// （文档时间，与主路径语义一致），故 needDate 时额外多取一列。
	query.WriteString(`SELECT c.id, c.doc_id, c.content, c.chunk_index, c.created_at,
		COALESCE(c.page_start,0),COALESCE(c.page_end,0),c.source_digest,
		COALESCE(c.source_offset_start,0),COALESCE(c.source_offset_end,0)`)
	if needDate {
		query.WriteString(", d.created_at")
	}
	query.WriteString(" FROM kb_chunks c JOIN kb_documents d ON d.id = c.doc_id WHERE d.deleted=0 AND (")
	if needGeneration {
		query.Reset()
		query.WriteString(`SELECT c.id, c.doc_id, c.content, c.chunk_index, c.created_at,
			COALESCE(c.page_start,0),COALESCE(c.page_end,0),c.source_digest,
			COALESCE(c.source_offset_start,0),COALESCE(c.source_offset_end,0)`)
		if needDate {
			query.WriteString(", d.created_at")
		}
		query.WriteString(` FROM kb_chunks c
			JOIN kb_documents d ON d.id=c.doc_id
			JOIN kb_semantic_document_bindings b
			  ON b.document_id=d.id AND b.lifecycle_state='active'
			WHERE d.deleted=0 AND (`)
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
	if scopeClause != "" {
		query.WriteString(" AND ")
		query.WriteString(scopeClause)
		args = append(args, scopeArgs...)
	}
	// 带日期过滤时不能用 SQL LIMIT（日期在 Go 层裁，匹配项可能排在 LIMIT 之外）。
	if !needDate {
		query.WriteString(" LIMIT ?")
		args = append(args, topK)
	}

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		observeRetrievalLane(ctx, RetrievalLaneLike, time.Since(likeStarted), 0, err, false)
		return nil, err
	}
	defer rows.Close()

	var results []*SearchResult
	for rows.Next() {
		chunk := &Chunk{}
		var docCreatedAt time.Time
		// Fix 15: Scan 与 SELECT 对齐（已移除 embedding 列）
		if needDate {
			if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.Index, &chunk.CreatedAt,
				&chunk.PageStart, &chunk.PageEnd, &chunk.SourceDigest,
				&chunk.SourceOffsetStart, &chunk.SourceOffsetEnd, &docCreatedAt); err != nil {
				logger.Error("[knowledge] fallbackTextSearch scan 失败", "error", err)
				continue
			}
			if !filter.matchesDate(docCreatedAt) {
				continue
			}
		} else if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.Index, &chunk.CreatedAt,
			&chunk.PageStart, &chunk.PageEnd, &chunk.SourceDigest,
			&chunk.SourceOffsetStart, &chunk.SourceOffsetEnd); err != nil {
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

	err = rows.Err()
	observeRetrievalLane(ctx, RetrievalLaneLike, time.Since(likeStarted), len(results), err, false)
	return results, err
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
	query.WriteString(`SELECT c.id, c.doc_id, d.title, d.source, d.source_type, d.chunk_count, c.content, c.chunk_index, c.embedding, c.created_at,
		COALESCE(c.page_start,0),COALESCE(c.page_end,0),c.source_digest,
		COALESCE(c.source_offset_start,0),COALESCE(c.source_offset_end,0)
		 FROM kb_chunks c
		 JOIN kb_documents d ON d.id = c.doc_id
		 WHERE c.id IN (`)
	query.WriteString(strings.Join(placeholders, ","))
	query.WriteString(")")
	if clause, scopeArgs := s.semanticScopeClause("d"); clause != "" {
		query.WriteString(" AND ")
		query.WriteString(clause)
		args = append(args, scopeArgs...)
	}

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("批量获取 chunks 失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		chunk := &Chunk{}
		var embBlob []byte
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.DocTitle, &chunk.Source,
			&chunk.SourceType, &chunk.ChunkCount, &chunk.Content, &chunk.Index, &embBlob,
			&chunk.CreatedAt, &chunk.PageStart, &chunk.PageEnd, &chunk.SourceDigest,
			&chunk.SourceOffsetStart, &chunk.SourceOffsetEnd); err != nil {
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
