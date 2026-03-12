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
	ID        string    // 会话 ID
	UserID    string    // 用户 ID
	Platform  string    // 平台
	Title     string    // 会话标题（自动生成或用户设置）
	CreatedAt time.Time // 创建时间
	UpdatedAt time.Time // 最后更新时间
}

// MessageRecord 消息记录
type MessageRecord struct {
	ID        string    // 消息 ID
	SessionID string    // 所属会话 ID
	Role      string    // 角色: user / assistant / system / tool
	Content   string    // 消息内容
	Metadata  string    // JSON 格式的元数据
	CreatedAt time.Time // 创建时间
}

// CostRecord 成本记录
type CostRecord struct {
	ID        string    // 记录 ID
	UserID    string    // 用户 ID
	Provider  string    // LLM Provider 名称
	Model     string    // 模型名称
	Tokens    int       // 消耗 Token 数
	Cost      float64   // 费用（美元）
	CreatedAt time.Time // 创建时间
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

	// ListSessions 列出用户的会话（按更新时间倒序）
	ListSessions(ctx context.Context, userID string, limit, offset int) ([]*Session, error)

	// DeleteSession 删除会话及其所有消息
	DeleteSession(ctx context.Context, id string) error

	// --- 消息管理 ---

	// SaveMessage 保存消息
	SaveMessage(ctx context.Context, msg *MessageRecord) error

	// DeleteMessage 删除单条消息
	DeleteMessage(ctx context.Context, id string) error

	// ListMessages 获取会话的消息历史（按创建时间正序）
	ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]*MessageRecord, error)

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
