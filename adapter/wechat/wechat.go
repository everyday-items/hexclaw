// Package wechat 提供微信公众号适配器
//
// 通过 HTTP 回调接收微信公众号消息（XML 格式），
// 通过被动回复或客服消息接口发送回复。
//
// 配置方式：
//  1. 微信公众平台设置消息接收 URL: http://your-host:6065/wechat/callback
//  2. 获取 AppID、AppSecret、Token、EncodingAESKey
//
// 注意：
//   - 微信公众号被动回复要求 5 秒内响应
//   - 超时场景使用客服消息接口（需认证服务号）
//   - 未认证公众号只能被动回复
//
// 对标 OpenClaw 微信集成。
package wechat

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const wechatAPIBase = "https://api.weixin.qq.com/cgi-bin"

// WechatAdapter 微信公众号适配器
//
// 支持两种回复方式：
//   - 被动回复：5 秒内直接在 HTTP 响应中返回 XML 消息
//   - 客服消息：超时场景通过客服接口主动推送（需认证服务号）
type WechatAdapter struct {
	cfg     config.WechatConfig
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client
	queue   *adapter.SendQueue

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
}

// New 创建微信公众号适配器
func New(cfg config.WechatConfig) *WechatAdapter {
	a := &WechatAdapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	a.queue = adapter.NewPlatformSendQueue(adapter.PlatformWechat, a.sendReplyNow)
	return a
}

func (a *WechatAdapter) Name() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return "wechat"
}
func (a *WechatAdapter) Platform() adapter.Platform { return adapter.PlatformWechat }

// Attach 注册消息处理器，但不启动独立 HTTP 服务器。
func (a *WechatAdapter) Attach(handler adapter.MessageHandler) error {
	a.handler = handler
	if a.cfg.AppID == "" || a.cfg.AppSecret == "" {
		return fmt.Errorf("微信公众号 AppID 和 AppSecret 不能为空")
	}
	return nil
}

// Start 启动消息回调服务器
func (a *WechatAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	if err := a.Attach(handler); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /wechat/callback", a.handleVerify)
	mux.HandleFunc("POST /wechat/callback", a.handleMessage)

	a.server = &http.Server{
		Addr:              ":6065",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("微信回调服务器错误: %v", err)
		}
	}()

	log.Println("微信公众号适配器已启动")
	return nil
}

// Stop 停止适配器
func (a *WechatAdapter) Stop(ctx context.Context) error {
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	if a.server != nil {
		return a.server.Shutdown(ctx)
	}
	return nil
}

