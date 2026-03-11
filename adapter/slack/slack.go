// Package slack 提供 Slack Bot 适配器
//
// 通过 Events API (HTTP Webhook) 接收 Slack 事件，
// 通过 Web API 发送回复。支持流式更新（chat.update）。
//
// 配置方式：
//   1. 创建 Slack App，启用 Event Subscriptions
//   2. 设置 Request URL 为 http://your-host:6063/slack/events
//   3. 订阅 message.channels 和 message.im 事件
//   4. 添加 Bot Token Scopes: chat:write, channels:history, im:history
//
// 对标 OpenClaw Slack 集成。
package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/toolkit/util/idgen"
)

const slackAPIBase = "https://slack.com/api"

// SlackAdapter Slack Bot 适配器
//
// 通过 Events API 接收消息，通过 Web API 回复。
// 支持签名验证（Signing Secret）确保请求来自 Slack。
type SlackAdapter struct {
	cfg     config.SlackConfig
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client
	botID   string // Bot 自身的 User ID（避免处理自己的消息）
}

// New 创建 Slack 适配器
func New(cfg config.SlackConfig) *SlackAdapter {
	return &SlackAdapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *SlackAdapter) Name() string              { return "slack" }
func (a *SlackAdapter) Platform() adapter.Platform { return adapter.PlatformSlack }

// Start 启动 Slack Events API 服务器
func (a *SlackAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler

	if a.cfg.Token == "" {
		return fmt.Errorf("Slack Bot Token 不能为空")
	}

	// 获取 Bot 自身的 User ID
	a.fetchBotID()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /slack/events", a.handleEvents)

	a.server = &http.Server{
		Addr:              ":6063",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Slack 事件服务器错误: %v", err)
		}
	}()

	log.Println("Slack 适配器已启动（Events API 模式）")
	return nil
}

// Stop 停止适配器
func (a *SlackAdapter) Stop(ctx context.Context) error {
	if a.server != nil {
		return a.server.Shutdown(ctx)
	}
	return nil
}

// Send 发送同步回复
func (a *SlackAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	return a.postMessage(ctx, chatID, reply.Content)
}

// SendStream 流式发送（先发初始消息，后续 chat.update 更新）
func (a *SlackAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	var ts string // Slack 消息的 timestamp（作为 ID）

	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)

		if ts == "" {
			// 发送初始消息
			msgTS, err := a.postMessageWithTS(ctx, chatID, sb.String())
			if err != nil {
				return err
			}
			ts = msgTS
		} else {
			// 更新已有消息
			a.updateMessage(ctx, chatID, ts, sb.String())
		}
	}
	return nil
}

// ============== Events API 处理 ==============

// handleEvents 处理 Slack Events API 回调
func (a *SlackAdapter) handleEvents(w http.ResponseWriter, r *http.Request) {
	// 读取请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求失败", http.StatusBadRequest)
		return
	}

	// 验证签名
	if a.cfg.SigningSecret != "" {
		if !a.verifySignature(r, body) {
			http.Error(w, "签名验证失败", http.StatusUnauthorized)
			return
		}
	}

	// 解析事件类型
	var envelope struct {
		Type      string          `json:"type"`
		Challenge string          `json:"challenge"`
		Event     json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "解析请求失败", http.StatusBadRequest)
		return
	}

	switch envelope.Type {
	case "url_verification":
		// Slack URL 验证回调
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge})
		return

	case "event_callback":
		// 立即返回 200（Slack 要求 3 秒内响应）
		w.WriteHeader(http.StatusOK)
		// 异步处理事件
		go a.processEvent(envelope.Event)
	}
}

