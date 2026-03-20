// Package session 提供会话管理
//
// 会话管理器负责：
//   - 创建/恢复会话
//   - 维护对话上下文窗口（最近 N 轮消息）
//   - 自动生成会话标题
//   - 将消息历史转换为 LLM 可理解的格式
//
// 会话数据持久化到 Storage 层。
package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/lang/stringx"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// Manager 会话管理器
//
// 管理用户会话的生命周期，维护对话上下文。
// 线程安全，支持并发会话操作。
type Manager struct {
	store storage.Store
	cfg   config.MemoryConfig
}

// NewManager 创建会话管理器
func NewManager(store storage.Store, cfg config.MemoryConfig) *Manager {
	return &Manager{
		store: store,
		cfg:   cfg,
	}
}

// GetOrCreate 获取或创建会话
//
// 会话 scope = (UserID, Platform, InstanceID, ChatID)，确保多实例不串上下文。
// 如果 sessionID 不为空且存在，返回已有会话。
// 如果 sessionID 为空或不存在，创建新会话。
func (m *Manager) GetOrCreate(ctx context.Context, msg *adapter.Message) (*storage.Session, error) {
	// 如果消息已有 SessionID，尝试恢复
	if msg.SessionID != "" {
		sess, err := m.store.GetSession(ctx, msg.SessionID)
		if err == nil {
			return sess, nil
		}
	}

	// 对 IM/WebSocket 等无显式 SessionID 的场景，按 scope 复用最近会话。
	if msg.ChatID != "" || msg.InstanceID != "" {
		sess, err := m.store.FindSessionByScope(ctx, msg.UserID, string(msg.Platform), msg.InstanceID, msg.ChatID)
		if err == nil {
			return sess, nil
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("按 scope 查找会话失败: %w", err)
		}
	}

	// 创建新会话，scope 包含 InstanceID 以隔离多实例
	sessionID := msg.SessionID
	if sessionID == "" {
		sessionID = "sess-" + idgen.ShortID()
	}

	sess := &storage.Session{
		ID:         sessionID,
		UserID:     msg.UserID,
		Platform:   string(msg.Platform),
		InstanceID: msg.InstanceID,
		ChatID:     msg.ChatID,
		Title:      generateTitle(msg.Content),
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	if err := m.store.CreateSession(ctx, sess); err != nil {
		return nil, fmt.Errorf("创建会话失败: %w", err)
	}

	return sess, nil
}

// SaveUserMessage 保存用户消息到会话
func (m *Manager) SaveUserMessage(ctx context.Context, sessionID, content string) error {
	msg := &storage.MessageRecord{
		ID:        "msg-" + idgen.ShortID(),
		SessionID: sessionID,
		Role:      "user",
		Content:   content,
		Metadata:  "{}",
		CreatedAt: time.Now(),
	}
	return m.store.SaveMessage(ctx, msg)
}

// SaveAssistantMessage 保存助手回复到会话
func (m *Manager) SaveAssistantMessage(ctx context.Context, sessionID, content string) error {
	msg := &storage.MessageRecord{
		ID:        "msg-" + idgen.ShortID(),
		SessionID: sessionID,
		Role:      "assistant",
		Content:   content,
		Metadata:  "{}",
		CreatedAt: time.Now(),
	}
	return m.store.SaveMessage(ctx, msg)
}

// BuildContext 构建对话上下文
//
// 从存储中加载最近 N 轮消息，转换为 hexagon.Message（即 llm.Message）格式。
// 消息按时间正序排列，最新消息在最后。
// 如果消息数超过 maxTurns，只返回最近的 maxTurns 条。
func (m *Manager) BuildContext(ctx context.Context, sessionID string) ([]hexagon.Message, error) {
	maxTurns := m.cfg.Conversation.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 50
	}

	records, err := m.store.ListMessages(ctx, sessionID, maxTurns, 0)
	if err != nil {
		return nil, fmt.Errorf("加载消息历史失败: %w", err)
	}

	messages := make([]hexagon.Message, 0, len(records))
	for _, r := range records {
		messages = append(messages, hexagon.Message{
			Role:    toRole(r.Role),
			Content: r.Content,
		})
	}

	return messages, nil
}

// ListSessions 列出用户的会话
func (m *Manager) ListSessions(ctx context.Context, userID string, limit, offset int) ([]*storage.Session, error) {
	return m.store.ListSessions(ctx, userID, limit, offset)
}

// DeleteSession 删除会话
func (m *Manager) DeleteSession(ctx context.Context, sessionID string) error {
	return m.store.DeleteSession(ctx, sessionID)
}

// CleanupOldSessions 清理超过指定天数未活跃的会话
func (m *Manager) CleanupOldSessions(ctx context.Context, olderThanDays int) (int64, error) {
	return m.store.CleanupOldSessions(ctx, olderThanDays)
}

// toRole 将字符串角色转换为 hexagon.LLMRole
func toRole(role string) hexagon.LLMRole {
	switch role {
	case "system":
		return hexagon.RoleSystem
	case "user":
		return hexagon.RoleUser
	case "assistant":
		return hexagon.RoleAssistant
	case "tool":
		return hexagon.RoleTool
	default:
		return hexagon.RoleUser
	}
}

// generateTitle 从消息内容生成会话标题
//
// 取消息前 30 个字符作为标题。
// 后续可接入 LLM 自动生成更好的标题。
func generateTitle(content string) string {
	return stringx.Truncate(content, 30)
}
