// Package telegram 提供 Telegram Bot 适配器
//
// 通过长轮询（Long Polling）接收 Telegram 消息，
// 通过 Bot API 发送回复，支持流式更新（打字机效果）。
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hexagon-codes/toolkit/util/logger"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/trace"
	"github.com/hexagon-codes/toolkit/net/httpx"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const baseURL = "https://api.telegram.org/bot"

// TelegramAdapter Telegram Bot 适配器
type TelegramAdapter struct {
	cfg     config.TelegramConfig
	handler adapter.MessageHandler
	client  *http.Client
	queue   *adapter.SendQueue
	offset  atomic.Int64 // 长轮询偏移量
	stopped atomic.Bool
	// v0.4.0 E5：Start 收到的 ctx，仅作 logger/trace 链路源头使用；
	// 实际异步 handler 处理用 trace.Detach(baseCtx) 派生新 ctx，避免被父 cancel 杀死。
	baseCtx context.Context
}

// New 创建 Telegram 适配器
func New(cfg config.TelegramConfig) *TelegramAdapter {
	a := &TelegramAdapter{
		cfg:    cfg,
		client: httpx.RawClient(httpx.WithRawTimeout(40 * time.Second)),
	}
	a.queue = adapter.NewPlatformSendQueue(adapter.PlatformTelegram, a.sendMessageNow)
	return a
}

func (a *TelegramAdapter) Name() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return "telegram"
}
func (a *TelegramAdapter) Platform() adapter.Platform { return adapter.PlatformTelegram }

// Start 启动长轮询
func (a *TelegramAdapter) Start(ctx context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	a.stopped.Store(false)
	// v0.4.0 E5：保留 Start ctx，handleMessage 用 Detach 派生异步处理 ctx
	if ctx == nil {
		ctx = context.Background()
	}
	a.baseCtx = ctx

	go a.pollLoop()
	logger.Info("Telegram 适配器已启动（长轮询模式）")
	return nil
}

// Stop 停止长轮询
func (a *TelegramAdapter) Stop(_ context.Context) error {
	a.stopped.Store(true)
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	logger.Info("Telegram 适配器已停止")
	return nil
}

// Send 发送消息
//
// v0.4.0 F6：reply.Interactive 非空时，flag interactive.render.v1 OFF 自动追加文本
// fallback，让按钮/选项/审批/卡片在 Telegram 基础可用；flag ON 时 no-op，等待后续
// Inline Keyboard 原生 renderer。
func (a *TelegramAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	adapter.MaybeApplyTextFallback(ctx, reply)
	if a.queue == nil {
		return a.sendMessageNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

// SendStream 流式发送（先发初始消息，后续编辑更新模拟打字机）
//
// v0.4.0 E1：替换原硬编码 50 字阈值为 adapter.StreamEditLimiter，
// 支持字符阈值 + 时间间隔 + 连续 3 次失败自适应降级（封顶 maxInterval）。
// Telegram editMessageText 限制约 30 次/秒/聊天，限流器避免触发 429。
func (a *TelegramAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	var messageID int64
	limiter := adapter.NewStreamEditLimiter()

	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
		if chunk.Done {
			break
		}

		if messageID == 0 {
			if !limiter.ShouldEmit(sb.Len()) {
				continue
			}
			id, err := a.sendAndGetID(ctx, chatID, adapter.StripThinking(sb.String())+"…")
			if err != nil {
				if degraded := limiter.Failed(); degraded {
					logger.Warn("[telegram] 流式限流降级", "delay", limiter.CurrentDelay())
				}
				continue
			}
			messageID = id
			limiter.Emitted(sb.Len())
			continue
		}

		if limiter.ShouldEmit(sb.Len()) {
			if err := a.editMessage(ctx, chatID, messageID, adapter.StripThinking(sb.String())+"…"); err != nil {
				logger.Error("[telegram] 编辑流式消息失败", "error", err)
				if degraded := limiter.Failed(); degraded {
					logger.Warn("[telegram] 流式限流降级", "delay", limiter.CurrentDelay())
				}
			} else {
				limiter.Emitted(sb.Len())
			}
		}
	}

	// v0.4.0 E2：最终发送前 strip thinking 防泄漏给家长
	finalContent := adapter.StripThinking(sb.String())
	if finalContent == "" {
		return nil
	}
	if messageID != 0 {
		return a.editMessage(ctx, chatID, messageID, finalContent)
	}
	return a.Send(ctx, chatID, &adapter.Reply{Content: finalContent})
}

// sendAndGetID 发送消息并返回 message_id
func (a *TelegramAdapter) sendAndGetID(ctx context.Context, chatID, text string) (int64, error) {
	body, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "Markdown",
	})

	url := fmt.Sprintf("%s%s/sendMessage", baseURL, a.cfg.Token)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Result.MessageID, nil
}

