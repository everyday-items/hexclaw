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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
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
			if sess.UserID != msg.UserID {
				return nil, fmt.Errorf("会话 %s 不属于当前用户", msg.SessionID)
			}
			return sess, nil
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("加载会话失败: %w", err)
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
		Title:      generateTitleForMessage(msg),
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	if err := m.store.CreateSession(ctx, sess); err != nil {
		return nil, fmt.Errorf("创建会话失败: %w", err)
	}

	return sess, nil
}

type messageMetadata struct {
	Attachments []adapter.Attachment `json:"attachments,omitempty"`
	// Documents 文档卡片（name/mime/size），前端随请求 metadata["documents"] 上送的 ChatDocumentRef[] JSON。
	// 此前只存前端本地、重载即丢 → 文档退化为纯文本；现一并落库（BUG-20260626）。
	Documents json.RawMessage `json:"documents,omitempty"`
}

// SaveUserMessage 保存用户消息到会话。
func (m *Manager) SaveUserMessage(ctx context.Context, sessionID string, msg *adapter.Message) error {
	metadata, err := encodeMessageMetadata(msg.Attachments, msg.Metadata["documents"])
	if err != nil {
		return fmt.Errorf("编码消息元数据失败: %w", err)
	}

	record := &storage.MessageRecord{
		ID:        userMessageID(msg.Metadata),
		SessionID: sessionID,
		Role:      "user",
		Content:   msg.Content,
		// BUG-20260626：图片附件 base64 存独立列 attachments（不受 metadata 64KB 截断）；
		// 读取时由 scanMessage 合并回 metadata，前端继续读 metadata.attachments 渲染。
		Attachments: metadata,
		RequestID:   requestIDFromMetadata(msg.Metadata),
		CreatedAt:   time.Now(),
	}
	if err := m.saveMessage(ctx, record); err != nil {
		return err
	}

	// 首条用户消息时自动更新默认标题（同步、无 LLM 依赖）
	m.autoUpdateDefaultTitle(ctx, sessionID, msg.Content)
	return nil
}

// SaveAssistantMessage 保存助手回复到会话
func (m *Manager) SaveAssistantMessage(ctx context.Context, sessionID, content string) error {
	_, err := m.SaveAssistantMessageRecord(ctx, sessionID, content)
	return err
}

// SaveAssistantMessageRecord 保存助手回复并返回消息记录。
func (m *Manager) SaveAssistantMessageRecord(ctx context.Context, sessionID, content string) (*storage.MessageRecord, error) {
	return m.SaveAssistantMessageWithMeta(ctx, sessionID, content, "")
}

// AssistantMeta 助手消息的完整元数据。
type AssistantMeta struct {
	Reasoning        string
	ThinkingDuration int
	Provider         string
	Model            string
	AgentName        string
	RequestID        string
}

// SaveAssistantMessageWithMeta 保存助手回复（含 reasoning 等元数据）并返回消息记录。
func (m *Manager) SaveAssistantMessageWithMeta(ctx context.Context, sessionID, content, reasoning string) (*storage.MessageRecord, error) {
	return m.SaveAssistantMessageWithMetaAndRequestID(ctx, sessionID, content, reasoning, "")
}

// SaveAssistantMessageWithMetaAndRequestID 保存助手回复（含 reasoning 和 request_id）并返回消息记录。
// Deprecated: 新代码请使用 SaveAssistantReply。
func (m *Manager) SaveAssistantMessageWithMetaAndRequestID(ctx context.Context, sessionID, content, reasoning, requestID string) (*storage.MessageRecord, error) {
	return m.SaveAssistantReply(ctx, sessionID, content, AssistantMeta{
		Reasoning: reasoning,
		RequestID: requestID,
	})
}

// SaveAssistantMessageFull 保存助手回复（含 reasoning、thinking_duration 和 request_id）。
// Deprecated: 新代码请使用 SaveAssistantReply。
func (m *Manager) SaveAssistantMessageFull(ctx context.Context, sessionID, content, reasoning string, thinkingDuration int, requestID string) (*storage.MessageRecord, error) {
	return m.SaveAssistantReply(ctx, sessionID, content, AssistantMeta{
		Reasoning:        reasoning,
		ThinkingDuration: thinkingDuration,
		RequestID:        requestID,
	})
}

