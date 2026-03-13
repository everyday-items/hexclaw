// Package whatsapp 提供 WhatsApp Business API 适配器
//
// 通过 WhatsApp Cloud API (Meta) 实现消息收发。
// 需要配置 Meta Business 开发者账号和 WhatsApp Business API Token。
//
// 接入方式：
//   - 接收：Webhook 回调（需配置公网 URL）
//   - 发送：REST API 推送
package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
)

// WhatsAppAdapter WhatsApp Business API 适配器
type WhatsAppAdapter struct {
	config  Config
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client
}

// Config WhatsApp 适配器配置
type Config struct {
	Token       string `yaml:"token"`        // WhatsApp Cloud API Token
	PhoneID     string `yaml:"phone_id"`     // 电话号码 ID
	VerifyToken string `yaml:"verify_token"` // Webhook 验证 Token
	WebhookPort int    `yaml:"webhook_port"` // Webhook 监听端口，默认 6063
	BaseURL     string `yaml:"base_url"`     // API 基础 URL
}

// New 创建 WhatsApp 适配器
func New(cfg Config) *WhatsAppAdapter {
	if cfg.WebhookPort == 0 {
		cfg.WebhookPort = 6063
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://graph.facebook.com/v18.0"
	}
	return &WhatsAppAdapter{
		config: cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *WhatsAppAdapter) Name() string                { return "whatsapp" }
func (a *WhatsAppAdapter) Platform() adapter.Platform   { return PlatformWhatsApp }

// PlatformWhatsApp WhatsApp 平台常量
const PlatformWhatsApp adapter.Platform = "whatsapp"

// Start 启动 Webhook 服务器接收消息
func (a *WhatsAppAdapter) Start(ctx context.Context, handler adapter.MessageHandler) error {
	a.handler = handler

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/whatsapp", a.handleWebhook)

	a.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", a.config.WebhookPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("[WhatsApp] Webhook 监听端口 %d", a.config.WebhookPort)

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[WhatsApp] 服务器错误: %v", err)
		}
	}()

	return nil
}

// Stop 停止适配器
func (a *WhatsAppAdapter) Stop(ctx context.Context) error {
	if a.server != nil {
		return a.server.Shutdown(ctx)
	}
	return nil
}

// Send 发送文本消息
func (a *WhatsAppAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                chatID,
		"type":              "text",
		"text": map[string]string{
			"body": reply.Content,
		},
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/%s/messages", a.config.BaseURL, a.config.PhoneID)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.config.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("whatsApp API 返回 %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SendStream 流式发送（WhatsApp 不支持流式，降级为完整发送）
func (a *WhatsAppAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	return a.Send(ctx, chatID, &adapter.Reply{Content: sb.String()})
}

// handleWebhook 处理 Webhook 请求
func (a *WhatsAppAdapter) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Webhook 验证（GET 请求）
	if r.Method == "GET" {
		mode := r.URL.Query().Get("hub.mode")
		token := r.URL.Query().Get("hub.verify_token")
		challenge := r.URL.Query().Get("hub.challenge")

		if mode == "subscribe" && token == a.config.VerifyToken {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, challenge)
			return
		}
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// 消息处理（POST 请求）
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload whatsappWebhook
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// 立即返回 200，避免 WhatsApp 重试
	w.WriteHeader(http.StatusOK)

	// 异步处理消息
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			for _, message := range change.Value.Messages {
				if message.Type != "text" {
					continue
				}
				msg := &adapter.Message{
					ID:        message.ID,
					Platform:  PlatformWhatsApp,
					ChatID:    message.From,
					UserID:    message.From,
					UserName:  a.getContactName(change.Value.Contacts, message.From),
					Content:   message.Text.Body,
					Timestamp: time.Now(),
				}
				go func(m *adapter.Message) {
					if a.handler != nil {
						reply, err := a.handler(context.Background(), m)
						if err != nil {
							log.Printf("[WhatsApp] 处理消息错误: %v", err)
							return
						}
						if reply != nil {
							if err := a.Send(context.Background(), m.ChatID, reply); err != nil {
								log.Printf("[WhatsApp] 发送回复错误: %v", err)
							}
						}
					}
				}(msg)
			}
		}
	}
}

// getContactName 从联系人列表中获取用户名
func (a *WhatsAppAdapter) getContactName(contacts []whatsappContact, waID string) string {
	for _, c := range contacts {
		if c.WaID == waID {
			return c.Profile.Name
		}
	}
	return waID
}

// WhatsApp Webhook 数据结构
type whatsappWebhook struct {
	Entry []whatsappEntry `json:"entry"`
}

type whatsappEntry struct {
	ID      string            `json:"id"`
	Changes []whatsappChange  `json:"changes"`
}

type whatsappChange struct {
	Value whatsappValue `json:"value"`
}

type whatsappValue struct {
	Messages []whatsappMessage `json:"messages"`
	Contacts []whatsappContact `json:"contacts"`
}

type whatsappMessage struct {
	ID   string          `json:"id"`
	From string          `json:"from"`
	Type string          `json:"type"`
	Text whatsappMsgText `json:"text"`
}

type whatsappMsgText struct {
	Body string `json:"body"`
}

type whatsappContact struct {
	WaID    string         `json:"wa_id"`
	Profile whatsappProfile `json:"profile"`
}

type whatsappProfile struct {
	Name string `json:"name"`
}
