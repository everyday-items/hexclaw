// Package dingtalk 提供钉钉 Bot 适配器
//
// 通过 Stream 长连接（WebSocket）接收钉钉事件，无需公网地址。
// 回复通过钉钉 OpenAPI 发送。
//
// Stream 模式流程：
//  1. 调用 /v1.0/gateway/connections/open 获取 WebSocket 端点
//  2. 客户端主动连接 WebSocket
//  3. 通过 WebSocket 接收消息事件
//  4. 发送回复通过 REST API
package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hexagon-codes/toolkit/util/logger"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	sign_ "github.com/hexagon-codes/toolkit/crypto/sign"
	"github.com/hexagon-codes/toolkit/net/httpx"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const apiBase = "https://api.dingtalk.com"

// DingtalkAdapter 钉钉 Bot 适配器
type DingtalkAdapter struct {
	cfg     config.DingtalkConfig
	handler adapter.MessageHandler
	client  *http.Client
	queue   *adapter.SendQueue

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time

	conn    *websocket.Conn
	connMu  sync.Mutex
	stopped atomic.Bool
}

// New 创建钉钉适配器
func New(cfg config.DingtalkConfig) *DingtalkAdapter {
	a := &DingtalkAdapter{
		cfg:    cfg,
		client: httpx.RawClient(httpx.WithRawTimeout(10 * time.Second)),
	}
	a.queue = adapter.NewPlatformSendQueue(adapter.PlatformDingtalk, a.sendReplyNow)
	return a
}

func (a *DingtalkAdapter) Name() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return "dingtalk"
}
func (a *DingtalkAdapter) Platform() adapter.Platform { return adapter.PlatformDingtalk }

// Start 启动钉钉 Stream 长连接
func (a *DingtalkAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	a.stopped.Store(false)
	go a.connectLoop()
	logger.Info("钉钉适配器 [", "name", a.Name())
	return nil
}

// Stop 停止钉钉适配器
func (a *DingtalkAdapter) Stop(ctx context.Context) error {
	a.stopped.Store(true)
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}

	a.connMu.Lock()
	if a.conn != nil {
		_ = a.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = a.conn.Close()
		a.conn = nil
	}
	a.connMu.Unlock()

	return nil
}

// Handler 返回 HTTP Handler（保留向后兼容）
func (a *DingtalkAdapter) Handler() http.Handler {
	return http.HandlerFunc(a.handleWebhook)
}

// ============== Stream 长连接 ==============

// connectLoop 自动重连循环
func (a *DingtalkAdapter) connectLoop() {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for !a.stopped.Load() {
		if err := a.connectAndListen(); err != nil {
			if !a.stopped.Load() {
				logger.Info("钉钉 Stream 断开", "error", err, "backoff", backoff)
				time.Sleep(backoff)
				backoff = min(backoff*2, maxBackoff)
			}
		} else {
			backoff = time.Second
		}
	}
}

// connectAndListen 建立 Stream 连接并监听
func (a *DingtalkAdapter) connectAndListen() error {
	endpoint, ticket, err := a.openConnection()
	if err != nil {
		return fmt.Errorf("打开 Stream 连接失败: %w", err)
	}

	wsURL := endpoint + "?ticket=" + ticket
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("WebSocket 连接失败: %w", err)
	}

	a.connMu.Lock()
	a.conn = conn
	a.connMu.Unlock()

	defer func() {
		_ = conn.Close()
		a.connMu.Lock()
		a.conn = nil
		a.connMu.Unlock()
	}()

	logger.Info("钉钉 Stream 连接已建立")

	stopPing := make(chan struct{})
	go a.pingLoop(conn, 30*time.Second, stopPing)
	defer close(stopPing)

	for !a.stopped.Load() {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !a.stopped.Load() {
				return fmt.Errorf("读取消息失败: %w", err)
			}
			return nil
		}
		a.handleStreamMessage(conn, msg)
	}
	return nil
}