// SaveAssistantReply 统一的助手消息保存方法。
// 所有元数据（reasoning、duration、provider、model、agent）一次性写入 meta 字段。
func (m *Manager) SaveAssistantReply(ctx context.Context, sessionID, content string, am AssistantMeta) (*storage.MessageRecord, error) {
	metaMap := map[string]any{}
	if am.Reasoning != "" {
		metaMap["reasoning"] = am.Reasoning
	}
	if am.ThinkingDuration > 0 {
		metaMap["thinking_duration"] = am.ThinkingDuration
	}
	if am.Provider != "" {
		metaMap["provider"] = am.Provider
	}
	if am.Model != "" {
		metaMap["model"] = am.Model
	}
	if am.AgentName != "" {
		metaMap["agent_name"] = am.AgentName
	}
	meta := "{}"
	if len(metaMap) > 0 {
		if b, err := json.Marshal(metaMap); err == nil {
			meta = string(b)
		}
	}
	msg := &storage.MessageRecord{
		ID:        "msg-" + idgen.ShortID(),
		SessionID: sessionID,
		Role:      "assistant",
		Content:   content,
		Metadata:  meta,
		RequestID: am.RequestID,
		CreatedAt: time.Now(),
	}
	if err := m.saveMessage(ctx, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// saveMessage 持久化一条消息，并对 SQLite 写写冲突（SQLITE_BUSY/517）做有限退避重试。
//
// 并发写场景下底层存储可能瞬时返回 "database is locked (517)" 之类的可重试错误
// （busy_timeout 不覆盖 BUSY_SNAPSHOT 写写冲突）。直接上抛会导致用户/助手消息
// 被静默丢弃、上下文残缺。这里统一经 sqliteutil.RetryOnBusy 重试，让瞬时冲突在
// 对方提交后自动落库；非 BUSY 错误立即原样返回，不改变错误语义。
//
// 重试用的写入闭包是幂等的：消息 ID 在上层已生成且固定，重复 INSERT 同一行
// 在首次成功后不会发生（成功即返回 nil，不再重试）。
func (m *Manager) saveMessage(ctx context.Context, record *storage.MessageRecord) error {
	return sqliteutil.RetryOnBusy(ctx, func() error {
		return m.store.SaveMessage(ctx, record)
	})
}

func requestIDFromMetadata(metadata map[string]string) string {
	if metadata == nil {
		return ""
	}
	return metadata["request_id"]
}

// userMessageID 选定用户消息的存储主键：优先用前端 request_id（前端唯一持有的消息句柄，
// 编辑/重试时按它 DELETE /api/v1/messages/{id}）；缺省才回退生成。二者一致后端 delete 才命中，
// 否则 404 → 前端报 "删除消息失败"（BUG-20260626）。
func userMessageID(metadata map[string]string) string {
	if rid := requestIDFromMetadata(metadata); rid != "" {
		return rid
	}
	return "msg-" + idgen.ShortID()
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
		if r.Role == "user" {
			attachments := decodeMessageAttachments(r.Metadata)
			messages = append(messages, adapter.BuildUserMessage(r.Content, attachments))
			continue
		}
		messages = append(messages, hexagon.Message{
			Role:    toRole(r.Role),
			Content: r.Content,
		})
	}

	// Token 预算兜底：按字符数近似估算，从最新消息往回保留
	// 预留 40% 给 system prompt + knowledge context + 新回复
	tokenBudget := m.cfg.Conversation.TokenBudget
	if tokenBudget <= 0 {
		tokenBudget = 60000 // 默认 6 万 token（适配 128K 上下文窗口的 ~47%）
	}
	// 近似：1 token ≈ 3 个字符（中文偏保守）
	charBudget := tokenBudget * 3
	totalChars := 0
	cutIdx := 0
	for i := len(messages) - 1; i >= 0; i-- {
		totalChars += len(messages[i].Content)
		if totalChars > charBudget {
			cutIdx = i + 1
			break
		}
	}
	if cutIdx > 0 && cutIdx < len(messages) {
		// Fix 12: 确保截断后以 user 消息开头，避免孤立的 assistant 消息
		for cutIdx < len(messages) && messages[cutIdx].Role != "user" {
			cutIdx++
		}
		if cutIdx < len(messages) {
			messages = messages[cutIdx:]
		}
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

// autoUpdateDefaultTitle 当 session 标题为默认值时，用消息内容同步更新。
func (m *Manager) autoUpdateDefaultTitle(ctx context.Context, sessionID, content string) {
	sess, err := m.store.GetSession(ctx, sessionID)
	if err != nil || sess == nil {
		return
	}
	title := strings.ToLower(strings.TrimSpace(sess.Title))
	if title != "" && title != "new chat" && title != "新对话" && title != "新会话" {
		return // 已有自定义标题
	}
	newTitle := generateTitle(content)
	if newTitle == "" {
		return
	}
	sess.Title = newTitle
	_ = m.store.UpdateSession(ctx, sess)
}

// generateTitle 从消息内容生成会话标题
//
// 取消息前 30 个字符作为标题。
// 后续可接入 LLM 自动生成更好的标题。
func generateTitle(content string) string {
	return stringx.Truncate(content, 30)
}

func generateTitleForMessage(msg *adapter.Message) string {
	if title := generateTitle(msg.Content); title != "" {
		return title
	}
	if len(msg.Attachments) > 0 {
		return "图片消息"
	}
	return ""
}

// encodeMessageMetadata 组装用户消息富内容 blob（图片附件 + 文档卡片）。
// documentsJSON 来自前端随请求 metadata["documents"] 上送的 ChatDocumentRef[] JSON（可空）。
func encodeMessageMetadata(attachments []adapter.Attachment, documentsJSON string) (string, error) {
	mm := messageMetadata{Attachments: attachments}
	if documentsJSON != "" && documentsJSON != "null" && json.Valid([]byte(documentsJSON)) {
		mm.Documents = json.RawMessage(documentsJSON)
	}
	if len(mm.Attachments) == 0 && mm.Documents == nil {
		return "{}", nil
	}
	data, err := json.Marshal(mm)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func decodeMessageAttachments(raw string) []adapter.Attachment {
	if raw == "" || raw == "{}" {
		return nil
	}

	var metadata messageMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return nil
	}
	return metadata.Attachments
}
