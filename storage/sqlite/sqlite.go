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
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动

	"github.com/everyday-items/hexclaw/storage"
	"github.com/everyday-items/toolkit/lang/stringx"
	"github.com/everyday-items/toolkit/util/idgen"
)

// Store SQLite 存储实现
type Store struct {
	db *sql.DB
}

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

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// SQLite 优化参数
	db.SetMaxOpenConns(1) // SQLite 写入单连接
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	return &Store{db: db}, nil
}

// Init 初始化数据库表
func (s *Store) Init(ctx context.Context) error {
	// 先执行增量迁移：对已有数据库添加新字段，确保旧表结构兼容新 schema
	s.runMigrations(ctx)

	_, err := s.db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("初始化数据库表失败: %w", err)
	}
	return nil
}

// runMigrations 执行增量迁移
//
// 每条 ALTER TABLE 独立执行，忽略已存在的列/索引错误。
// FTS5 虚拟表和触发器使用 IF NOT EXISTS 保证幂等。
func (s *Store) runMigrations(ctx context.Context) {
	stmts := []string{
		`ALTER TABLE sessions ADD COLUMN parent_session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN branch_message_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id)`,
		`ALTER TABLE messages ADD COLUMN parent_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_messages_parent_id ON messages(parent_id)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(content, content='messages', content_rowid='rowid')`,
		`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content); END`,
		`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content); END`,
		`CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content); INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content); END`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			// 忽略 "duplicate column" 等已存在的错误
			if !strings.Contains(err.Error(), "duplicate") && !strings.Contains(err.Error(), "already exists") {
				// FTS5/trigger 创建失败需要记录
				_ = err // 非致命错误，静默忽略
			}
		}
	}
}

// Close 关闭数据库连接
func (s *Store) Close() error {
	return s.db.Close()
}

// DB 返回底层 *sql.DB 实例
//
// 供知识库等模块共享数据库连接使用。
func (s *Store) DB() *sql.DB {
	return s.db
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, platform, title, parent_session_id, branch_message_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.UserID, session.Platform, session.Title, session.ParentSessionID, session.BranchMessageID, session.CreatedAt, session.UpdatedAt,
	)
	return err
}

// GetSession 获取会话
func (s *Store) GetSession(ctx context.Context, id string) (*storage.Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, platform, title, parent_session_id, branch_message_id, created_at, updated_at FROM sessions WHERE id = ?`, id,
	)
	var sess storage.Session
	if err := row.Scan(&sess.ID, &sess.UserID, &sess.Platform, &sess.Title, &sess.ParentSessionID, &sess.BranchMessageID, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
		return nil, err
	}
	return &sess, nil
}

// ListSessions 列出用户的会话
func (s *Store) ListSessions(ctx context.Context, userID string, limit, offset int) ([]*storage.Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, platform, title, parent_session_id, branch_message_id, created_at, updated_at FROM sessions WHERE user_id = ? ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*storage.Session
	for rows.Next() {
		var sess storage.Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Platform, &sess.Title, &sess.ParentSessionID, &sess.BranchMessageID, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, &sess)
	}
	return sessions, rows.Err()
}

// DeleteSession 删除会话及其所有消息
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveMessage 保存消息（事务保证原子性）
func (s *Store) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, parent_id, role, content, metadata, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.SessionID, msg.ParentID, msg.Role, msg.Content, msg.Metadata, msg.CreatedAt,
	)
	if err != nil {
		return err
	}

	// 更新会话的 updated_at
	_, err = tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, time.Now(), msg.SessionID,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// DeleteMessage 删除单条消息
func (s *Store) DeleteMessage(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id)
	return err
}

// ListMessages 获取会话的消息历史
func (s *Store) ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]*storage.MessageRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, parent_id, role, content, metadata, created_at FROM (
			SELECT id, session_id, parent_id, role, content, metadata, created_at FROM messages WHERE session_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?
		) ORDER BY created_at ASC`,
		sessionID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []*storage.MessageRecord
	for rows.Next() {
		var msg storage.MessageRecord
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.ParentID, &msg.Role, &msg.Content, &msg.Metadata, &msg.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, &msg)
	}
	return messages, rows.Err()
}

// SaveCost 记录成本
func (s *Store) SaveCost(ctx context.Context, record *storage.CostRecord) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cost_records (id, user_id, provider, model, tokens, cost, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.UserID, record.Provider, record.Model, record.Tokens, record.Cost, record.CreatedAt,
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

	// 尝试 FTS5 搜索
	var total int
	countRow := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages_fts
		 JOIN messages ON messages.rowid = messages_fts.rowid
		 JOIN sessions ON sessions.id = messages.session_id
		 WHERE messages_fts MATCH ? AND sessions.user_id = ?`,
		query, userID,
	)
	if err := countRow.Scan(&total); err != nil {
		// FTS 查询失败（表不存在/语法错误）→ 降级
		return s.searchMessagesLike(ctx, userID, query, limit, offset)
	}

	// FTS 返回 0 条 → 降级到 LIKE（中文无空格分词场景）
	if total == 0 {
		return s.searchMessagesLike(ctx, userID, query, limit, offset)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.session_id, m.parent_id, m.role, m.content, m.metadata, m.created_at,
		        s.title, rank
		 FROM messages_fts
		 JOIN messages m ON m.rowid = messages_fts.rowid
		 JOIN sessions s ON s.id = m.session_id
		 WHERE messages_fts MATCH ? AND s.user_id = ?
		 ORDER BY rank
		 LIMIT ? OFFSET ?`,
		query, userID, limit, offset,
	)
	if err != nil {
		return s.searchMessagesLike(ctx, userID, query, limit, offset)
	}
	defer rows.Close()

	var results []*storage.SearchResult
	for rows.Next() {
		var msg storage.MessageRecord
		var r storage.SearchResult
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.ParentID, &msg.Role, &msg.Content, &msg.Metadata, &msg.CreatedAt, &r.SessionTitle, &r.Rank); err != nil {
			return nil, 0, err
		}
		r.Message = &msg
		results = append(results, &r)
	}
	return results, total, rows.Err()
}

