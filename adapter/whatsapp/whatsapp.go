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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/hexagon-codes/toolkit/util/logger"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/trace"
	"github.com/hexagon-codes/toolkit/net/httpx"
)

// WhatsAppAdapter WhatsApp Business API 适配器
type WhatsAppAdapter struct {
	config  Config
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client
	queue   *adapter.SendQueue
}

// Config WhatsApp 适配器配置
type Config struct {
	Name        string `yaml:"name"`
	Token       string `yaml:"token"`        // WhatsApp Cloud API Token
	PhoneID     string `yaml:"phone_id"`     // 电话号码 ID
	VerifyToken string `yaml:"verify_token"` // Webhook 验证 Token
	AppSecret   string `yaml:"app_secret"`   // Meta App Secret，用于校验 X-Hub-Signature-256
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
	a := &WhatsAppAdapter{
		config: cfg,
		client: httpx.RawClient(httpx.WithRawTimeout(30 * time.Second)),
	}
	a.queue = adapter.NewPlatformSendQueue(PlatformWhatsApp, a.sendReplyNow)
	return a
}

func (a *WhatsAppAdapter) Name() string {
	if a.config.Name != "" {
		return a.config.Name
	}
	return "whatsapp"
}
func (a *WhatsAppAdapter) Platform() adapter.Platform { return PlatformWhatsApp }

// PlatformWhatsApp WhatsApp 平台常量
const PlatformWhatsApp adapter.Platform = "whatsapp"

// Start 启动 Webhook 服务器接收消息
func (a *WhatsAppAdapter) Start(ctx context.Context, handler adapter.MessageHandler) error {
	if err := a.Attach(handler); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/whatsapp", a.handleWebhook)

	a.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", a.config.WebhookPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("[WhatsApp] Webhook 监听端口", "webhook_port", a.config.WebhookPort)

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("[WhatsApp] 服务器错误", "error", err)
		}
	}()

	return nil
}

// Attach 注册消息处理器，但不启动独立 HTTP 服务器。
func (a *WhatsAppAdapter) Attach(handler adapter.MessageHandler) error {
	a.handler = handler
	return nil
}

// Stop 停止适配器
func (a *WhatsAppAdapter) Stop(ctx context.Context) error {
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	if a.server != nil {
		return a.server.Shutdown(ctx)
	}
	return nil
}

// Handler 返回统一 ingress 使用的处理器。
func (a *WhatsAppAdapter) Handler() http.Handler {
	return http.HandlerFunc(a.handleWebhook)
}

// Send 发送文本消息
func (a *WhatsAppAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

func (a *WhatsAppAdapter) sendReplyNow(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给家长
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                chatID,
		"type":              "text",
		"text": map[string]string{
			"body": adapter.StripThinking(reply.Content),
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
	defer func() { _ = resp.Body.Close() }()

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
			_, _ = fmt.Fprint(w, challenge)
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

	// Verify Meta's X-Hub-Signature-256 (HMAC-SHA256 over the raw body) before
	// trusting the payload — same posture as the Slack/LINE/wecom adapters.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if a.config.AppSecret != "" && !a.verifySignature(r, body) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var payload whatsappWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
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
					ID:         message.ID,
					Platform:   PlatformWhatsApp,
					InstanceID: a.Name(),
					ChatID:     message.From,
					UserID:     message.From,
					UserName:   a.getContactName(change.Value.Contacts, message.From),
					Content:    message.Text.Body,
					Timestamp:  time.Now(),
				}
				go func(m *adapter.Message) {
					if a.handler == nil {
						return
					}
					// H7: Detach(r.Context()) 保留 logger，脱离 webhook 响应返回后的 cancel
					bgCtx, cancel := context.WithTimeout(trace.Detach(r.Context()), 2*time.Minute)
					defer cancel()
					reply, err := a.handler(bgCtx, m)
					if err != nil {
						logger.Error("[WhatsApp] 处理消息错误", "error", err)
						return
					}
					if reply != nil {
						if err := a.Send(bgCtx, m.ChatID, reply); err != nil {
							logger.Error("[WhatsApp] 发送回复错误", "error", err)
						}
					}
				}(msg)
			}
		}
	}
}

// verifySignature 校验 Meta 的 X-Hub-Signature-256（HMAC-SHA256 over raw body）。
func (a *WhatsAppAdapter) verifySignature(r *http.Request, body []byte) bool {
	const prefix = "sha256="
	sig := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(sig, prefix) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.config.AppSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.TrimPrefix(sig, prefix)), []byte(expected))
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

// ValidateConfig validates credentials by verifying the phone number ID.
func (a *WhatsAppAdapter) ValidateConfig(ctx context.Context) error {
	if a.config.Token == "" {
		return fmt.Errorf("whatsapp token 未配置")
	}
	if a.config.PhoneID == "" {
		return fmt.Errorf("whatsapp phone_id 未配置")
	}
	url := fmt.Sprintf("%s/%s", a.config.BaseURL, a.config.PhoneID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.config.Token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp 验证请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("whatsapp 凭证验证失败 (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// Health 返回适配器健康状态。
func (a *WhatsAppAdapter) Health(_ context.Context) error {
	if a.handler == nil {
		return fmt.Errorf("whatsapp handler 未附加")
	}
	if a.config.Token == "" || a.config.PhoneID == "" {
		return fmt.Errorf("whatsapp token/phone_id 未配置")
	}
	return nil
}

// WhatsApp Webhook 数据结构
type whatsappWebhook struct {
	Entry []whatsappEntry `json:"entry"`
}

type whatsappEntry struct {
	ID      string           `json:"id"`
	Changes []whatsappChange `json:"changes"`
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
	WaID    string          `json:"wa_id"`
	Profile whatsappProfile `json:"profile"`
}

type whatsappProfile struct {
	Name string `json:"name"`
}
