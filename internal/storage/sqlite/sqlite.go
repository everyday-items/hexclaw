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

	"github.com/everyday-items/hexclaw/internal/storage"
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
	_, err := s.db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("初始化数据库表失败: %w", err)
	}
	return nil
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
		`INSERT INTO sessions (id, user_id, platform, title, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		session.ID, session.UserID, session.Platform, session.Title, session.CreatedAt, session.UpdatedAt,
	)
	return err
}

// GetSession 获取会话
func (s *Store) GetSession(ctx context.Context, id string) (*storage.Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, platform, title, created_at, updated_at FROM sessions WHERE id = ?`, id,
	)
	var sess storage.Session
	if err := row.Scan(&sess.ID, &sess.UserID, &sess.Platform, &sess.Title, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
		return nil, err
	}
	return &sess, nil
}

// ListSessions 列出用户的会话
func (s *Store) ListSessions(ctx context.Context, userID string, limit, offset int) ([]*storage.Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, platform, title, created_at, updated_at FROM sessions WHERE user_id = ? ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*storage.Session
	for rows.Next() {
		var sess storage.Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Platform, &sess.Title, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
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

// SaveMessage 保存消息
func (s *Store) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content, metadata, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.SessionID, msg.Role, msg.Content, msg.Metadata, msg.CreatedAt,
	)
	if err != nil {
		return err
	}

	// 更新会话的 updated_at
	_, err = s.db.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, time.Now(), msg.SessionID,
	)
	return err
}

// ListMessages 获取会话的消息历史
func (s *Store) ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]*storage.MessageRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, role, content, metadata, created_at FROM messages WHERE session_id = ? ORDER BY created_at ASC LIMIT ? OFFSET ?`,
		sessionID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []*storage.MessageRecord
	for rows.Next() {
		var msg storage.MessageRecord
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Role, &msg.Content, &msg.Metadata, &msg.CreatedAt); err != nil {
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
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL,
    platform   TEXT NOT NULL DEFAULT 'web',
    title      TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at);

CREATE TABLE IF NOT EXISTS messages (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    metadata   TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at);

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