// escapeLike 转义 LIKE 通配符，防止用户输入 % 或 _ 被当作通配符
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// searchMessagesLike 降级的 LIKE 搜索（FTS 不可用时使用）
//
// 使用 window function COUNT(*) OVER() 在单次查询中同时获取匹配数据和总数，
// 避免 COUNT + SELECT 双查询。
func (s *Store) searchMessagesLike(ctx context.Context, userID, query string, limit, offset int) ([]*storage.SearchResult, int, error) {
	likeQuery := "%" + escapeLike(query) + "%"

	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.session_id, m.parent_id, m.role, m.content, m.metadata, m.created_at,
		        s.title, COUNT(*) OVER() AS total_count
		 FROM messages m JOIN sessions s ON s.id = m.session_id
		 WHERE m.content LIKE ? ESCAPE '\' AND s.user_id = ?
		 ORDER BY m.created_at DESC
		 LIMIT ? OFFSET ?`,
		likeQuery, userID, limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var results []*storage.SearchResult
	var total int
	for rows.Next() {
		var msg storage.MessageRecord
		var r storage.SearchResult
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.ParentID, &msg.Role, &msg.Content, &msg.Metadata, &msg.CreatedAt, &r.SessionTitle, &total); err != nil {
			return nil, 0, err
		}
		r.Message = &msg
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
	defer tx.Rollback()

	// 1. 验证源会话存在并获取 platform
	var sourceTitle, sourcePlatform string
	err = tx.QueryRowContext(ctx, `SELECT title, platform FROM sessions WHERE id = ?`, sourceSessionID).Scan(&sourceTitle, &sourcePlatform)
	if err != nil {
		return nil, fmt.Errorf("源会话不存在: %w", err)
	}

	// 2. 获取目标消息的 rowid（用于精确范围查询）
	var msgRowID int64
	err = tx.QueryRowContext(ctx,
		`SELECT rowid FROM messages WHERE id = ? AND session_id = ?`,
		messageID, sourceSessionID,
	).Scan(&msgRowID)
	if err != nil {
		return nil, fmt.Errorf("消息不存在: %w", err)
	}

	// 3. 创建分支会话
	now := time.Now()
	newSessionID := "sess-" + idgen.ShortID()
	branchTitle := stringx.Truncate(sourceTitle+" (分支)", 60)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, platform, title, parent_session_id, branch_message_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		newSessionID, userID, sourcePlatform, branchTitle, sourceSessionID, messageID, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("创建分支会话失败: %w", err)
	}

	// 4. 复制源会话中 messageID 之前（含）的所有消息到新会话
	// 使用 rowid <= ? 而非 created_at <= ?，避免精度丢失
	// LIMIT 10000 防止超大会话 fork 导致长时间事务锁
	forkPrefix := "msg-fork-" + idgen.ShortID() + "-"
	_, err = tx.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, parent_id, role, content, metadata, created_at)
		 SELECT ? || rowid, ?, parent_id, role, content, metadata, created_at
		 FROM messages
		 WHERE session_id = ? AND rowid <= ?
		 ORDER BY created_at ASC
		 LIMIT 10000`,
		forkPrefix, newSessionID, sourceSessionID, msgRowID,
	)
	if err != nil {
		return nil, fmt.Errorf("复制消息失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &storage.Session{
		ID:              newSessionID,
		UserID:          userID,
		Platform:        sourcePlatform,
		Title:           branchTitle,
		ParentSessionID: sourceSessionID,
		BranchMessageID: messageID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// ListSessionBranches 列出会话的所有分支
func (s *Store) ListSessionBranches(ctx context.Context, sessionID string) ([]*storage.Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, platform, title, parent_session_id, branch_message_id, created_at, updated_at
		 FROM sessions WHERE parent_session_id = ? ORDER BY created_at DESC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*storage.Session
	for rows.Next() {
		var sess storage.Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Platform, &sess.Title, &sess.ParentSessionID, &sess.BranchMessageID, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, &sess)
	}
	return sessions, rows.Err()
}

// WithTx 在事务中执行操作
func (s *Store) WithTx(ctx context.Context, fn func(storage.Store) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	txStore := &txStore{tx: tx}
	if err := fn(txStore); err != nil {
		return err
	}
	return tx.Commit()
}

// schema 数据库建表 SQL
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id                TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL,
    platform          TEXT NOT NULL DEFAULT 'web',
    title             TEXT NOT NULL DEFAULT '',
    parent_session_id TEXT NOT NULL DEFAULT '',
    branch_message_id TEXT NOT NULL DEFAULT '',
    created_at        DATETIME NOT NULL,
    updated_at        DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at);
CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id);

CREATE TABLE IF NOT EXISTS messages (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    parent_id  TEXT NOT NULL DEFAULT '',
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    metadata   TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at);
CREATE INDEX IF NOT EXISTS idx_messages_parent_id ON messages(parent_id);

CREATE TABLE IF NOT EXISTS cost_records (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    tokens     INTEGER NOT NULL DEFAULT 0,
    cost       REAL NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_cost_records_user_id ON cost_records(user_id);
CREATE INDEX IF NOT EXISTS idx_cost_records_created_at ON cost_records(created_at);
`

