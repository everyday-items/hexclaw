// Package sqlite 提供基于 SQLite 的存储实现
//
// 这是 HexClaw 的默认存储驱动，零配置即可使用。
// 数据库文件默认位于 ~/.hexclaw/data.db。
//
// 使用 modernc.org/sqlite 纯 Go 实现，无 CGO 依赖，
// 跨平台编译无需额外工具链。
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动

	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	"github.com/hexagon-codes/toolkit/lang/stringx"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// Store SQLite 存储实现
type Store struct {
	db *sql.DB

	searchFTSStmt      *sql.Stmt
	searchFTSCountStmt *sql.Stmt
	searchLikeStmt     *sql.Stmt

	searchCacheMu sync.RWMutex
	searchCache   map[searchCacheKey]searchCacheEntry
	searchOrder   []searchCacheKey

	forkCacheMu sync.RWMutex
	forkCache   map[forkCacheKey]forkCacheEntry
}

type searchCacheKey struct {
	userID string
	query  string
	limit  int
	offset int
}

type searchCacheEntry struct {
	results []*storage.SearchResult
	total   int
}

type forkCacheKey struct {
	sessionID string
	messageID string
}

type forkCacheEntry struct {
	title      string
	platform   string
	instanceID string
	chatID     string
	msgRowID   int64
}

const searchCacheMaxEntries = 128

// New 创建 SQLite 存储
//
// dbPath 支持 ~ 前缀，会自动展开为用户主目录。
// 如果目录不存在会自动创建。
func New(dbPath string) (*Store, error) {
	// 展开 ~ 为用户主目录
	if strings.HasPrefix(dbPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("获取主目录失败: %w", err)
		}
		dbPath = filepath.Join(home, dbPath[2:])
	}

	// 确保目录存在
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 确保外键约束对已有连接生效（连接池复用时 DSN pragma 可能不触发）
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return nil, fmt.Errorf("启用外键约束失败: %w", err)
	}

	// SQLite WAL 模式下允许多个并发读连接
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	return &Store{db: db}, nil
}

// Init 初始化数据库表
//
// 使用集中式版本迁移系统，所有 DDL 定义在 storage/migrate/migrations.go。
func (s *Store) Init(ctx context.Context) error {
	if err := migrate.Run(ctx, s.db, migrate.All); err != nil {
		return fmt.Errorf("数据库迁移失败: %w", err)
	}
	s.prepareHotStatements()
	return nil
}

// searchMsgCols 搜索结果 Scan 用的消息子集（搜索结果只需要核心字段）
const searchMsgCols = `m.id, m.session_id, m.parent_id, m.role, m.content, m.content_type, m.metadata, m.feedback, m.model_name, m.prompt_tokens, m.completion_tokens, m.finish_reason, m.latency_ms, m.request_id, m.meta, m.created_at`

func (s *Store) prepareHotStatements() {
	if stmt, err := s.db.Prepare(`
		SELECT ` + searchMsgCols + `,
		       s.title, bm25(messages_fts) AS rank
		FROM messages_fts
		JOIN messages m ON m.rowid = messages_fts.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE messages_fts MATCH ? AND s.user_id = ? AND s.status >= 0
		ORDER BY rank
		LIMIT ? OFFSET ?`); err == nil {
		s.searchFTSStmt = stmt
	}

	if stmt, err := s.db.Prepare(`
		SELECT COUNT(*)
		FROM messages_fts
		JOIN messages m ON m.rowid = messages_fts.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE messages_fts MATCH ? AND s.user_id = ? AND s.status >= 0`); err == nil {
		s.searchFTSCountStmt = stmt
	}

	if stmt, err := s.db.Prepare(`
		SELECT ` + searchMsgCols + `,
		       s.title, COUNT(*) OVER() AS total_count
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE m.content LIKE ? ESCAPE '\' AND s.user_id = ? AND s.status >= 0
		ORDER BY m.created_at DESC
		LIMIT ? OFFSET ?`); err == nil {
		s.searchLikeStmt = stmt
	}
}