// Handler 返回统一 ingress 使用的处理器。
func (a *WechatAdapter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			a.handleVerify(w, r)
		case http.MethodPost:
			a.handleMessage(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// Send 发送消息（通过客服接口）
func (a *WechatAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

func (a *WechatAdapter) sendReplyNow(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	return a.sendCustomMessage(ctx, chatID, reply.Content)
}

// SendStream 流式发送（微信不支持消息编辑，合并后发送）
func (a *WechatAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	return a.sendCustomMessage(ctx, chatID, sb.String())
}

// ============== 回调处理 ==============

// handleVerify 处理微信 URL 验证
func (a *WechatAdapter) handleVerify(w http.ResponseWriter, r *http.Request) {
	signature := r.URL.Query().Get("signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	echoStr := r.URL.Query().Get("echostr")

	if a.checkSignature(signature, timestamp, nonce) {
		_, _ = w.Write([]byte(echoStr))
	} else {
		http.Error(w, "签名验证失败", http.StatusForbidden)
	}
}

// handleMessage 处理消息回调
func (a *WechatAdapter) handleMessage(w http.ResponseWriter, r *http.Request) {
	// 验证签名
	signature := r.URL.Query().Get("signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	if !a.checkSignature(signature, timestamp, nonce) {
		http.Error(w, "签名验证失败", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求失败", http.StatusBadRequest)
		return
	}

	var msg wechatMessage
	if err := xml.Unmarshal(body, &msg); err != nil {
		http.Error(w, "解析 XML 失败", http.StatusBadRequest)
		return
	}

	// 只处理文本消息
	if msg.MsgType != "text" {
		// 返回空响应（表示不处理）
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))
		return
	}

	// 尝试 5 秒内被动回复
	ctx, cancel := context.WithTimeout(r.Context(), 4500*time.Millisecond)

	replyCh := make(chan string, 1)
	go func() {
		defer cancel()
		unified := &adapter.Message{
			ID:         "wechat-" + idgen.ShortID(),
			Platform:   adapter.PlatformWechat,
			InstanceID: a.Name(),
			ChatID:     msg.FromUserName,
			UserID:     msg.FromUserName,
			Content:    msg.Content,
			Timestamp:  time.Now(),
			Metadata: map[string]string{
				"msg_id": fmt.Sprintf("%d", msg.MsgID),
			},
		}

		reply, err := a.handler(context.Background(), unified)
		if err != nil {
			log.Printf("微信消息处理失败: %v", err)
			return
		}
		if reply != nil {
			select {
			case replyCh <- reply.Content:
			default:
			}
		}
	}()

	// 等待处理结果或超时
	select {
	case content := <-replyCh:
		// 被动回复
		replyXML := wechatReplyText{
			ToUserName:   msg.FromUserName,
			FromUserName: msg.ToUserName,
			CreateTime:   time.Now().Unix(),
			MsgType:      "text",
			Content:      content,
		}
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(replyXML)

	case <-ctx.Done():
		// 超时，先返回空响应，后续通过客服消息推送
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))

		// 等待异步结果并通过客服消息发送
		go func() {
			select {
			case content := <-replyCh:
				bgCtx := context.Background()
				_ = a.Send(bgCtx, msg.FromUserName, &adapter.Reply{Content: content})
			case <-time.After(120 * time.Second):
				// 超时放弃
			}
		}()
	}
}

// ============== 签名验证 ==============

// checkSignature 验证微信请求签名
func (a *WechatAdapter) checkSignature(signature, timestamp, nonce string) bool {
	strs := []string{a.cfg.Token, timestamp, nonce}
	sort.Strings(strs)
	combined := strings.Join(strs, "")

	hash := sha1.Sum([]byte(combined))
	expected := fmt.Sprintf("%x", hash)

	return expected == signature
}

// ============== API 调用 ==============

// getAccessToken 获取 Access Token（带缓存）
func (a *WechatAdapter) getAccessToken(ctx context.Context) (string, error) {
	a.mu.RLock()
	if a.accessToken != "" && time.Now().Before(a.tokenExpiry) {
		token := a.accessToken
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	// 双检锁
	if a.accessToken != "" && time.Now().Before(a.tokenExpiry) {
		return a.accessToken, nil
	}

	url := fmt.Sprintf("%s/token?grant_type=client_credential&appid=%s&secret=%s",
		wechatAPIBase, a.cfg.AppID, a.cfg.AppSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取微信 Access Token 失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析微信 Token 响应失败: %w", err)
	}

	if result.ErrCode != 0 {
		return "", fmt.Errorf("微信 API 错误 (%d): %s", result.ErrCode, result.ErrMsg)
	}

	a.accessToken = result.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(result.ExpiresIn-300) * time.Second)
	return a.accessToken, nil
}

// sendCustomMessage 通过客服消息接口发送文本
func (a *WechatAdapter) sendCustomMessage(ctx context.Context, toUser, content string) error {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return err
	}

	body := map[string]any{
		"touser":  toUser,
		"msgtype": "text",
		"text": map[string]string{
			"content": content,
		},
	}
	data, _ := json.Marshal(body)

	url := fmt.Sprintf("%s/message/custom/send?access_token=%s", wechatAPIBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送微信客服消息失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("解析微信发送响应失败: %w", err)
	}

	if result.ErrCode != 0 {
		return fmt.Errorf("微信客服消息失败 (%d): %s", result.ErrCode, result.ErrMsg)
	}
	return nil
}

// ValidateConfig validates credentials by attempting to fetch an access token.
func (a *WechatAdapter) ValidateConfig(ctx context.Context) error {
	if a.cfg.AppID == "" || a.cfg.AppSecret == "" {
		return fmt.Errorf("wechat app_id/app_secret 未配置")
	}
	if a.cfg.Token == "" {
		return fmt.Errorf("wechat token 未配置")
	}
	_, err := a.getAccessToken(ctx)
	return err
}

// Health 返回适配器健康状态。
func (a *WechatAdapter) Health(_ context.Context) error {
	if a.handler == nil {
		return fmt.Errorf("wechat handler 未附加")
	}
	if a.cfg.AppID == "" || a.cfg.AppSecret == "" || a.cfg.Token == "" {
		return fmt.Errorf("wechat app_id/app_secret/token 未配置")
	}
	return nil
}

// ============== 数据模型 ==============

// wechatMessage 微信消息（XML 格式）
type wechatMessage struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
	MsgID        int64    `xml:"MsgId"`
}

// wechatReplyText 微信文本回复（XML 格式）
type wechatReplyText struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
}
