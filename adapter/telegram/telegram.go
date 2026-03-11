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

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/toolkit/util/idgen"
)

const baseURL = "https://api.telegram.org/bot"

// TelegramAdapter Telegram Bot 适配器
type TelegramAdapter struct {
	cfg     config.TelegramConfig
	handler adapter.MessageHandler
	client  *http.Client
	offset  int64 // 长轮询偏移量
	stopped atomic.Bool
}

// New 创建 Telegram 适配器
func New(cfg config.TelegramConfig) *TelegramAdapter {
	return &TelegramAdapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 40 * time.Second},
	}
}

func (a *TelegramAdapter) Name() string              { return "telegram" }
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
	log.Println("Telegram 适配器已停止")
	return nil
}

// Send 发送消息
func (a *TelegramAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	return a.sendMessage(ctx, chatID, reply.Content)
}

// SendStream 流式发送（先发初始消息，后续编辑更新）
func (a *TelegramAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	return a.sendMessage(ctx, chatID, sb.String())
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
			a.offset = int64(update.UpdateID) + 1
			if update.Message != nil && update.Message.Text != "" {
				go a.handleMessage(update.Message)
			}
		}
	}
}

// getUpdates 获取新消息（长轮询）
func (a *TelegramAdapter) getUpdates() ([]tgUpdate, error) {
	url := fmt.Sprintf("%s%s/getUpdates?offset=%d&timeout=30", baseURL, a.cfg.Token, a.offset)

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
		return nil, fmt.Errorf("Telegram API 返回失败")
	}

	return result.Result, nil
}

// handleMessage 处理收到的消息
func (a *TelegramAdapter) handleMessage(tgMsg *tgMessage) {
	if a.handler == nil {
		return
	}

	msg := &adapter.Message{
		ID:        "tg-" + idgen.ShortID(),
		Platform:  adapter.PlatformTelegram,
		ChatID:    fmt.Sprintf("%d", tgMsg.Chat.ID),
		UserID:    fmt.Sprintf("%d", tgMsg.From.ID),
		UserName:  tgMsg.From.Username,
		Content:   tgMsg.Text,
		Timestamp: time.Now(),
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
		_ = a.sendMessage(ctx, msg.ChatID, "处理消息时出现错误，请稍后重试。")
		return
	}

	if err := a.sendMessage(ctx, msg.ChatID, reply.Content); err != nil {
		log.Printf("Telegram: 发送回复失败: %v", err)
	}
}

// sendMessage 发送文本消息
func (a *TelegramAdapter) sendMessage(ctx context.Context, chatID, text string) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"text":       text,
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
		return fmt.Errorf("Telegram API 返回 %d: %s", resp.StatusCode, string(respBody))
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