// Close 关闭数据库连接
func (s *Store) Close() error {
	if s.searchFTSStmt != nil {
		_ = s.searchFTSStmt.Close()
	}
	if s.searchFTSCountStmt != nil {
		_ = s.searchFTSCountStmt.Close()
	}
	if s.searchLikeStmt != nil {
		_ = s.searchLikeStmt.Close()
	}
	return s.db.Close()
}

// DB 返回底层 *sql.DB 实例
//
// 供知识库等模块共享数据库连接使用。
func (s *Store) DB() *sql.DB {
	return s.db
}

// ─── SQL 列定义 + Scan 辅助（减少重复） ───────────────

const sessionCols = `id, user_id, platform, instance_id, chat_id, title, parent_session_id, branch_message_id, status, message_count, total_prompt_tokens, total_completion_tokens, last_message_preview, meta, created_at, updated_at`

const messageCols = `id, session_id, parent_id, role, content, content_type, metadata, feedback, model_name, prompt_tokens, completion_tokens, finish_reason, latency_ms, request_id, meta, created_at`

type scannable interface{ Scan(dest ...any) error }

func scanSession(sc scannable) (*storage.Session, error) {
	var s storage.Session
	if err := sc.Scan(
		&s.ID, &s.UserID, &s.Platform, &s.InstanceID, &s.ChatID,
		&s.Title, &s.ParentSessionID, &s.BranchMessageID,
		&s.Status, &s.MessageCount, &s.TotalPromptTokens, &s.TotalCompletionTokens,
		&s.LastMessagePreview, &s.Meta,
		&s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

func scanMessage(sc scannable) (*storage.MessageRecord, error) {
	var m storage.MessageRecord
	if err := sc.Scan(
		&m.ID, &m.SessionID, &m.ParentID, &m.Role, &m.Content,
		&m.ContentType, &m.Metadata, &m.Feedback,
		&m.ModelName, &m.PromptTokens, &m.CompletionTokens,
		&m.FinishReason, &m.LatencyMs, &m.RequestID, &m.Meta,
		&m.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &m, nil
}

func sessionInsertArgs(s *storage.Session) []any {
	return []any{
		s.ID, s.UserID, s.Platform, s.InstanceID, s.ChatID,
		s.Title, s.ParentSessionID, s.BranchMessageID,
		s.Status, s.MessageCount, s.TotalPromptTokens, s.TotalCompletionTokens,
		s.LastMessagePreview, s.Meta,
		s.CreatedAt, s.UpdatedAt,
	}
}

func messageInsertArgs(m *storage.MessageRecord) []any {
	return []any{
		m.ID, m.SessionID, m.ParentID, m.Role, m.Content,
		m.ContentType, m.Metadata, m.Feedback,
		m.ModelName, m.PromptTokens, m.CompletionTokens,
		m.FinishReason, m.LatencyMs, m.RequestID, m.Meta,
		m.CreatedAt,
	}
}

func (s *Store) invalidateSearchCache() {
	s.searchCacheMu.Lock()
	s.searchCache = nil
	s.searchOrder = nil
	s.searchCacheMu.Unlock()
}

func (s *Store) invalidateForkCache() {
	s.forkCacheMu.Lock()
	s.forkCache = nil
	s.forkCacheMu.Unlock()
}

func cloneSearchResults(results []*storage.SearchResult) []*storage.SearchResult {
	cloned := make([]*storage.SearchResult, len(results))
	for i, result := range results {
		if result == nil {
			continue
		}
		copyResult := *result
		if result.Message != nil {
			copyMessage := *result.Message
			copyResult.Message = &copyMessage
		}
		cloned[i] = &copyResult
	}
	return cloned
}

func (s *Store) getCachedSearch(userID, query string, limit, offset int) ([]*storage.SearchResult, int, bool) {
	key := searchCacheKey{userID: userID, query: query, limit: limit, offset: offset}
	s.searchCacheMu.RLock()
	entry, ok := s.searchCache[key]
	s.searchCacheMu.RUnlock()
	if !ok {
		return nil, 0, false
	}
	return cloneSearchResults(entry.results), entry.total, true
}

func (s *Store) cacheSearchResult(userID, query string, limit, offset, total int, results []*storage.SearchResult) {
	key := searchCacheKey{userID: userID, query: query, limit: limit, offset: offset}
	entry := searchCacheEntry{
		results: cloneSearchResults(results),
		total:   total,
	}

	s.searchCacheMu.Lock()
	defer s.searchCacheMu.Unlock()

	if s.searchCache == nil {
		s.searchCache = make(map[searchCacheKey]searchCacheEntry, searchCacheMaxEntries)
	}
	if _, exists := s.searchCache[key]; !exists {
		s.searchOrder = append(s.searchOrder, key)
		if len(s.searchOrder) > searchCacheMaxEntries {
			evict := s.searchOrder[0]
			s.searchOrder = s.searchOrder[1:]
			delete(s.searchCache, evict)
		}
	}
	s.searchCache[key] = entry
}

func (s *Store) getCachedForkSource(sessionID, messageID string) (forkCacheEntry, bool) {
	key := forkCacheKey{sessionID: sessionID, messageID: messageID}
	s.forkCacheMu.RLock()
	entry, ok := s.forkCache[key]
	s.forkCacheMu.RUnlock()
	return entry, ok
}

func (s *Store) cacheForkSource(sessionID, messageID string, entry forkCacheEntry) {
	key := forkCacheKey{sessionID: sessionID, messageID: messageID}
	s.forkCacheMu.Lock()
	if s.forkCache == nil {
		s.forkCache = make(map[forkCacheKey]forkCacheEntry)
	}
	s.forkCache[key] = entry
	s.forkCacheMu.Unlock()
}

// CreateSession 创建新会话
func (s *Store) CreateSession(ctx context.Context, session *storage.Session) error {
	now := time.Now()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = now
	}
	if session.Status == 0 {
		session.Status = 1 // 默认 active
	}
	if session.Meta == "" {
		session.Meta = "{}"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (`+sessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionInsertArgs(session)...,
	)
	return err
}

// GetSession 获取会话
func (s *Store) GetSession(ctx context.Context, id string) (*storage.Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id,
	)
	sess, err := scanSession(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	return sess, nil
}

// FindSessionByScope 按 scope 查找最近活跃会话（仅 active）
func (s *Store) FindSessionByScope(ctx context.Context, userID, platform, instanceID, chatID string) (*storage.Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+`
		 FROM sessions
		 WHERE user_id = ? AND platform = ? AND instance_id = ? AND chat_id = ? AND status = 1
		 ORDER BY updated_at DESC, created_at DESC
		 LIMIT 1`,
		userID, platform, instanceID, chatID,
	)
	sess, err := scanSession(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	return sess, nil
}

// ListSessions 列出用户的会话（仅 active + archived，排除 deleted）
func (s *Store) ListSessions(ctx context.Context, userID string, limit, offset int) ([]*storage.Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE user_id = ? AND status >= 0 ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var sessions []*storage.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// DeleteSession 软删除会话（标记 status=-1）
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET status = -1, updated_at = ? WHERE id = ?`,
		time.Now(), id,
	)
	if err == nil {
		s.invalidateSearchCache()
		s.invalidateForkCache()
	}
	return err
}

// CleanupOldSessions 删除超过指定天数未活跃的会话及其消息
func (s *Store) CleanupOldSessions(ctx context.Context, olderThanDays int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -olderThanDays)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 先删子表消息，再删父表会话（维护引用完整性）
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE session_id IN (SELECT id FROM sessions WHERE updated_at < ?)`, cutoff,
	); err != nil {
		return 0, err
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.invalidateSearchCache()
	s.invalidateForkCache()
	return result.RowsAffected()
}

// SaveMessage 保存消息 + 原子更新会话冗余统计字段
func (s *Store) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	if msg.ContentType == "" {
		msg.ContentType = "text"
	}
	if msg.Meta == "" {
		msg.Meta = "{}"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO messages (`+messageCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageInsertArgs(msg)...,
	)
	if err != nil {
		return err
	}

	// 原子更新会话冗余字段：message_count, token 汇总, last_message_preview, updated_at
	preview := msg.Content
	if len(preview) > 200 {
		preview = preview[:200]
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE sessions SET
			updated_at = ?,
			message_count = message_count + 1,
			total_prompt_tokens = total_prompt_tokens + ?,
			total_completion_tokens = total_completion_tokens + ?,
			last_message_preview = ?
		 WHERE id = ?`,
		time.Now(), msg.PromptTokens, msg.CompletionTokens, preview, msg.SessionID,
	)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateSearchCache()
	return nil
}

// DeleteMessage 删除单条消息
func (s *Store) DeleteMessage(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id)
	if err == nil {
		s.invalidateSearchCache()
		s.invalidateForkCache()
	}
	return err
}

// ListMessages 获取会话的消息历史
func (s *Store) ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]*storage.MessageRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageCols+` FROM (
			SELECT `+messageCols+` FROM messages WHERE session_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?
		) ORDER BY created_at ASC`,
		sessionID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []*storage.MessageRecord
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

// UpdateMessageFeedback 更新消息反馈。
func (s *Store) UpdateMessageFeedback(ctx context.Context, id, feedback string) error {
	switch feedback {
	case "", "like", "dislike":
	default:
		return fmt.Errorf("无效反馈值: %s", feedback)
	}

	result, err := s.db.ExecContext(ctx, `UPDATE messages SET feedback = ? WHERE id = ?`, feedback, id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return storage.ErrNotFound
	}
	s.invalidateSearchCache()
	return nil
}

// SaveCost 记录成本
func (s *Store) SaveCost(ctx context.Context, record *storage.CostRecord) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	if record.Meta == "" {
		record.Meta = "{}"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cost_records (id, user_id, session_id, message_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost, meta, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.UserID, record.SessionID, record.MessageID, record.Provider, record.Model,
		record.PromptTokens, record.CompletionTokens, record.TotalTokens, record.Cost, record.Meta, record.CreatedAt,
	)
	return err
}

// GetUserCost 获取用户在指定时间范围内的总成本
func (s *Store) GetUserCost(ctx context.Context, userID string, since time.Time) (float64, error) {
	var total sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(cost) FROM cost_records WHERE user_id = ? AND created_at >= ?`,
		userID, since,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Float64, nil
}

// GetGlobalCost 获取全局在指定时间范围内的总成本
func (s *Store) GetGlobalCost(ctx context.Context, since time.Time) (float64, error) {
	var total sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(cost) FROM cost_records WHERE created_at >= ?`, since,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Float64, nil
}

// UpdateSession 更新会话信息
func (s *Store) UpdateSession(ctx context.Context, session *storage.Session) error {
	session.UpdatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		session.Title, session.UpdatedAt, session.ID,
	)
	if err == nil {
		s.invalidateSearchCache()
		s.invalidateForkCache()
	}
	return err
}

// SearchMessages 全文搜索消息内容
//
// 优先使用 FTS5 全文索引搜索；FTS 不可用或无结果时自动降级为 LIKE 搜索。
// FTS5 默认 tokenizer 对无空格的中文文本支持有限，因此 FTS 结果为空时
// 必须降级，否则中文搜索会静默返回空。
func (s *Store) SearchMessages(ctx context.Context, userID, query string, limit, offset int) ([]*storage.SearchResult, int, error) {
	if limit <= 0 {
		limit = 20
	}
	if cached, total, ok := s.getCachedSearch(userID, query, limit, offset); ok {
		return cached, total, nil
	}

	var rows *sql.Rows
	var err error
	if s.searchFTSStmt != nil {
		rows, err = s.searchFTSStmt.QueryContext(ctx, query, userID, limit, offset)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+searchMsgCols+`,
			        s.title, bm25(messages_fts) AS rank
			 FROM messages_fts
			 JOIN messages m ON m.rowid = messages_fts.rowid
			 JOIN sessions s ON s.id = m.session_id
			 WHERE messages_fts MATCH ? AND s.user_id = ? AND s.status >= 0
			 ORDER BY rank
			 LIMIT ? OFFSET ?`,
			query, userID, limit, offset,
		)
	}
	if err != nil {
		return s.searchMessagesLike(ctx, userID, query, limit, offset)
	}
	defer func() { _ = rows.Close() }()

	results := make([]*storage.SearchResult, 0, limit)
	for rows.Next() {
		var m storage.MessageRecord
		var r storage.SearchResult
		if err := rows.Scan(
			&m.ID, &m.SessionID, &m.ParentID, &m.Role, &m.Content,
			&m.ContentType, &m.Metadata, &m.Feedback,
			&m.ModelName, &m.PromptTokens, &m.CompletionTokens,
			&m.FinishReason, &m.LatencyMs, &m.RequestID, &m.Meta,
			&m.CreatedAt,
			&r.SessionTitle, &r.Rank,
		); err != nil {
			return nil, 0, err
		}
		r.Message = &m
		results = append(results, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	total, countErr := s.countFTSMessages(ctx, userID, query)
	if countErr != nil {
		return s.searchMessagesLike(ctx, userID, query, limit, offset)
	}
	if len(results) == 0 {
		if countErr == nil && total > 0 {
			s.cacheSearchResult(userID, query, limit, offset, total, results)
			return results, total, nil
		}
		likeResults, likeTotal, likeErr := s.searchMessagesLike(ctx, userID, query, limit, offset)
		if likeErr == nil {
			s.cacheSearchResult(userID, query, limit, offset, likeTotal, likeResults)
		}
		return likeResults, likeTotal, likeErr
	}
	s.cacheSearchResult(userID, query, limit, offset, total, results)
	return results, total, nil
}

func (s *Store) countFTSMessages(ctx context.Context, userID, query string) (int, error) {
	var total int
	if s.searchFTSCountStmt != nil {
		if err := s.searchFTSCountStmt.QueryRowContext(ctx, query, userID).Scan(&total); err != nil {
			return 0, err
		}
		return total, nil
	}

	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM messages_fts
		 JOIN messages m ON m.rowid = messages_fts.rowid
		 JOIN sessions s ON s.id = m.session_id
		 WHERE messages_fts MATCH ? AND s.user_id = ?`,
		query, userID,
	).Scan(&total)
	return total, err
}

// searchMessagesLike 降级的 LIKE 搜索（FTS 不可用时使用）
//
// 使用 window function COUNT(*) OVER() 在单次查询中同时获取匹配数据和总数，
// 避免 COUNT + SELECT 双查询。
func (s *Store) searchMessagesLike(ctx context.Context, userID, query string, limit, offset int) ([]*storage.SearchResult, int, error) {
	likeQuery := "%" + sqliteutil.EscapeLike(query) + "%"
	var (
		rows *sql.Rows
		err  error
	)

	if s.searchLikeStmt != nil {
		rows, err = s.searchLikeStmt.QueryContext(ctx, likeQuery, userID, limit, offset)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+searchMsgCols+`,
			        s.title, COUNT(*) OVER() AS total_count
			 FROM messages m JOIN sessions s ON s.id = m.session_id
			 WHERE m.content LIKE ? ESCAPE '\' AND s.user_id = ? AND s.status >= 0
			 ORDER BY m.created_at DESC
			 LIMIT ? OFFSET ?`,
			likeQuery, userID, limit, offset,
		)
	}
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	results := make([]*storage.SearchResult, 0, limit)
	var total int
	for rows.Next() {
		var m storage.MessageRecord
		var r storage.SearchResult
		if err := rows.Scan(
			&m.ID, &m.SessionID, &m.ParentID, &m.Role, &m.Content,
			&m.ContentType, &m.Metadata, &m.Feedback,
			&m.ModelName, &m.PromptTokens, &m.CompletionTokens,
			&m.FinishReason, &m.LatencyMs, &m.RequestID, &m.Meta,
			&m.CreatedAt,
			&r.SessionTitle, &total,
		); err != nil {
			return nil, 0, err
		}
		r.Message = &m
		results = append(results, &r)
	}
	return results, total, rows.Err()
}