// openConnection 调用钉钉 Stream API 获取 WebSocket 端点
func (a *DingtalkAdapter) openConnection() (endpoint, ticket string, err error) {
	body, _ := json.Marshal(map[string]any{
		"clientId":     a.cfg.AppKey,
		"clientSecret": a.cfg.AppSecret,
		"subscriptions": []map[string]string{
			{"type": "EVENT", "id": "*"},
			{"type": "CALLBACK", "id": "chat_bot_message_receive"},
		},
		"ua": "hexclaw",
	})

	req, err := http.NewRequest("POST", apiBase+"/v1.0/gateway/connections/open", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("请求 Stream 端点失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Endpoint string `json:"endpoint"`
		Ticket   string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("解析响应失败: %w", err)
	}
	if result.Endpoint == "" || result.Ticket == "" {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("钉钉返回端点为空: %s", string(respBody))
	}

	return result.Endpoint, result.Ticket, nil
}

// streamFrame 钉钉 Stream 消息帧
type streamFrame struct {
	SpecVersion string            `json:"specVersion,omitempty"`
	Type        string            `json:"type"`
	Headers     map[string]string `json:"headers,omitempty"`
	Data        string            `json:"data,omitempty"`
}

// handleStreamMessage 处理 Stream 收到的消息
func (a *DingtalkAdapter) handleStreamMessage(conn *websocket.Conn, raw []byte) {
	var frame streamFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		logger.Error("钉钉 Stream: 解析消息失败", "error", err)
		return
	}

	switch frame.Type {
	case "SYSTEM":
		if frame.Headers["topic"] == "ping" {
			a.sendStreamAck(conn, frame, "")
		}
	case "EVENT", "CALLBACK":
		go a.handleStreamEvent(conn, frame)
	default:
		logger.Info("钉钉 Stream: 未知消息类型", "type", frame.Type)
	}
}

// sendStreamAck 发送 ack 确认
func (a *DingtalkAdapter) sendStreamAck(conn *websocket.Conn, frame streamFrame, body string) {
	msgID := frame.Headers["messageId"]
	ack := map[string]any{
		"code":    200,
		"headers": map[string]string{"contentType": "application/json", "messageId": msgID},
		"message": "OK",
		"data":    body,
	}
	data, _ := json.Marshal(ack)

	a.connMu.Lock()
	defer a.connMu.Unlock()
	if conn != nil {
		_ = conn.WriteMessage(websocket.TextMessage, data)
	}
}

// handleStreamEvent 处理 Stream 事件
func (a *DingtalkAdapter) handleStreamEvent(conn *websocket.Conn, frame streamFrame) {
	a.sendStreamAck(conn, frame, "")

	if frame.Data == "" {
		return
	}

	var event dtEvent
	if err := json.Unmarshal([]byte(frame.Data), &event); err != nil {
		logger.Error("钉钉 Stream: 解析事件数据失败", "error", err)
		return
	}

	if event.Text.Content != "" {
		a.handleMessage(event)
	}
}

// pingLoop 定期发送 ping
func (a *DingtalkAdapter) pingLoop(conn *websocket.Conn, interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.connMu.Lock()
			err := conn.WriteMessage(websocket.PingMessage, nil)
			a.connMu.Unlock()
			if err != nil {
				return
			}
		case <-stop:
			return
		}
	}
}

// handleWebhook 处理钉钉回调（向后兼容 HTTP Webhook）
func (a *DingtalkAdapter) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	timestamp := r.Header.Get("timestamp")
	sign := r.Header.Get("sign")
	if a.cfg.AppSecret != "" && !a.verifySign(timestamp, sign) {
		http.Error(w, "name", http.StatusUnauthorized)
		return
	}

	var event dtEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "error", http.StatusBadRequest)
		return
	}

	if event.Text.Content != "" {
		go a.handleMessage(event)
	}

	w.WriteHeader(http.StatusOK)
}

// ============== 消息处理 ==============

