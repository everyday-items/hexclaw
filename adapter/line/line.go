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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/util/logger"

	"github.com/hexagon-codes/toolkit/net/httpx"
	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"
	linewebhook "github.com/line/line-bot-sdk-go/v8/linebot/webhook"

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
	APIEndpoint   string `yaml:"api_endpoint"`   // 测试注入；生产默认 https://api.line.me
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
	api, err := a.messagingAPI(ctx)
	if err != nil {
		return fmt.Errorf("创建 LINE SDK 客户端失败: %w", err)
	}

	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给家长（同时覆盖 reply/push 两条路径）。
	clean := adapter.StripThinking(reply.Content)
	messages := []messaging_api.MessageInterface{
		&messaging_api.TextMessage{Text: clean},
	}
	if replyToken := reply.Metadata["reply_token"]; replyToken != "" {
		_, err := api.ReplyMessage(&messaging_api.ReplyMessageRequest{
			ReplyToken: replyToken,
			Messages:   messages,
		})
		if err != nil {
			return fmt.Errorf("line reply API 发送失败: %w", err)
		}
		return nil
	}
	_, err = api.PushMessage(&messaging_api.PushMessageRequest{
		To:       chatID,
		Messages: messages,
	}, "")
	if err != nil {
		return fmt.Errorf("line push API 发送失败: %w", err)
	}
	return nil
}

// SendStream 流式发送（LINE 不支持流式编辑，聚合后完整发送）
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

	if err := whauth.RequireSecret("line", a.config.ChannelSecret); err != nil {
		http.Error(w, "Webhook signature verification required", http.StatusForbidden)
		return
	}
	parseReq := r.Clone(r.Context())
	parseReq.Body = io.NopCloser(bytes.NewReader(body))
	payload, err := linewebhook.ParseRequest(a.config.ChannelSecret, parseReq)
	if errors.Is(err, linewebhook.ErrInvalidSignature) {
		http.Error(w, "Invalid signature", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)

	for _, event := range payload.Events {
		msg, ok := messageFromWebhookEvent(event)
		if !ok {
			continue
		}
		msg.Platform = PlatformLINE
		msg.InstanceID = a.Name()

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
	api, err := a.messagingAPI(ctx)
	if err != nil {
		return fmt.Errorf("创建 LINE SDK 客户端失败: %w", err)
	}
	_, err = api.GetBotInfo()
	return err
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

func (a *LineAdapter) messagingAPI(ctx context.Context) (*messaging_api.MessagingApiAPI, error) {
	opts := []messaging_api.MessagingApiAPIOption{
		messaging_api.WithHTTPClient(a.client),
	}
	if a.config.APIEndpoint != "" {
		opts = append(opts, messaging_api.WithEndpoint(a.config.APIEndpoint))
	}
	api, err := messaging_api.NewMessagingApiAPI(a.config.ChannelToken, opts...)
	if err != nil {
		return nil, err
	}
	if ctx != nil {
		api.WithContext(ctx)
	}
	return api, nil
}

func messageFromWebhookEvent(event linewebhook.EventInterface) (*adapter.Message, bool) {
	messageEvent, ok := event.(linewebhook.MessageEvent)
	if !ok {
		return nil, false
	}
	textMessage, ok := messageEvent.Message.(linewebhook.TextMessageContent)
	if !ok {
		return nil, false
	}
	chatID, userID, sourceType := sourceIDs(messageEvent.Source)
	return &adapter.Message{
		ID:        textMessage.Id,
		ChatID:    chatID,
		UserID:    userID,
		Content:   textMessage.Text,
		Timestamp: time.UnixMilli(messageEvent.Timestamp),
		Metadata: map[string]string{
			"reply_token": messageEvent.ReplyToken,
			"source_type": sourceType,
		},
	}, true
}

func sourceIDs(source linewebhook.SourceInterface) (chatID, userID, sourceType string) {
	switch s := source.(type) {
	case linewebhook.GroupSource:
		if s.GroupId != "" {
			chatID = s.GroupId
		}
		userID = s.UserId
		sourceType = "group"
	case linewebhook.RoomSource:
		if s.RoomId != "" {
			chatID = s.RoomId
		}
		userID = s.UserId
		sourceType = "room"
	case linewebhook.UserSource:
		chatID = s.UserId
		userID = s.UserId
		sourceType = "user"
	default:
		if source != nil {
			sourceType = source.GetType()
		}
	}
	if chatID == "" {
		chatID = userID
	}
	return chatID, userID, sourceType
}
