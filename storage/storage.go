// Package storage 提供数据持久化层
//
// 支持两种存储驱动：
//   - SQLite: 默认驱动，零配置，适合个人使用
//   - PostgreSQL: 企业级驱动，适合高并发场景
//
// 存储层负责会话、消息历史、用户信息、成本记录等数据的持久化。
// 所有操作支持事务 (WithTx)。
package storage

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 表示请求的资源不存在
var ErrNotFound = errors.New("not found")

// Session 会话记录
type Session struct {
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	Platform        string    `json:"platform"`
	InstanceID      string    `json:"instance_id"`
	ChatID          string    `json:"chat_id"`
	Title           string    `json:"title"`
	ParentSessionID string    `json:"parent_session_id"`
	BranchMessageID string    `json:"branch_message_id"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MessageRecord 消息记录
type MessageRecord struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	ParentID  string    `json:"parent_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Metadata  string    `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
}

// SearchResult 消息搜索结果
type SearchResult struct {
	Message      *MessageRecord `json:"message"`
	SessionTitle string         `json:"session_title"`
	Rank         float64        `json:"rank"`
}

// CostRecord 成本记录
type CostRecord struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Tokens    int       `json:"tokens"`
	Cost      float64   `json:"cost"`
	CreatedAt time.Time `json:"created_at"`
}

// Store 存储接口
//
// 定义数据层的核心操作，由具体驱动（SQLite/PostgreSQL）实现。
// 所有方法都接受 context.Context，支持超时和取消。
type Store interface {
	// --- 生命周期 ---

	// Init 初始化存储（创建表、执行迁移等）
	Init(ctx context.Context) error

	// Close 关闭存储连接
	Close() error

	// --- 会话管理 ---

	// CreateSession 创建新会话
	CreateSession(ctx context.Context, session *Session) error

	// GetSession 获取会话
	GetSession(ctx context.Context, id string) (*Session, error)

	// FindSessionByScope 按会话作用域查找最近活跃会话
	FindSessionByScope(ctx context.Context, userID, platform, instanceID, chatID string) (*Session, error)

	// ListSessions 列出用户的会话（按更新时间倒序）
	ListSessions(ctx context.Context, userID string, limit, offset int) ([]*Session, error)

	// DeleteSession 删除会话及其所有消息
	DeleteSession(ctx context.Context, id string) error

	// CleanupOldSessions 删除超过指定天数未活跃的会话及其消息
	CleanupOldSessions(ctx context.Context, olderThanDays int) (int64, error)

	// --- 消息管理 ---

	// SaveMessage 保存消息
	SaveMessage(ctx context.Context, msg *MessageRecord) error

	// DeleteMessage 删除单条消息
	DeleteMessage(ctx context.Context, id string) error

	// ListMessages 获取会话的消息历史（按创建时间正序）
	ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]*MessageRecord, error)

	// UpdateSession 更新会话信息（标题等）
	UpdateSession(ctx context.Context, session *Session) error

	// --- 消息搜索 ---

	// SearchMessages 全文搜索消息内容
	// 返回匹配的消息列表和总数
	SearchMessages(ctx context.Context, userID, query string, limit, offset int) ([]*SearchResult, int, error)

	// --- 对话分支 ---

	// ForkSession 从指定消息处创建分支会话
	// 复制源会话中 messageID 之前（含）的所有消息到新会话
	ForkSession(ctx context.Context, sourceSessionID, messageID, userID string) (*Session, error)

	// ListSessionBranches 列出会话的所有分支
	ListSessionBranches(ctx context.Context, sessionID string) ([]*Session, error)

	// --- 成本管理 ---

	// SaveCost 记录成本
	SaveCost(ctx context.Context, record *CostRecord) error

	// GetUserCost 获取用户在指定时间范围内的总成本
	GetUserCost(ctx context.Context, userID string, since time.Time) (float64, error)

	// GetGlobalCost 获取全局在指定时间范围内的总成本
	GetGlobalCost(ctx context.Context, since time.Time) (float64, error)

	// --- 事务 ---

	// WithTx 在事务中执行操作
	// fn 返回 error 时自动回滚，否则自动提交
	WithTx(ctx context.Context, fn func(Store) error) error
}