// ForkSession 从指定消息处创建分支会话
//
// 使用 rowid 比较而非 created_at，避免 datetime 精度丢失导致
// 边界消息被遗漏（time.Time 经 SQLite 存储后纳秒精度丢失）。
func (s *Store) ForkSession(ctx context.Context, sourceSessionID, messageID, userID string) (*storage.Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. 验证源会话存在并获取 scope
	var sourceTitle, sourcePlatform, sourceInstanceID, sourceChatID string
	var msgRowID int64
	if cached, ok := s.getCachedForkSource(sourceSessionID, messageID); ok {
		sourceTitle = cached.title
		sourcePlatform = cached.platform
		sourceInstanceID = cached.instanceID
		sourceChatID = cached.chatID
		msgRowID = cached.msgRowID
	} else {
		err = tx.QueryRowContext(ctx,
			`SELECT s.title, s.platform, s.instance_id, s.chat_id,
			        (SELECT rowid FROM messages WHERE id = ? AND session_id = s.id)
			 FROM sessions s
			 WHERE s.id = ?`,
			messageID, sourceSessionID,
		).Scan(&sourceTitle, &sourcePlatform, &sourceInstanceID, &sourceChatID, &msgRowID)
		if err != nil {
			return nil, fmt.Errorf("源会话不存在: %w", err)
		}
		if msgRowID == 0 {
			return nil, fmt.Errorf("消息不存在")
		}
		s.cacheForkSource(sourceSessionID, messageID, forkCacheEntry{
			title:      sourceTitle,
			platform:   sourcePlatform,
			instanceID: sourceInstanceID,
			chatID:     sourceChatID,
			msgRowID:   msgRowID,
		})
	}

	// 2. 创建分支会话
	now := time.Now()
	newSessionID := "sess-" + idgen.ShortID()
	branchTitle := stringx.Truncate(sourceTitle+" (分支)", 60)

	forkSession := &storage.Session{
		ID: newSessionID, UserID: userID, Platform: sourcePlatform, InstanceID: sourceInstanceID,
		ChatID: sourceChatID, Title: branchTitle, ParentSessionID: sourceSessionID,
		BranchMessageID: messageID, Status: 1, Meta: "{}",
		CreatedAt: now, UpdatedAt: now,
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions (`+sessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionInsertArgs(forkSession)...,
	)
	if err != nil {
		return nil, fmt.Errorf("创建分支会话失败: %w", err)
	}

	// 3. 复制源会话中 messageID 之前（含）的所有消息到新会话
	// 使用 rowid <= ? 而非 created_at <= ?，避免精度丢失
	// 不依赖插入顺序；读取时仍按 created_at 排序
	forkPrefix := "msg-fork-" + strings.TrimPrefix(newSessionID, "sess-") + "-"
	_, err = tx.ExecContext(ctx,
		`INSERT INTO messages (`+messageCols+`)
			 SELECT ? || rowid, ?, parent_id, role, content, content_type, metadata, feedback,
			        model_name, prompt_tokens, completion_tokens, finish_reason, latency_ms, request_id, meta, created_at
			 FROM messages
			 WHERE session_id = ? AND rowid <= ?`,
		forkPrefix, newSessionID, sourceSessionID, msgRowID,
	)
	if err != nil {
		return nil, fmt.Errorf("复制消息失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.invalidateSearchCache()

	return forkSession, nil
}

// ListSessionBranches 列出会话的所有分支
func (s *Store) ListSessionBranches(ctx context.Context, sessionID string) ([]*storage.Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE parent_session_id = ? AND status >= 0 ORDER BY created_at DESC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var sessions []*storage.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// WithTx 在事务中执行操作
func (s *Store) WithTx(ctx context.Context, fn func(storage.Store) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	txStore := &txStore{tx: tx}
	if err := fn(txStore); err != nil {
		return err
	}
	return tx.Commit()
}

// schema 已迁移到 storage/migrate/migrations.go，不再在此定义。
