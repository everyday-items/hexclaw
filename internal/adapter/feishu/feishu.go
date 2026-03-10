// Package feishu 提供飞书（Lark）Bot 适配器
//
// 通过 Webhook 接收飞书事件回调，将消息转换为统一格式交给引擎处理。
// 回复通过飞书 OpenAPI 发送。
//
// 支持的功能：
//   - 接收文本消息（单聊/群聊 @机器人）
//   - URL 验证（challenge 回调）
//   - Access Token 缓存（2 小时有效期）
//   - 消息回复（同步/流式拼接后发送）
package feishu

import (
	"bytes"
	"context"
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

const (
	// 飞书 OpenAPI 基础地址
	baseURL = "https://open.feishu.cn/open-apis"
	// Token 提前刷新时间（5 分钟）
	tokenRefreshBuffer = 5 * time.Minute
)

// FeishuAdapter 飞书 Bot 适配器
//
// 通过 HTTP Webhook 接收飞书事件，将文本消息转换为统一 Message。
// 回复通过飞书 OpenAPI 发送。
type FeishuAdapter struct {
	cfg     config.FeishuConfig
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client

	// Access Token 缓存
	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
}

// New 创建飞书适配器
func New(cfg config.FeishuConfig) *FeishuAdapter {
	return &FeishuAdapter{
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (a *FeishuAdapter) Name() string              { return "feishu" }
func (a *FeishuAdapter) Platform() adapter.Platform { return adapter.PlatformFeishu }

// Start 启动飞书 Webhook 服务器
//
// 监听 /feishu/webhook 路径，接收飞书事件回调。
// 服务器默认监听 :6061 端口（与主 API 分开）。
func (a *FeishuAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler

	mux := http.NewServeMux()
	mux.HandleFunc("POST /feishu/webhook", a.handleWebhook)

	a.server = &http.Server{
		Addr:              ":6061",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	log.Println("飞书适配器已启动: :6061/feishu/webhook")

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("飞书 Webhook 服务器错误: %v", err)
		}
	}()

	return nil
}

// Stop 停止飞书适配器
func (a *FeishuAdapter) Stop(ctx context.Context) error {
	if a.server == nil {
		return nil
	}
	log.Println("飞书适配器停止中...")
	return a.server.Shutdown(ctx)
}

// Send 发送同步回复
//
// 通过飞书 OpenAPI 发送文本消息到指定会话。
func (a *FeishuAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("获取 Access Token 失败: %w", err)
	}

	// 构建飞书发送消息请求
	body := map[string]any{
		"receive_id": chatID,
		"msg_type":   "text",
		"content":    fmt.Sprintf(`{"text":"%s"}`, escapeJSON(reply.Content)),
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("飞书 API 返回 %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// SendStream 发送流式回复
//
// 消费 chunk channel，拼接所有内容后一次性发送。
// 飞书不支持真正的流式推送，所以先拼接再发送。
func (a *FeishuAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
	}

	return a.Send(ctx, chatID, &adapter.Reply{Content: sb.String()})
}

// Handler 返回 HTTP Handler（供外部挂载使用）
func (a *FeishuAdapter) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /feishu/webhook", a.handleWebhook)
	return mux
}

// handleWebhook 处理飞书事件回调
func (a *FeishuAdapter) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// 解析通用事件结构
	var event feishuEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "解析事件失败", http.StatusBadRequest)
		return
	}

	// URL 验证请求（challenge）
	if event.Challenge != "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"challenge": event.Challenge,
		})
		return
	}

	// 验证请求来源
	if event.Header.Token != "" && a.cfg.VerificationToken != "" {
		if event.Header.Token != a.cfg.VerificationToken {
			log.Printf("飞书事件验证失败: Token 不匹配")
			http.Error(w, "验证失败", http.StatusUnauthorized)
			return
		}
	}

	// 处理消息事件
	if event.Header.EventType == "im.message.receive_v1" {
		go a.handleMessage(event)
	}

	// 飞书要求 200 响应
	w.WriteHeader(http.StatusOK)
}

// handleMessage 处理消息事件
func (a *FeishuAdapter) handleMessage(event feishuEvent) {
	if a.handler == nil {
		return
	}

	// 提取消息内容
	msgEvent := event.Event
	if msgEvent.Message.MessageType != "text" {
		log.Printf("飞书: 暂不支持 %s 类型消息", msgEvent.Message.MessageType)
		return
	}

	// 解析文本内容（飞书文本消息格式: {"text":"内容"}）
	var textContent struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(msgEvent.Message.Content), &textContent); err != nil {
		log.Printf("飞书: 解析文本内容失败: %v", err)
		return
	}

	// 去掉 @机器人 的内容
	content := strings.TrimSpace(textContent.Text)
	if content == "" {
		return
	}

	// 构建统一消息
	msg := &adapter.Message{
		ID:        "feishu-" + idgen.ShortID(),
		Platform:  adapter.PlatformFeishu,
		ChatID:    msgEvent.Message.ChatID,
		UserID:    msgEvent.Sender.SenderID.OpenID,
		UserName:  msgEvent.Sender.SenderID.OpenID, // 飞书需要额外 API 获取用户名
		Content:   content,
		Timestamp: time.Now(),
		Metadata: map[string]string{
			"message_id":   msgEvent.Message.MessageID,
			"chat_type":    msgEvent.Message.ChatType,
			"message_type": msgEvent.Message.MessageType,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	reply, err := a.handler(ctx, msg)
	if err != nil {
		log.Printf("飞书: 处理消息失败: %v", err)
		// 发送错误提示
		_ = a.Send(ctx, msg.ChatID, &adapter.Reply{
			Content: "处理消息时出现错误，请稍后重试。",
		})
		return
	}

	if err := a.Send(ctx, msg.ChatID, reply); err != nil {
		log.Printf("飞书: 发送回复失败: %v", err)
	}
}

// getAccessToken 获取飞书 Tenant Access Token（带缓存）
//
// Token 有效期 2 小时，提前 5 分钟刷新。
func (a *FeishuAdapter) getAccessToken(ctx context.Context) (string, error) {
	a.mu.RLock()
	if a.accessToken != "" && time.Now().Before(a.tokenExpiry.Add(-tokenRefreshBuffer)) {
		token := a.accessToken
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	// 需要刷新
	a.mu.Lock()
	defer a.mu.Unlock()

	// 双重检查
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
		Expire            int    `json:"expire"` // 秒
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析 Token 响应失败: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("获取 Token 失败: code=%d, msg=%s", result.Code, result.Msg)
	}

	a.accessToken = result.TenantAccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(result.Expire) * time.Second)
	log.Printf("飞书 Access Token 已刷新, 有效期 %d 秒", result.Expire)

	return a.accessToken, nil
}

// feishuEvent 飞书事件通用结构
type feishuEvent struct {
	// URL 验证
	Challenge string `json:"challenge,omitempty"`
	Token     string `json:"token,omitempty"`
	Type      string `json:"type,omitempty"`

	// 事件通用头
	Header struct {
		EventID    string `json:"event_id"`
		EventType  string `json:"event_type"`
		CreateTime string `json:"create_time"`
		Token      string `json:"token"`
		AppID      string `json:"app_id"`
	} `json:"header"`

	// 消息事件
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

// escapeJSON 转义 JSON 字符串中的特殊字符
func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
