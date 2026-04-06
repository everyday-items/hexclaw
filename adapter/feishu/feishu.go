// Package feishu 提供飞书（Lark）Bot 适配器
//
// 使用飞书官方 Go SDK（larkws）建立 WebSocket 长连接接收事件，无需公网地址。
// 回复通过飞书 OpenAPI 发送。
package feishu

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

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/internal/upstreamerr"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const (
	baseURL            = "https://open.feishu.cn/open-apis"
	tokenRefreshBuffer = 5 * time.Minute
)

// FeishuAdapter 飞书 Bot 适配器
type FeishuAdapter struct {
	cfg     config.FeishuConfig
	handler adapter.MessageHandler
	client  *http.Client
	queue   *adapter.SendQueue

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
	lastError   string

	wsClient  *larkws.Client
	connected atomic.Bool
	stopped   atomic.Bool
}

// New 创建飞书适配器
func New(cfg config.FeishuConfig) *FeishuAdapter {
	a := &FeishuAdapter{
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
	a.queue = adapter.NewPlatformSendQueue(adapter.PlatformFeishu, a.sendReplyNow)
	return a
}

func (a *FeishuAdapter) Name() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return "feishu"
}
func (a *FeishuAdapter) Platform() adapter.Platform { return adapter.PlatformFeishu }

// Start 启动飞书 WebSocket 长连接（使用官方 larkws SDK）
func (a *FeishuAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	a.stopped.Store(false)

	if a.cfg.AppID == "" || a.cfg.AppSecret == "" {
		return fmt.Errorf("feishu app_id/app_secret 未配置")
	}

	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			go a.handleSDKMessage(event)
			return nil
		})

	a.wsClient = larkws.NewClient(a.cfg.AppID, a.cfg.AppSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	)

	go func() {
		logger.Info("飞书适配器 [", "name", a.Name())
		a.connected.Store(true)
		a.mu.Lock()
		a.lastError = ""
		a.mu.Unlock()
		if err := a.wsClient.Start(context.Background()); err != nil {
			a.connected.Store(false)
			if !a.stopped.Load() {
				logger.Error("飞书 WebSocket 连接失败", "error", err)
				a.mu.Lock()
				a.lastError = err.Error()
				a.mu.Unlock()
			}
		}
	}()

	logger.Info("飞书适配器 [", "name", a.Name())
	return nil
}

// Stop 停止飞书适配器
func (a *FeishuAdapter) Stop(_ context.Context) error {
	a.stopped.Store(true)
	a.connected.Store(false)
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	logger.Info("飞书适配器停止中...")
	return nil
}

// Handler 返回 HTTP Handler（保留向后兼容，WebSocket 模式下不使用）
func (a *FeishuAdapter) Handler() http.Handler {
	return http.HandlerFunc(a.handleWebhook)
}

