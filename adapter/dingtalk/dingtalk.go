// Package dingtalk 提供钉钉 Bot 适配器
//
// 通过 HTTP Webhook 接收钉钉事件回调，将消息转换为统一格式。
// 回复通过钉钉 OpenAPI 发送。
package dingtalk

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
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const apiBase = "https://api.dingtalk.com"

// DingtalkAdapter 钉钉 Bot 适配器
type DingtalkAdapter struct {
	cfg     config.DingtalkConfig
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client
	queue   *adapter.SendQueue

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
}

// New 创建钉钉适配器
func New(cfg config.DingtalkConfig) *DingtalkAdapter {
	a := &DingtalkAdapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
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

// Attach 注册消息处理器，但不启动独立 HTTP 服务器。
func (a *DingtalkAdapter) Attach(handler adapter.MessageHandler) error {
	a.handler = handler
	return nil
}

// Start 启动钉钉 Webhook 服务器
func (a *DingtalkAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	if err := a.Attach(handler); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /dingtalk/webhook", a.handleWebhook)

	port := a.cfg.WebhookPort
	if port <= 0 {
		port = 6062
	}
	addr := fmt.Sprintf(":%d", port)

	a.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	log.Printf("钉钉适配器 [%s] 已启动: %s/dingtalk/webhook", a.Name(), addr)
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("钉钉 Webhook 服务器错误: %v", err)
		}
	}()

	return nil
}

// Stop 停止钉钉适配器
func (a *DingtalkAdapter) Stop(ctx context.Context) error {
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	if a.server == nil {
		return nil
	}
	return a.server.Shutdown(ctx)
}

// Handler 返回统一 ingress 使用的处理器。
func (a *DingtalkAdapter) Handler() http.Handler {
	return http.HandlerFunc(a.handleWebhook)
}

// Send 发送消息
func (a *DingtalkAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
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
	return a.Send(ctx, chatID, &adapter.Reply{Content: sb.String()})
}

// handleWebhook 处理钉钉回调
func (a *DingtalkAdapter) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	// 验证签名
	timestamp := r.Header.Get("timestamp")
	sign := r.Header.Get("sign")
	if a.cfg.AppSecret != "" && !a.verifySign(timestamp, sign) {
		log.Println("钉钉: 签名验证失败")
		http.Error(w, "签名验证失败", http.StatusUnauthorized)
		return
	}

	// 解析消息
	var event dtEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "解析事件失败", http.StatusBadRequest)
		return
	}

	if event.Text.Content != "" {
		go a.handleMessage(event)
	}

	w.WriteHeader(http.StatusOK)
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
		log.Printf("钉钉: 处理消息失败: %v", err)
		_ = a.Send(ctx, msg.ChatID, &adapter.Reply{Content: "处理消息时出现错误，请稍后重试。"})
		return
	}
	if reply == nil {
		return
	}

	if err := a.Send(ctx, msg.ChatID, reply); err != nil {
		log.Printf("钉钉: 发送回复失败: %v", err)
	}
}

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

// verifySign 验证钉钉签名
func (a *DingtalkAdapter) verifySign(timestamp, sign string) bool {
	if timestamp == "" || sign == "" {
		return false
	}
	stringToSign := timestamp + "\n" + a.cfg.AppSecret
	h := hmac.New(sha256.New, []byte(a.cfg.AppSecret))
	h.Write([]byte(stringToSign))
	expected := base64.StdEncoding.EncodeToString(h.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sign))
}

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

// marshalTextContent 安全地序列化文本消息内容为 JSON 字符串
func marshalTextContent(text string) string {
	b, _ := json.Marshal(map[string]string{"content": text})
	return string(b)
}

// Health 返回适配器健康状态。
func (a *DingtalkAdapter) Health(_ context.Context) error {
	if a.handler == nil {
		return fmt.Errorf("dingtalk handler 未附加")
	}
	if a.cfg.AppKey == "" || a.cfg.AppSecret == "" || a.cfg.RobotCode == "" {
		return fmt.Errorf("dingtalk app_key/app_secret/robot_code 未配置")
	}
	return nil
}