// editMessage 编辑已发送的消息
func (a *TelegramAdapter) editMessage(ctx context.Context, chatID string, messageID int64, text string) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "Markdown",
	})

	url := fmt.Sprintf("%s%s/editMessageText", baseURL, a.cfg.Token)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// pollLoop 长轮询循环
func (a *TelegramAdapter) pollLoop() {
	for !a.stopped.Load() {
		updates, err := a.getUpdates()
		if err != nil {
			if !a.stopped.Load() {
				logger.Error("Telegram: 获取更新失败", "error", err)
				time.Sleep(3 * time.Second)
			}
			continue
		}

		for _, update := range updates {
			a.offset.Store(int64(update.UpdateID) + 1)
			if update.Message != nil && update.Message.Text != "" {
				go a.handleMessage(update.Message)
			}
		}
	}
}

// getUpdates 获取新消息（长轮询）
func (a *TelegramAdapter) getUpdates() ([]tgUpdate, error) {
	url := fmt.Sprintf("%s%s/getUpdates?offset=%d&timeout=30", baseURL, a.cfg.Token, a.offset.Load())

	resp, err := a.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram API 返回失败")
	}

	return result.Result, nil
}

// handleMessage 处理收到的消息
//
// v0.4.0 E5：所有派生 ctx 一律走 trace.Detach(baseCtx)，保留 logger/Values
// 链路，又脱离 Start ctx 的 cancel（消息处理可能比 Start 提前结束更长）。
func (a *TelegramAdapter) handleMessage(tgMsg *tgMessage) {
	if a.handler == nil {
		return
	}

	msg := &adapter.Message{
		ID:         "tg-" + idgen.ShortID(),
		Platform:   adapter.PlatformTelegram,
		InstanceID: a.Name(),
		ChatID:     fmt.Sprintf("%d", tgMsg.Chat.ID),
		UserID:     fmt.Sprintf("%d", tgMsg.From.ID),
		UserName:   tgMsg.From.Username,
		Content:    tgMsg.Text,
		Timestamp:  time.Now(),
		Metadata: map[string]string{
			"message_id": fmt.Sprintf("%d", tgMsg.MessageID),
			"chat_type":  tgMsg.Chat.Type,
		},
	}

	base := a.baseCtx
	if base == nil {
		base = context.Background()
	}

	ctx, cancel := context.WithTimeout(trace.Detach(base), 2*time.Minute)
	defer cancel()

	reply, err := a.handler(ctx, msg)
	if err != nil {
		logger.Error("Telegram: 处理消息失败", "error", err)
		errCtx, errCancel := context.WithTimeout(trace.Detach(base), 10*time.Second)
		defer errCancel()
		_ = a.Send(errCtx, msg.ChatID, &adapter.Reply{Content: "处理消息时出现错误，请稍后重试。"})
		return
	}
	if reply == nil {
		return
	}
	sendCtx, sendCancel := context.WithTimeout(trace.Detach(base), 30*time.Second)
	defer sendCancel()
	if err := a.Send(sendCtx, msg.ChatID, reply); err != nil {
		logger.Error("Telegram: 发送回复失败", "error", err)
	}
}

// ValidateConfig validates credentials by calling getMe.
func (a *TelegramAdapter) ValidateConfig(ctx context.Context) error {
	if a.cfg.Token == "" {
		return fmt.Errorf("telegram token 未配置")
	}
	url := fmt.Sprintf("%s%s/getMe", baseURL, a.cfg.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram getMe 请求失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("解析 telegram getMe 响应失败: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("telegram token 验证失败: %s", result.Description)
	}
	return nil
}

// Health 返回适配器健康状态。
func (a *TelegramAdapter) Health(_ context.Context) error {
	if a.cfg.Token == "" {
		return fmt.Errorf("telegram token 不能为空")
	}
	if a.stopped.Load() {
		return fmt.Errorf("telegram adapter stopped")
	}
	return nil
}

// sendMessage 保持旧测试兼容。
func (a *TelegramAdapter) sendMessage(ctx context.Context, chatID, text string) error {
	return a.sendMessageNow(ctx, chatID, &adapter.Reply{Content: text})
}

// sendMessageNow 立即发送文本消息。
func (a *TelegramAdapter) sendMessageNow(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"text":       reply.Content,
		"parse_mode": "Markdown",
	})

	url := fmt.Sprintf("%s%s/sendMessage", baseURL, a.cfg.Token)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API 返回 %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// Telegram API 数据结构
type tgUpdate struct {
	UpdateID int        `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	MessageID int    `json:"message_id"`
	From      tgUser `json:"from"`
	Chat      tgChat `json:"chat"`
	Text      string `json:"text"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
