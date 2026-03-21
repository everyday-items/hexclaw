// Package line 提供 LINE Messaging API 适配器
//
// 通过 LINE Messaging API 实现消息收发。
// 覆盖日本、台湾、泰国等亚洲市场的即时通讯需求。
//
// 接入方式：
//   - 接收：Webhook 回调（LINE 推送事件）
//   - 发送：Reply API / Push API
package line

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// LineAdapter LINE Messaging API 适配器
type LineAdapter struct {
	config  Config
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client
	queue   *adapter.SendQueue
}

// Config LINE 适配器配置
type Config struct {
	Name          string `yaml:"name"`
	ChannelSecret string `yaml:"channel_secret"` // Channel Secret（用于签名验证）
	ChannelToken  string `yaml:"channel_token"`  // Channel Access Token
	WebhookPort   int    `yaml:"webhook_port"`   // Webhook 端口，默认 6064
}

// PlatformLINE LINE 平台常量
const PlatformLINE adapter.Platform = "line"

// New 创建 LINE 适配器
func New(cfg Config) *LineAdapter {
	if cfg.WebhookPort == 0 {
		cfg.WebhookPort = 6064
	}
	a := &LineAdapter{
		config: cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
	a.queue = adapter.NewPlatformSendQueue(PlatformLINE, a.sendReplyNow)
	return a
}

func (a *LineAdapter) Name() string {
	if a.config.Name != "" {
		return a.config.Name
	}
	return "line"
}
func (a *LineAdapter) Platform() adapter.Platform { return PlatformLINE }

// Attach 注册消息处理器，但不启动独立 HTTP 服务器。
func (a *LineAdapter) Attach(handler adapter.MessageHandler) error {
	a.handler = handler
	return nil
}

// Start 启动 Webhook 服务器
func (a *LineAdapter) Start(ctx context.Context, handler adapter.MessageHandler) error {
	if err := a.Attach(handler); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/line", a.handleWebhook)

	a.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", a.config.WebhookPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("[LINE] Webhook 监听端口 %d", a.config.WebhookPort)

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[LINE] 服务器错误: %v", err)
		}
	}()

	return nil
}

// Stop 停止适配器
func (a *LineAdapter) Stop(ctx context.Context) error {
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	if a.server != nil {
		return a.server.Shutdown(ctx)
	}
	return nil
}

// Handler 返回统一 ingress 使用的处理器。
func (a *LineAdapter) Handler() http.Handler {
	return http.HandlerFunc(a.handleWebhook)
}

// Send 发送消息（Push Message）
func (a *LineAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

func (a *LineAdapter) sendReplyNow(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	if replyToken := reply.Metadata["reply_token"]; replyToken != "" {
		return a.replyMessage(ctx, replyToken, reply.Content)
	}
	payload := map[string]any{
		"to": chatID,
		"messages": []map[string]string{
			{"type": "text", "text": reply.Content},
		},
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.line.me/v2/bot/message/push", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.config.ChannelToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("line API 返回 %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SendStream 流式发送（LINE 不支持流式，降级为完整发送）
func (a *LineAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	return a.Send(ctx, chatID, &adapter.Reply{Content: sb.String()})
}

// replyMessage 使用 Reply API 回复（需要 replyToken）
func (a *LineAdapter) replyMessage(ctx context.Context, replyToken, text string) error {
	payload := map[string]any{
		"replyToken": replyToken,
		"messages": []map[string]string{
			{"type": "text", "text": text},
		},
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.line.me/v2/bot/message/reply", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.config.ChannelToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送回复失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("line reply API 返回 %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// handleWebhook 处理 LINE Webhook 事件
func (a *LineAdapter) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// 签名验证
	if a.config.ChannelSecret != "" {
		signature := r.Header.Get("X-Line-Signature")
		if !a.verifySignature(body, signature) {
			http.Error(w, "Invalid signature", http.StatusForbidden)
			return
		}
	}

	var payload lineWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)

	for _, event := range payload.Events {
		if event.Type != "message" || event.Message.Type != "text" {
			continue
		}

		msg := &adapter.Message{
			ID:         event.Message.ID,
			Platform:   PlatformLINE,
			InstanceID: a.Name(),
			ChatID:     event.Source.UserID,
			UserID:     event.Source.UserID,
			Content:    event.Message.Text,
			Timestamp:  time.UnixMilli(event.Timestamp),
			Metadata: map[string]string{
				"reply_token": event.ReplyToken,
				"source_type": event.Source.Type,
			},
		}

		go func(m *adapter.Message) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			if a.handler != nil {
				reply, err := a.handler(ctx, m)
				if err != nil {
					log.Printf("[LINE] 处理消息错误: %v", err)
					return
				}
				if reply != nil {
					replyToken := m.Metadata["reply_token"]
					if replyToken != "" {
						if reply.Metadata == nil {
							reply.Metadata = make(map[string]string, 1)
						}
						reply.Metadata["reply_token"] = replyToken
					}
					if err := a.Send(ctx, m.ChatID, reply); err != nil {
						log.Printf("[LINE] 发送回复失败: %v", err)
					}
				}
			}
		}(msg)
	}
}

// ValidateConfig validates credentials by fetching bot profile.
func (a *LineAdapter) ValidateConfig(ctx context.Context) error {
	if a.config.ChannelSecret == "" {
		return fmt.Errorf("line channel_secret 未配置")
	}
	if a.config.ChannelToken == "" {
		return fmt.Errorf("line channel_token 未配置")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.line.me/v2/bot/info", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.config.ChannelToken)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("line bot info 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("line token 验证失败 (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// Health 返回适配器健康状态。
func (a *LineAdapter) Health(_ context.Context) error {
	if a.handler == nil {
		return fmt.Errorf("line handler 未附加")
	}
	if a.config.ChannelSecret == "" || a.config.ChannelToken == "" {
		return fmt.Errorf("line channel_secret/channel_token 未配置")
	}
	return nil
}

// verifySignature 验证 LINE Webhook 签名
func (a *LineAdapter) verifySignature(body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(a.config.ChannelSecret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// LINE Webhook 数据结构
type lineWebhook struct {
	Events []lineEvent `json:"events"`
}

type lineEvent struct {
	Type       string      `json:"type"`
	ReplyToken string      `json:"replyToken"`
	Timestamp  int64       `json:"timestamp"`
	Source     lineSource  `json:"source"`
	Message    lineMessage `json:"message"`
}

type lineSource struct {
	Type    string `json:"type"`
	UserID  string `json:"userId"`
	GroupID string `json:"groupId,omitempty"`
}

type lineMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
}