// Send 发送消息
//
// v0.4.0 F6：reply.Interactive 非空且 flag interactive.render.v1 OFF 时，
// 自动追加文本 fallback 让按钮/选项/审批/卡片在钉钉基础可用。
func (a *DingtalkAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	adapter.MaybeApplyTextFallback(ctx, reply)
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

func (a *DingtalkAdapter) sendReplyNow(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("获取 Access Token 失败: %w", err)
	}

	body, _ := json.Marshal(map[string]any{
		"robotCode": a.cfg.RobotCode,
		"userIds":   []string{chatID},
		"msgKey":    "sampleText",
		"msgParam":  marshalTextContent(reply.Content),
	})

	url := apiBase + "/v1.0/robot/oToMessages/batchSend"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("钉钉 API 返回 %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SendStream 流式发送（拼接后一次性发送）
func (a *DingtalkAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给家长
	return a.Send(ctx, chatID, &adapter.Reply{Content: adapter.StripThinking(sb.String())})
}

// handleMessage 处理消息
func (a *DingtalkAdapter) handleMessage(event dtEvent) {
	if a.handler == nil {
		return
	}

	content := strings.TrimSpace(event.Text.Content)
	if content == "" {
		return
	}

	msg := &adapter.Message{
		ID:         "dt-" + idgen.ShortID(),
		Platform:   adapter.PlatformDingtalk,
		InstanceID: a.Name(),
		ChatID:     event.SenderStaffId,
		UserID:     event.SenderStaffId,
		UserName:   event.SenderNick,
		Content:    content,
		Timestamp:  time.Now(),
		Metadata: map[string]string{
			"conversation_id":   event.ConversationId,
			"conversation_type": event.ConversationType,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	reply, err := a.handler(ctx, msg)
	if err != nil {
		logger.Error("钉钉: 处理消息失败", "error", err)
		errCtx, errCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer errCancel()
		_ = a.Send(errCtx, msg.ChatID, &adapter.Reply{Content: "处理消息时出现错误，请稍后重试。"})
		return
	}
	if reply == nil {
		return
	}

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()
	if err := a.Send(sendCtx, msg.ChatID, reply); err != nil {
		logger.Error("钉钉: 发送回复失败", "error", err)
	}
}

// ============== Token 管理 ==============

// getAccessToken 获取钉钉 Access Token（带缓存）
func (a *DingtalkAdapter) getAccessToken(ctx context.Context) (string, error) {
	a.mu.RLock()
	if a.accessToken != "" && time.Now().Before(a.tokenExpiry.Add(-5*time.Minute)) {
		token := a.accessToken
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.accessToken != "" && time.Now().Before(a.tokenExpiry.Add(-5*time.Minute)) {
		return a.accessToken, nil
	}

	body, _ := json.Marshal(map[string]string{
		"appKey":    a.cfg.AppKey,
		"appSecret": a.cfg.AppSecret,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", apiBase+"/v1.0/oauth2/accessToken", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	a.accessToken = result.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(result.ExpireIn) * time.Second)
	return a.accessToken, nil
}

// verifySign 验证钉钉签名（用于向后兼容 Webhook 模式）
// v0.3.12 C2：复用 toolkit/crypto/sign 的常量时间比较实现
func (a *DingtalkAdapter) verifySign(timestamp, sign string) bool {
	if timestamp == "" || sign == "" {
		return false
	}
	stringToSign := timestamp + "\n" + a.cfg.AppSecret
	return sign_.VerifyHMACSHA256Base64([]byte(stringToSign), []byte(a.cfg.AppSecret), sign)
}

// Health 返回适配器健康状态。
func (a *DingtalkAdapter) ValidateConfig(_ context.Context) error {
	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" || a.cfg.RobotCode == "" {
		return fmt.Errorf("dingtalk app_key/app_secret/robot_code 未配置")
	}
	return nil
}

func (a *DingtalkAdapter) Health(_ context.Context) error {
	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" || a.cfg.RobotCode == "" {
		return fmt.Errorf("dingtalk app_key/app_secret/robot_code 未配置")
	}
	if a.handler == nil {
		return fmt.Errorf("dingtalk handler 未附加")
	}
	if a.stopped.Load() {
		return fmt.Errorf("dingtalk adapter stopped")
	}
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.conn == nil {
		return fmt.Errorf("dingtalk Stream 未连接")
	}
	return nil
}

// ============== 数据模型 ==============

// dtEvent 钉钉消息事件
type dtEvent struct {
	ConversationId   string `json:"conversationId"`
	ConversationType string `json:"conversationType"`
	SenderStaffId    string `json:"senderStaffId"`
	SenderNick       string `json:"senderNick"`
	Text             struct {
		Content string `json:"content"`
	} `json:"text"`
	MsgType string `json:"msgtype"`
}

func marshalTextContent(text string) string {
	b, _ := json.Marshal(map[string]string{"content": text})
	return string(b)
}