// processEvent 异步处理事件
func (a *SlackAdapter) processEvent(data json.RawMessage) {
	var event slackEvent
	if err := json.Unmarshal(data, &event); err != nil {
		log.Printf("解析 Slack 事件失败: %v", err)
		return
	}

	// 只处理消息事件
	if event.Type != "message" {
		return
	}
	// 忽略 Bot 自己的消息和消息编辑
	if event.BotID != "" || event.SubType != "" {
		return
	}
	// 忽略自己发送的消息
	if event.User == a.botID {
		return
	}

	// 转换为统一消息格式
	unified := &adapter.Message{
		ID:        "slack-" + idgen.ShortID(),
		Platform:  adapter.PlatformSlack,
		ChatID:    event.Channel,
		UserID:    event.User,
		Content:   event.Text,
		Timestamp: time.Now(),
		Metadata: map[string]string{
			"thread_ts": event.ThreadTS,
			"ts":        event.TS,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reply, err := a.handler(ctx, unified)
	if err != nil {
		log.Printf("Slack 消息处理失败: %v", err)
		a.postMessage(ctx, event.Channel, "处理消息时出错，请稍后重试。")
		return
	}
	if reply != nil {
		// 如果原消息在线程中，回复到同一线程
		if event.ThreadTS != "" {
			a.postThreadMessage(ctx, event.Channel, event.ThreadTS, reply.Content)
		} else {
			a.postMessage(ctx, event.Channel, reply.Content)
		}
	}
}

// verifySignature 验证 Slack 请求签名
//
// 使用 HMAC-SHA256 验证请求确实来自 Slack。
func (a *SlackAdapter) verifySignature(r *http.Request, body []byte) bool {
	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	signature := r.Header.Get("X-Slack-Signature")

	if timestamp == "" || signature == "" {
		return false
	}

	// 拼接签名基础字符串: v0:timestamp:body
	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))

	mac := hmac.New(sha256.New, []byte(a.cfg.SigningSecret))
	mac.Write([]byte(baseString))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

// ============== Web API ==============

// postMessage 发送消息
func (a *SlackAdapter) postMessage(ctx context.Context, channel, text string) error {
	_, err := a.postMessageWithTS(ctx, channel, text)
	return err
}

// postMessageWithTS 发送消息并返回 ts（消息 ID）
func (a *SlackAdapter) postMessageWithTS(ctx context.Context, channel, text string) (string, error) {
	body := map[string]string{
		"channel": channel,
		"text":    text,
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/chat.postMessage", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("发送 Slack 消息失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	if !result.OK {
		return "", fmt.Errorf("Slack API 错误: %s", result.Error)
	}
	return result.TS, nil
}

// postThreadMessage 回复到线程
func (a *SlackAdapter) postThreadMessage(ctx context.Context, channel, threadTS, text string) error {
	body := map[string]string{
		"channel":   channel,
		"text":      text,
		"thread_ts": threadTS,
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/chat.postMessage", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送 Slack 线程消息失败: %w", err)
	}
	resp.Body.Close()
	return nil
}

// updateMessage 更新已发送的消息（用于流式效果）
func (a *SlackAdapter) updateMessage(ctx context.Context, channel, ts, text string) error {
	body := map[string]string{
		"channel": channel,
		"ts":      ts,
		"text":    text,
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/chat.update", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("更新 Slack 消息失败: %w", err)
	}
	resp.Body.Close()
	return nil
}

// fetchBotID 获取 Bot 自身的 User ID
func (a *SlackAdapter) fetchBotID() {
	req, err := http.NewRequest(http.MethodPost, slackAPIBase+"/auth.test", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)

	resp, err := a.client.Do(req)
	if err != nil {
		log.Printf("获取 Slack Bot ID 失败: %v", err)
		return
	}
	defer resp.Body.Close()

	var result struct {
		UserID string `json:"user_id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	a.botID = result.UserID
}

// ============== 数据模型 ==============

// slackEvent Slack 事件
type slackEvent struct {
	Type     string `json:"type"`
	SubType  string `json:"subtype"`
	Channel  string `json:"channel"`
	User     string `json:"user"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	BotID    string `json:"bot_id"`
}
