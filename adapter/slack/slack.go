// Package slack 提供 Slack Bot 适配器
//
// 通过 Events API (HTTP Webhook) 接收 Slack 事件，
// 通过 Web API 发送回复。支持流式更新（chat.update）。
//
// 配置方式：
//  1. 创建 Slack App，启用 Event Subscriptions
//  2. 设置 Request URL 为 http://your-host:6063/slack/events
//  3. 订阅 message.channels 和 message.im 事件
//  4. 添加 Bot Token Scopes: chat:write, channels:history, im:history
//
// 对标 OpenClaw Slack 集成。
package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/crypto/sign"
	"github.com/hexagon-codes/toolkit/util/logger"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/toolkit/net/httpx"
	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// slackAPIBase 是 Slack Web API 的基础地址。
// 声明为 var 而非 const，便于测试时指向 httptest mock 服务器，
// 验证对 Slack API ok:false 响应的处理（无需真实网络）。
var slackAPIBase = "https://slack.com/api"

// SlackAdapter Slack Bot 适配器
//
// 通过 Events API 接收消息，通过 Web API 回复。
// 支持签名验证（Signing Secret）确保请求来自 Slack。
type SlackAdapter struct {
	cfg     config.SlackConfig
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client
	queue   *adapter.SendQueue
	botID   string // Bot 自身的 User ID（避免处理自己的消息）
}

// New 创建 Slack 适配器
func New(cfg config.SlackConfig) *SlackAdapter {
	a := &SlackAdapter{
		cfg:    cfg,
		client: httpx.RawClient(httpx.WithRawTimeout(30 * time.Second)),
	}
	a.queue = adapter.NewPlatformSendQueue(adapter.PlatformSlack, a.sendReplyNow)
	return a
}

func (a *SlackAdapter) Name() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return "slack"
}
func (a *SlackAdapter) Platform() adapter.Platform { return adapter.PlatformSlack }

// Attach 注册消息处理器，但不启动独立 HTTP 服务器。
func (a *SlackAdapter) Attach(handler adapter.MessageHandler) error {
	a.handler = handler
	if a.cfg.Token == "" {
		return fmt.Errorf("slack bot token 不能为空")
	}
	a.fetchBotID()
	return nil
}

// Start 启动 Slack Events API 服务器
func (a *SlackAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	if err := a.Attach(handler); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /slack/events", a.handleEvents)

	port := a.cfg.WebhookPort
	if port <= 0 {
		port = 6063
	}
	addr := fmt.Sprintf(":%d", port)

	a.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Slack 事件服务器错误", "error", err)
		}
	}()

	logger.Info("Slack 适配器已启动（Events API 模式）")
	return nil
}

// Stop 停止适配器
func (a *SlackAdapter) Stop(ctx context.Context) error {
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	if a.server != nil {
		return a.server.Shutdown(ctx)
	}
	return nil
}

// Handler 返回统一 ingress 使用的处理器。
func (a *SlackAdapter) Handler() http.Handler {
	return http.HandlerFunc(a.handleEvents)
}

