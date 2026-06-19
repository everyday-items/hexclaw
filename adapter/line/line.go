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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/crypto/sign"
	"github.com/hexagon-codes/toolkit/util/logger"

	"github.com/hexagon-codes/toolkit/net/httpx"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/adapter/whauth"
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
		client: httpx.RawClient(httpx.WithRawTimeout(30 * time.Second)),
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

	logger.Info("[LINE] Webhook 监听端口", "webhook_port", a.config.WebhookPort)

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("[LINE] 服务器错误", "error", err)
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
	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给家长（同时覆盖 reply/push 两条路径）
	clean := adapter.StripThinking(reply.Content)
	if replyToken := reply.Metadata["reply_token"]; replyToken != "" {
		return a.replyMessage(ctx, replyToken, clean)
	}
	payload := map[string]any{
		"to": chatID,
		"messages": []map[string]string{
			{"type": "text", "text": clean},
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

	// W3-14：显式拒绝超大请求体，避免静默截断后用残缺数据继续解析。
	// 多读 1 字节（上限 + 1），若实际读到的长度超过上限即判定超限，
	// 返回明确的 413 而非把截断后损坏的 JSON 误报成 400。
	const maxBodyBytes = 1 << 20 // 1MB
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, fmt.Sprintf("请求体过大: 超过 %d 字节上限 (request body too large)", maxBodyBytes), http.StatusRequestEntityTooLarge)
		return
	}

	// W3-13：签名校验强制 fail-closed。
	// 此前 ChannelSecret 为空时静默跳过校验（fail-open），等于把签名校验关掉，
	// 攻击者可伪造任意未签名请求。现统一用 whauth.RequireSecret 强制要求已配置
	// 密钥，未配置即拒绝（403），而非放行；已配置则用 toolkit 一步式常量时间
	// 校验保留 LINE 平台原算法（HMAC-SHA256 + Base64）。
	if err := whauth.RequireSecret("line", a.config.ChannelSecret); err != nil {
		http.Error(w, "Webhook signature verification required", http.StatusForbidden)
		return
	}
	signature := r.Header.Get("X-Line-Signature")
	if !a.verifySignature(body, signature) {
		http.Error(w, "Invalid signature", http.StatusForbidden)
		return
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

		// W3-12：按 source.type 取会话维度标识作为 ChatID。
		// 群组（group）用 groupId、房间（room）用 roomId，单聊（user）回退 userId。
		// 此前 ChatID 与 UserID 都取 userId，导致群消息丢失 groupId、ChatID
		// 退化为发言者，无法按群会话路由/回复。
		chatID := event.Source.chatID()

		msg := &adapter.Message{
			ID:         event.Message.ID,
			Platform:   PlatformLINE,
			InstanceID: a.Name(),
			ChatID:     chatID,
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
					logger.Error("[LINE] 处理消息错误", "error", err)
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
					sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer sendCancel()
					if err := a.Send(sendCtx, m.ChatID, reply); err != nil {
						logger.Error("[LINE] 发送回复失败", "error", err)
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
//
// 直接复用 toolkit/crypto/sign 的一步式校验函数：内部一次性完成
// "重新计算 HMAC-SHA256 + 用 hmac.Equal 做常量时间比较"，天然抗时序侧信道，
// 保留 LINE 平台原算法（HMAC-SHA256 + Base64）。
func (a *LineAdapter) verifySignature(body []byte, signature string) bool {
	return sign.VerifyHMACSHA256Base64(body, []byte(a.config.ChannelSecret), signature)
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
	RoomID  string `json:"roomId,omitempty"`
}

// chatID 返回会话维度标识，用作统一 Message 的 ChatID。
//
// LINE 的 source 有三类：
//   - group：多人群组，会话标识为 groupId。
//   - room：多人聊天室，会话标识为 roomId。
//   - user：一对一单聊，会话即对方 userId。
//
// 对 group/room，若平台未带对应 ID（异常场景）则回退到 userId，
// 保证 ChatID 始终非空可路由。
func (s lineSource) chatID() string {
	switch s.Type {
	case "group":
		if s.GroupID != "" {
			return s.GroupID
		}
	case "room":
		if s.RoomID != "" {
			return s.RoomID
		}
	}
	return s.UserID
}

type lineMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
}
