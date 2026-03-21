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
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
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
}

// New 创建 Telegram 适配器
func New(cfg config.TelegramConfig) *TelegramAdapter {
	a := &TelegramAdapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 40 * time.Second},
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
func (a *TelegramAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	a.stopped.Store(false)

	go a.pollLoop()
	log.Println("Telegram 适配器已启动（长轮询模式）")
	return nil
}

// Stop 停止长轮询
func (a *TelegramAdapter) Stop(_ context.Context) error {
	a.stopped.Store(true)
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	log.Println("Telegram 适配器已停止")
	return nil
}

// Send 发送消息
func (a *TelegramAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if a.queue == nil {
		return a.sendMessageNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

// SendStream 流式发送（先发初始消息，后续编辑更新模拟打字机）
func (a *TelegramAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	var messageID int64
	lastUpdateLen := 0
	const updateThreshold = 50

	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
		if chunk.Done {
			break
		}

		if messageID == 0 && sb.Len() >= updateThreshold {
			var err error
			messageID, err = a.sendAndGetID(ctx, chatID, sb.String()+"…")
			if err != nil {
				continue
			}
			lastUpdateLen = sb.Len()
			continue
		}

		if messageID != 0 && sb.Len()-lastUpdateLen >= updateThreshold {
			_ = a.editMessage(ctx, chatID, messageID, sb.String()+"…")
			lastUpdateLen = sb.Len()
		}
	}

	finalContent := sb.String()
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
				log.Printf("Telegram: 获取更新失败: %v", err)
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	reply, err := a.handler(ctx, msg)
	if err != nil {
		log.Printf("Telegram: 处理消息失败: %v", err)
		_ = a.Send(ctx, msg.ChatID, &adapter.Reply{Content: "处理消息时出现错误，请稍后重试。"})
		return
	}
	if reply == nil {
		return
	}
	if err := a.Send(ctx, msg.ChatID, reply); err != nil {
		log.Printf("Telegram: 发送回复失败: %v", err)
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
