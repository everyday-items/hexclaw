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

	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/hexclaw/internal/config"
	"github.com/everyday-items/toolkit/util/idgen"
)

const apiBase = "https://api.dingtalk.com"

// DingtalkAdapter 钉钉 Bot 适配器
type DingtalkAdapter struct {
	cfg     config.DingtalkConfig
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
}

// New 创建钉钉适配器
func New(cfg config.DingtalkConfig) *DingtalkAdapter {
	return &DingtalkAdapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (a *DingtalkAdapter) Name() string              { return "dingtalk" }
func (a *DingtalkAdapter) Platform() adapter.Platform { return adapter.PlatformDingtalk }

// Start 启动钉钉 Webhook 服务器
func (a *DingtalkAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler

	mux := http.NewServeMux()
	mux.HandleFunc("POST /dingtalk/webhook", a.handleWebhook)

	a.server = &http.Server{
		Addr:              ":6062",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	log.Println("钉钉适配器已启动: :6062/dingtalk/webhook")
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("钉钉 Webhook 服务器错误: %v", err)
		}
	}()

	return nil
}

// Stop 停止钉钉适配器
func (a *DingtalkAdapter) Stop(ctx context.Context) error {
	if a.server == nil {
		return nil
	}
	return a.server.Shutdown(ctx)
}

// Send 发送消息
func (a *DingtalkAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("获取 Access Token 失败: %w", err)
	}

	body, _ := json.Marshal(map[string]any{
		"robotCode": a.cfg.RobotCode,
		"userIds":   []string{chatID},
		"msgKey":    "sampleText",
		"msgParam":  fmt.Sprintf(`{"content":"%s"}`, escapeJSON(reply.Content)),
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
	defer r.Body.Close()

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
		ID:        "dt-" + idgen.ShortID(),
		Platform:  adapter.PlatformDingtalk,
		ChatID:    event.SenderStaffId,
		UserID:    event.SenderStaffId,
		UserName:  event.SenderNick,
		Content:   content,
		Timestamp: time.Now(),
		Metadata: map[string]string{
			"conversation_id": event.ConversationId,
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
	return expected == sign
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

// escapeJSON 转义 JSON 字符串
func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
