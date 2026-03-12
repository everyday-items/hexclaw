package session

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/everyday-items/hexagon"

	"github.com/everyday-items/hexclaw/storage"
	"github.com/everyday-items/toolkit/util/idgen"
)

// CompactionConfig 上下文压缩配置
type CompactionConfig struct {
	MaxMessages   int    // 触发压缩的消息数阈值，默认 50
	KeepRecent    int    // 保留最近 N 条消息完整，默认 10
	SummaryPrompt string // 摘要生成提示词（可自定义）
}

// DefaultCompactionConfig 返回默认压缩配置
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		MaxMessages: 50,
		KeepRecent:  10,
	}
}

// Compactor 上下文压缩器
//
// 当会话消息数超过阈值时，使用 LLM 将旧消息摘要为
// 简短的上下文摘要，防止 token 爆炸。
//
// 压缩流程：
//  1. 检测消息数是否超过阈值
//  2. 取出需要压缩的旧消息（保留最近 N 条完整）
//  3. 使用 LLM 生成摘要
//  4. 删除旧消息，插入摘要消息
//
// 对标 OpenClaw 的 Context Compaction 机制。
type Compactor struct {
	store  storage.Store
	config CompactionConfig
}

// NewCompactor 创建上下文压缩器
func NewCompactor(store storage.Store, cfg CompactionConfig) *Compactor {
	if cfg.MaxMessages <= 0 {
		cfg.MaxMessages = 50
	}
	if cfg.KeepRecent <= 0 {
		cfg.KeepRecent = 10
	}
	return &Compactor{
		store:  store,
		config: cfg,
	}
}

// NeedsCompaction 检查会话是否需要压缩
func (c *Compactor) NeedsCompaction(ctx context.Context, sessionID string) (bool, error) {
	// 使用 MaxMessages+1 作为 limit 来判断是否超过阈值，
	// 避免 limit=0 时 SQLite LIMIT 0 返回空结果的问题
	msgs, err := c.store.ListMessages(ctx, sessionID, c.config.MaxMessages+1, 0)
	if err != nil {
		return false, err
	}
	return len(msgs) > c.config.MaxMessages, nil
}

// Compact 执行上下文压缩
//
// provider 用于调用 LLM 生成摘要。
// 返回压缩后删除的消息数。
func (c *Compactor) Compact(ctx context.Context, sessionID string, provider hexagon.Provider) (int, error) {
	// 获取所有消息（使用足够大的 limit）
	msgs, err := c.store.ListMessages(ctx, sessionID, 100000, 0)
	if err != nil {
		return 0, fmt.Errorf("获取消息列表失败: %w", err)
	}

	if len(msgs) <= c.config.MaxMessages {
		return 0, nil // 不需要压缩
	}

	// 分割：需要压缩的旧消息 + 保留完整的新消息
	keepCount := c.config.KeepRecent
	if keepCount > len(msgs) {
		keepCount = len(msgs)
	}

	oldMsgs := msgs[:len(msgs)-keepCount]
	if len(oldMsgs) == 0 {
		return 0, nil
	}

	// 生成摘要
	summary, err := c.generateSummary(ctx, oldMsgs, provider)
	if err != nil {
		return 0, fmt.Errorf("生成摘要失败: %w", err)
	}

	// 在事务中执行：删除旧消息 + 插入摘要
	err = c.store.WithTx(ctx, func(txStore storage.Store) error {
		// 删除旧消息
		for _, msg := range oldMsgs {
			if err := txStore.DeleteMessage(ctx, msg.ID); err != nil {
				return fmt.Errorf("删除旧消息失败: %w", err)
			}
		}

		// 插入摘要消息（使用唯一 ID，避免多次压缩主键冲突）
		summaryMsg := &storage.MessageRecord{
			ID:        "summary-" + idgen.ShortID(),
			SessionID: sessionID,
			Role:      "system",
			Content:   "[上下文摘要] " + summary,
			CreatedAt: time.Now(),
		}
		return txStore.SaveMessage(ctx, summaryMsg)
	})
	if err != nil {
		return 0, fmt.Errorf("保存压缩结果失败: %w", err)
	}

	compactedCount := len(oldMsgs)
	log.Printf("上下文压缩完成: session=%s 压缩了 %d 条消息", sessionID, compactedCount)
	return compactedCount, nil
}

// CompactIfNeeded 如果需要则执行压缩
func (c *Compactor) CompactIfNeeded(ctx context.Context, sessionID string, provider hexagon.Provider) error {
	needs, err := c.NeedsCompaction(ctx, sessionID)
	if err != nil || !needs {
		return err
	}

	_, err = c.Compact(ctx, sessionID, provider)
	return err
}

// generateSummary 使用 LLM 生成对话摘要
func (c *Compactor) generateSummary(ctx context.Context, msgs []*storage.MessageRecord, provider hexagon.Provider) (string, error) {
	// 构建摘要请求
	var sb strings.Builder
	sb.WriteString("请将以下对话历史压缩为简洁的摘要。保留关键信息、用户偏好、重要决定和结论。\n\n")
	sb.WriteString("--- 对话历史 ---\n")

	for _, msg := range msgs {
		role := msg.Role
		switch role {
		case "user":
			role = "用户"
		case "assistant":
			role = "助手"
		case "system":
			role = "系统"
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", role, truncate(msg.Content, 500)))
	}

	sb.WriteString("\n--- 请求 ---\n")
	sb.WriteString("请输出一段简洁的摘要（200字以内），概括以上对话的关键信息。")

	prompt := sb.String()
	if c.config.SummaryPrompt != "" {
		prompt = c.config.SummaryPrompt + "\n\n" + prompt
	}

	// 调用 LLM
	resp, err := provider.Complete(ctx, hexagon.CompletionRequest{
		Messages: []hexagon.Message{
			{Role: "user", Content: prompt},
		},
		MaxTokens: 500,
	})
	if err != nil {
		return "", err
	}

	if resp.Content == "" {
		return "（摘要生成失败）", nil
	}

	return resp.Content, nil
}

// truncate 截断文本到指定长度
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