// handleSDKMessage 处理官方 SDK 收到的消息事件
func (a *FeishuAdapter) handleSDKMessage(event *larkim.P2MessageReceiveV1) {
	if a.handler == nil || event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}

	msgEvent := event.Event
	if msgEvent.Message.MessageType == nil || *msgEvent.Message.MessageType != "text" {
		if msgEvent.Message.MessageType != nil {
			logger.Info("飞书: 暂不支持", "message_type", *msgEvent.Message.MessageType)
		}
		return
	}

	if msgEvent.Message.Content == nil {
		return
	}

	var textContent struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(*msgEvent.Message.Content), &textContent); err != nil {
		logger.Error("飞书: 解析文本内容失败", "error", err)
		return
	}

	content := strings.TrimSpace(textContent.Text)
	if content == "" {
		return
	}

	chatID := ""
	if msgEvent.Message.ChatId != nil {
		chatID = *msgEvent.Message.ChatId
	}
	userID := ""
	if msgEvent.Sender != nil && msgEvent.Sender.SenderId != nil && msgEvent.Sender.SenderId.OpenId != nil {
		userID = *msgEvent.Sender.SenderId.OpenId
	}
	messageID := ""
	if msgEvent.Message.MessageId != nil {
		messageID = *msgEvent.Message.MessageId
	}
	chatType := ""
	if msgEvent.Message.ChatType != nil {
		chatType = *msgEvent.Message.ChatType
	}
	messageType := ""
	if msgEvent.Message.MessageType != nil {
		messageType = *msgEvent.Message.MessageType
	}

	msg := &adapter.Message{
		ID:         "feishu-" + idgen.ShortID(),
		Platform:   adapter.PlatformFeishu,
		InstanceID: a.Name(),
		ChatID:     chatID,
		UserID:     userID,
		UserName:   userID,
		Content:    content,
		Timestamp:  time.Now(),
		Metadata: map[string]string{
			"message_id":   messageID,
			"chat_type":    chatType,
			"message_type": messageType,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 先以回复形式发送思考占位消息，让用户知道 Agent 正在处理
	var thinkingMsgID string
	if messageID != "" {
		var thinkErr error
		thinkingMsgID, thinkErr = a.replyAndGetID(ctx, messageID, randomThinkingMessage())
		if thinkErr != nil {
			logger.Error("[feishu] 发送思考占位消息失败（将降级为直接回复）", "error", thinkErr)
		}
	}
	if thinkingMsgID == "" {
		var thinkErr error
		thinkingMsgID, thinkErr = a.sendAndGetID(ctx, msg.ChatID, randomThinkingMessage())
		if thinkErr != nil {
			logger.Error("[feishu] 发送思考占位消息失败", "error", thinkErr)
		}
	}

	reply, err := a.handler(ctx, msg)
	if err != nil {
		logger.Error("飞书: 处理消息失败", "error", err)
		errContent := "处理消息时出现错误：" + upstreamerr.PublicMessage(err, "未知错误") + "\n请检查 LLM Provider 配置后重试。"
		if thinkingMsgID != "" {
			_ = a.patchMessage(ctx, thinkingMsgID, "⚠️ "+errContent)
		} else {
			errCtx, errCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer errCancel()
			_ = a.Send(errCtx, msg.ChatID, &adapter.Reply{Content: errContent})
		}
		return
	}

	if reply == nil {
		if thinkingMsgID != "" {
			_ = a.patchMessage(ctx, thinkingMsgID, "(空回复)")
		}
		return
	}
	// 用最终回复内容替换占位消息
	if thinkingMsgID != "" {
		if pErr := a.patchMessage(ctx, thinkingMsgID, reply.Content); pErr != nil {
			logger.Error("[feishu] 更新占位消息失败，降级为新消息", "error", pErr)
			sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer sendCancel()
			_ = a.Send(sendCtx, msg.ChatID, reply)
		}
	} else {
		sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer sendCancel()
		if err := a.Send(sendCtx, msg.ChatID, reply); err != nil {
			logger.Error("飞书: 发送回复失败", "error", err)
		}
	}
}

// handleWebhook 处理飞书事件回调（向后兼容 HTTP Webhook）
func (a *FeishuAdapter) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	var event feishuEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "error", http.StatusBadRequest)
		return
	}

	if event.Challenge != "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"challenge": event.Challenge,
		})
		return
	}

	if event.Header.Token != "" && a.cfg.VerificationToken != "" {
		if event.Header.Token != a.cfg.VerificationToken {
			http.Error(w, "验证失败", http.StatusUnauthorized)
			return
		}
	}

	if event.Header.EventType == "im.message.receive_v1" {
		go a.handleMessage(event)
	}

	w.WriteHeader(http.StatusOK)
}

// ============== 消息处理 ==============