// Send 发送同步回复
//
// v0.4.0 F6：reply.Interactive 非空时，flag interactive.render.v1 OFF 自动追加
// 文本 fallback（"1) 是  2) 不是"），让按钮/选项/审批在 Slack 基础可用；flag ON
// 时 no-op，等待后续 Block Kit 原生 renderer 接入。
func (a *SlackAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	adapter.MaybeApplyTextFallback(ctx, reply)
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

// SendStream 流式发送（先发初始消息，后续 chat.update 更新）
//
// v0.4.0 E1：接入 adapter.StreamEditLimiter，避免高频 chat.update 触发 Slack
// 限流（Tier 2 限流约 50 次/分钟）。结合字符阈值 + 时间间隔 + 失败降级三级保护。
func (a *SlackAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	var ts string // Slack 消息的 timestamp（作为 ID）
	limiter := adapter.NewStreamEditLimiter()

	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)

		// v0.4.0 E2：每次 update 都 strip thinking，防止半截 <think> 标签闪现给用户
		clean := adapter.StripThinking(sb.String())
		// 首次 post 必发；后续 update 走限流器
		if ts == "" {
			if !limiter.ShouldEmit(sb.Len()) {
				continue
			}
			msgTS, err := a.postMessageWithTS(ctx, chatID, clean)
			if err != nil {
				if degraded := limiter.Failed(); degraded {
					logger.Warn("[slack] 流式限流降级", "delay", limiter.CurrentDelay())
				}
				return err
			}
			ts = msgTS
			limiter.Emitted(sb.Len())
		} else if limiter.ShouldEmit(sb.Len()) {
			if err := a.updateMessage(ctx, chatID, ts, clean); err != nil {
				logger.Error("[slack] 更新流式消息失败", "error", err)
				if degraded := limiter.Failed(); degraded {
					logger.Warn("[slack] 流式限流降级", "delay", limiter.CurrentDelay())
				}
			} else {
				limiter.Emitted(sb.Len())
			}
		}
	}

	// 收尾：如果限流期间一直没 post，仍要把完整结果落地
	finalClean := adapter.StripThinking(sb.String())
	if finalClean == "" {
		return nil
	}
	if ts == "" {
		_, err := a.postMessageWithTS(ctx, chatID, finalClean)
		return err
	}
	return a.updateMessage(ctx, chatID, ts, finalClean)
}

// ============== Events API 处理 ==============

// handleEvents 处理 Slack Events API 回调
func (a *SlackAdapter) handleEvents(w http.ResponseWriter, r *http.Request) {
	// 读取请求体
	// BUG-20260611: cap webhook body to 1 MiB — external callers must not
	// be able to OOM the sidecar with an unbounded payload.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// Distinguish an oversized payload (413) from a generic read error (400).
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "读取请求失败", http.StatusBadRequest)
		return
	}

	// 验证签名
	if a.cfg.SigningSecret != "" {
		if !a.verifySignature(r, body) {
			http.Error(w, "invalid request signature", http.StatusUnauthorized)
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
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge})
		return

	case "event_callback":
		// 立即返回 200（Slack 要求 3 秒内响应）
		w.WriteHeader(http.StatusOK)
		// v0.4.0 E5：透传 r.Context() 到异步处理，processEvent 内部用 trace.Detach
		// 保留 logger/Values，同时脱离 webhook 响应返回后的 ctx cancel
		go a.processEvent(r.Context(), envelope.Event)
	}
}

// processEvent 异步处理事件
//
// v0.4.0 E5：reqCtx 是 webhook 请求的 ctx，仅用作 logger/trace 链路源头。
// 内部一律用 trace.Detach(reqCtx) 派生子 ctx，避免 webhook 响应完成后 cancel
// 把消息处理半路杀掉。
func (a *SlackAdapter) processEvent(reqCtx context.Context, data json.RawMessage) {
	var event slackEvent
	if err := json.Unmarshal(data, &event); err != nil {
		logger.Error("解析 Slack 事件失败", "error", err)
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
		ID:         "slack-" + idgen.ShortID(),
		Platform:   adapter.PlatformSlack,
		InstanceID: a.Name(),
		ChatID:     event.Channel,
		UserID:     event.User,
		Content:    event.Text,
		Timestamp:  time.Now(),
		Metadata: map[string]string{
			"thread_ts": event.ThreadTS,
			"ts":        event.TS,
		},
	}

	// H7: Detach(reqCtx) 保留 logger/Values，脱离 webhook 响应后的 cancel
	bgCtx, cancel := context.WithTimeout(trace.Detach(reqCtx), 120*time.Second)
	defer cancel()

	reply, err := a.handler(bgCtx, unified)
	if err != nil {
		logger.Error("Slack 消息处理失败", "error", err)
		errCtx, errCancel := context.WithTimeout(trace.Detach(reqCtx), 10*time.Second)
		defer errCancel()
		_ = a.Send(errCtx, event.Channel, &adapter.Reply{Content: "处理消息时出错，请稍后重试。"})
		return
	}
	if reply != nil {
		if event.ThreadTS != "" {
			if reply.Metadata == nil {
				reply.Metadata = make(map[string]string, 1)
			}
			reply.Metadata["thread_ts"] = event.ThreadTS
		}
		sendCtx, sendCancel := context.WithTimeout(trace.Detach(reqCtx), 30*time.Second)
		defer sendCancel()
		if err := a.Send(sendCtx, event.Channel, reply); err != nil {
			logger.Error("Slack 回复失败", "error", err)
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

	// 防重放攻击：时间戳偏差超过 5 分钟则拒绝
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if abs64(time.Now().Unix()-ts) > 300 {
		return false
	}

	// 拼接签名基础字符串: v0:timestamp:body
	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))

	expected := "v0=" + sign.HMACSHA256Hex([]byte(baseString), []byte(a.cfg.SigningSecret))

	return hmac.Equal([]byte(expected), []byte(signature))
}

// ============== Web API ==============

// postMessage 发送消息
func (a *SlackAdapter) postMessage(ctx context.Context, channel, text string) error {
	_, err := a.postMessageWithTS(ctx, channel, text)
	return err
}

func (a *SlackAdapter) sendReplyNow(ctx context.Context, channel string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	if threadTS := reply.Metadata["thread_ts"]; threadTS != "" {
		return a.postThreadMessage(ctx, channel, threadTS, reply.Content)
	}
	return a.postMessage(ctx, channel, reply.Content)
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
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析 Slack 响应失败: %w", err)
	}

	if !result.OK {
		return "", fmt.Errorf("slack API 错误: %s", result.Error)
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
	defer func() { _ = resp.Body.Close() }()

	// Slack Web API 即使业务失败也返回 HTTP 200，仅通过 body 中的 "ok":false
	// 表达失败，因此必须解析 ok 字段，否则发送失败会被当成成功。
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("解析 Slack 响应失败: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("slack API 错误: %s", result.Error)
	}
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
	defer func() { _ = resp.Body.Close() }()

	// 同 postThreadMessage：chat.update 业务失败时也只在 body 标 ok=false，
	// 不解析 ok 会把失败当成功（功能未闭环）。
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("解析 Slack 响应失败: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("slack API 错误: %s", result.Error)
	}
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
		logger.Error("获取 Slack Bot ID 失败", "error", err)
		return
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logger.Error("解析 Slack auth.test 响应失败", "error", err)
		return
	}
	// auth.test 业务失败（如 invalid_auth）时仍返回 HTTP 200，但 ok=false 且
	// user_id 为空。不校验 ok 会把空 user_id 当成有效 botID 污染状态。
	if !result.OK {
		logger.Error("获取 Slack Bot ID 失败", "error", result.Error)
		return
	}
	a.botID = result.UserID
}

// ValidateConfig validates credentials by calling auth.test.
func (a *SlackAdapter) ValidateConfig(ctx context.Context) error {
	if a.cfg.Token == "" {
		return fmt.Errorf("slack bot token 未配置")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/auth.test", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack auth.test 请求失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("解析 slack auth.test 响应失败: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("slack auth.test 失败: %s", result.Error)
	}
	return nil
}

// Health 返回当前实例的基本健康状态。
func (a *SlackAdapter) Health(_ context.Context) error {
	if a.cfg.Token == "" {
		return fmt.Errorf("slack bot token 不能为空")
	}
	if a.handler == nil {
		return fmt.Errorf("slack handler 未附加")
	}
	if a.botID == "" {
		return fmt.Errorf("slack bot id 未初始化")
	}
	return nil
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

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