// Send 发送同步回复
func (a *FeishuAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

func (a *FeishuAdapter) sendReplyNow(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("获取 Access Token 失败: %w", err)
	}

	body := map[string]any{
		"receive_id": chatID,
		"msg_type":   "text",
		"content":    marshalTextContent(reply.Content),
	}
	bodyJSON, _ := json.Marshal(body)

	url := baseURL + "/im/v1/messages?receive_id_type=chat_id"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("飞书 API 返回 %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// SendStream 发送流式回复
//
// 先立即发送"思考中..."占位消息，收到内容后逐步更新，
// 让用户知道 Agent 正在处理（类似飞书"正在输入..."体验）。
func (a *FeishuAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	// 立即发送思考状态占位消息（随机表情增加趣味性）
	messageID, err := a.sendAndGetID(ctx, chatID, randomThinkingMessage())
	if err != nil {
		logger.Error("[feishu] 发送思考占位消息失败（将降级为直接回复）", "error", err)
	}

	var sb strings.Builder
	lastUpdateLen := 0
	lastUpdateTime := time.Now()
	const updateThreshold = 50         // 最少积累 50 字符才更新
	const updateInterval = time.Second // 最少间隔 1 秒才更新（防限流）

	for chunk := range chunks {
		if chunk.Error != nil {
			// 出错时更新占位消息为错误提示
			if messageID != "" {
				_ = a.patchMessage(ctx, messageID, "⚠️ 处理失败，请重试")
			}
			return chunk.Error
		}
		if chunk.Done {
			break
		}
		sb.WriteString(chunk.Content)

		// 收到内容后逐步更新占位消息（限流保护）
		if messageID != "" && sb.Len()-lastUpdateLen >= updateThreshold && time.Since(lastUpdateTime) >= updateInterval {
			if pErr := a.patchMessage(ctx, messageID, sb.String()+"…"); pErr != nil {
				logger.Error("[feishu] 更新流式消息失败", "error", pErr)
			}
			lastUpdateLen = sb.Len()
			lastUpdateTime = time.Now()
		}
	}

	finalContent := sb.String()
	if finalContent == "" {
		// 没有内容，删除占位消息或更新为空回复
		if messageID != "" {
			_ = a.patchMessage(ctx, messageID, "(空回复)")
		}
		return nil
	}
	if messageID != "" {
		return a.patchMessage(ctx, messageID, finalContent)
	}
	return a.Send(ctx, chatID, &adapter.Reply{Content: finalContent})
}

// sendAndGetID 发送消息并返回 message_id
func (a *FeishuAdapter) sendAndGetID(ctx context.Context, chatID, text string) (string, error) {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return "", err
	}

	body := map[string]any{
		"receive_id": chatID,
		"msg_type":   "text",
		"content":    marshalTextContent(text),
	}
	bodyJSON, _ := json.Marshal(body)

	url := baseURL + "/im/v1/messages?receive_id_type=chat_id"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	return result.Data.MessageID, nil
}

// replyAndGetID 以回复形式回复指定消息，返回新消息的 message_id
func (a *FeishuAdapter) replyAndGetID(ctx context.Context, replyToMsgID, text string) (string, error) {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return "", err
	}

	body := map[string]any{
		"msg_type": "text",
		"content":  marshalTextContent(text),
	}
	bodyJSON, _ := json.Marshal(body)

	url := baseURL + "/im/v1/messages/" + replyToMsgID + "/reply"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("飞书 reply 返回 %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(respBody, &result)
	if result.Code != 0 {
		return "", fmt.Errorf("飞书 reply 业务错误 code=%d: %s", result.Code, result.Msg)
	}
	return result.Data.MessageID, nil
}

// patchMessage 编辑已发送的消息
func (a *FeishuAdapter) patchMessage(ctx context.Context, messageID, text string) error {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return err
	}

	body := map[string]any{
		"msg_type": "text",
		"content":  marshalTextContent(text),
	}
	bodyJSON, _ := json.Marshal(body)

	url := baseURL + "/im/v1/messages/" + messageID
	req, err := http.NewRequestWithContext(ctx, "PATCH", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("飞书 PATCH 消息返回 %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// randomThinkingMessage 随机返回一条思考状态消息
func randomThinkingMessage() string {
	messages := []string{
		"🤔 思考中...",
		"⌨️ 正在输入...",
		"🧠 让我想想...",
		"💭 思考中...",
		"📝 正在整理思路...",
		"🔍 查找中...",
		"⏳ 稍等一下...",
		"🎯 正在分析...",
	}
	return messages[time.Now().UnixNano()%int64(len(messages))]
}

// handleMessage 处理消息事件（webhook 兼容路径）
func (a *FeishuAdapter) handleMessage(event feishuEvent) {
	if a.handler == nil {
		return
	}

	msgEvent := event.Event
	if msgEvent.Message.MessageType != "text" {
		logger.Info("飞书: 暂不支持", "message_type", msgEvent.Message.MessageType)
		return
	}

	var textContent struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(msgEvent.Message.Content), &textContent); err != nil {
		logger.Error("飞书: 解析文本内容失败", "error", err)
		return
	}

	content := strings.TrimSpace(textContent.Text)
	if content == "" {
		return
	}

	msg := &adapter.Message{
		ID:         "feishu-" + idgen.ShortID(),
		Platform:   adapter.PlatformFeishu,
		InstanceID: a.Name(),
		ChatID:     msgEvent.Message.ChatID,
		UserID:     msgEvent.Sender.SenderID.OpenID,
		UserName:   msgEvent.Sender.SenderID.OpenID,
		Content:    content,
		Timestamp:  time.Now(),
		Metadata: map[string]string{
			"message_id":   msgEvent.Message.MessageID,
			"chat_type":    msgEvent.Message.ChatType,
			"message_type": msgEvent.Message.MessageType,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 先以回复形式发送思考占位消息
	origMsgID := msgEvent.Message.MessageID
	var thinkingMsgID string
	if origMsgID != "" {
		var thinkErr error
		thinkingMsgID, thinkErr = a.replyAndGetID(ctx, origMsgID, randomThinkingMessage())
		if thinkErr != nil {
			logger.Error("[feishu] 发送思考占位消息失败（将降级为直接回复）", "error", thinkErr)
		}
	}
	if thinkingMsgID == "" {
		var thinkErr error
		thinkingMsgID, thinkErr = a.sendAndGetID(ctx, msg.ChatID, randomThinkingMessage())
		if thinkErr != nil {
			logger.Error("[feishu] 发送思考占位消息失败", "error", thinkErr)
		}
	}

	reply, err := a.handler(ctx, msg)
	if err != nil {
		logger.Error("飞书: 处理消息失败", "error", err)
		errContent := "处理消息时出现错误：" + upstreamerr.PublicMessage(err, "未知错误") + "\n请检查 LLM Provider 配置后重试。"
		if thinkingMsgID != "" {
			_ = a.patchMessage(ctx, thinkingMsgID, "⚠️ "+errContent)
		} else {
			errCtx, errCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer errCancel()
			_ = a.Send(errCtx, msg.ChatID, &adapter.Reply{Content: errContent})
		}
		return
	}

	if reply == nil {
		if thinkingMsgID != "" {
			_ = a.patchMessage(ctx, thinkingMsgID, "(空回复)")
		}
		return
	}
	if thinkingMsgID != "" {
		if pErr := a.patchMessage(ctx, thinkingMsgID, reply.Content); pErr != nil {
			logger.Error("[feishu] 更新占位消息失败，降级为新消息", "error", pErr)
			sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer sendCancel()
			_ = a.Send(sendCtx, msg.ChatID, reply)
		}
	} else {
		sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer sendCancel()
		if err := a.Send(sendCtx, msg.ChatID, reply); err != nil {
			logger.Error("飞书: 发送回复失败", "error", err)
		}
	}
}

// ValidateConfig validates credentials by attempting to fetch a tenant access token.
func (a *FeishuAdapter) ValidateConfig(ctx context.Context) error {
	if a.cfg.AppID == "" || a.cfg.AppSecret == "" {
		return fmt.Errorf("feishu app_id/app_secret 未配置")
	}
	_, err := a.getAccessToken(ctx)
	return err
}

// Health 返回适配器运行时健康状态
func (a *FeishuAdapter) Health(_ context.Context) error {
	if a.cfg.AppID == "" || a.cfg.AppSecret == "" {
		return fmt.Errorf("feishu app_id/app_secret 未配置")
	}
	if a.handler == nil {
		return fmt.Errorf("feishu handler 未附加，请重启引擎")
	}
	if a.stopped.Load() {
		return fmt.Errorf("feishu adapter stopped")
	}
	if !a.connected.Load() {
		a.mu.RLock()
		lastErr := a.lastError
		a.mu.RUnlock()
		if lastErr != "" {
			return fmt.Errorf("feishu WebSocket 未连接: %s", lastErr)
		}
		return fmt.Errorf("feishu WebSocket 未连接，请检查飞书开放平台是否已启用机器人和长连接事件订阅")
	}
	return nil
}

// ============== Token 管理 ==============

// getAccessToken 获取飞书 Tenant Access Token（带缓存）
func (a *FeishuAdapter) getAccessToken(ctx context.Context) (string, error) {
	a.mu.RLock()
	if a.accessToken != "" && time.Now().Before(a.tokenExpiry.Add(-tokenRefreshBuffer)) {
		token := a.accessToken
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.accessToken != "" && time.Now().Before(a.tokenExpiry.Add(-tokenRefreshBuffer)) {
		return a.accessToken, nil
	}

	body, _ := json.Marshal(map[string]string{
		"app_id":     a.cfg.AppID,
		"app_secret": a.cfg.AppSecret,
	})

	url := baseURL + "/auth/v3/tenant_access_token/internal"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 Token 请求失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析 Token 响应失败: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("获取 Token 失败: code=%d, msg=%s", result.Code, result.Msg)
	}

	a.accessToken = result.TenantAccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(result.Expire) * time.Second)
	logger.Info("飞书 Access Token 已刷新, 有效期", "expire", result.Expire)

	return a.accessToken, nil
}

// ============== 数据模型 ==============

// feishuEvent 飞书事件通用结构（webhook 兼容）
type feishuEvent struct {
	Challenge string `json:"challenge,omitempty"`
	Token     string `json:"token,omitempty"`
	Type      string `json:"type,omitempty"`

	Header struct {
		EventID    string `json:"event_id"`
		EventType  string `json:"event_type"`
		CreateTime string `json:"create_time"`
		Token      string `json:"token"`
		AppID      string `json:"app_id"`
	} `json:"header"`

	Event struct {
		Sender struct {
			SenderID struct {
				UnionID string `json:"union_id"`
				UserID  string `json:"user_id"`
				OpenID  string `json:"open_id"`
			} `json:"sender_id"`
			SenderType string `json:"sender_type"`
		} `json:"sender"`
		Message struct {
			MessageID   string `json:"message_id"`
			RootID      string `json:"root_id"`
			ParentID    string `json:"parent_id"`
			CreateTime  string `json:"create_time"`
			ChatID      string `json:"chat_id"`
			ChatType    string `json:"chat_type"`
			MessageType string `json:"message_type"`
			Content     string `json:"content"`
		} `json:"message"`
	} `json:"event"`
}

func marshalTextContent(text string) string {
	b, _ := json.Marshal(map[string]string{"text": text})
	return string(b)
}
