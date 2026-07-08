// Package wecom 提供企业微信 Bot 适配器
//
// 通过 HTTP 回调接收企业微信应用消息，
// 通过企业微信 API 发送回复。
// 消息体使用 AES-256-CBC 加解密。
//
// 配置方式：
//  1. 企业微信管理后台创建自建应用
//  2. 设置接收消息 URL 为 http://your-host:6064/wecom/callback
//  3. 获取 Token、EncodingAESKey、CorpID、AgentID、Secret
//
// 对标 OpenClaw 企业微信集成。
package wecom

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/hexagon-codes/toolkit/util/logger"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/adapter/whauth"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/net/httpx"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const wecomAPIBase = "https://qyapi.weixin.qq.com/cgi-bin"

// wecomReplayWindow 是 webhook 时间戳允许回看的重放窗口。
//
// W3-27 安全：企业微信签名把 timestamp 纳入 SHA1 计算，但源码原先从不比较
// timestamp 与当前时间，导致任意曾经合法的请求可被无限重放。此处以 5 分钟
// 作为向过去回看的容忍窗口（whauth.CheckReplayWindow 另对未来侧额外放宽 5 分钟
// 时钟漂移），过旧的请求一律拒绝。
const wecomReplayWindow = 5 * time.Minute

// WecomAdapter 企业微信适配器
//
// 支持被动回复和主动推送两种消息方式。
// 使用 AES 加密保障消息安全。
type WecomAdapter struct {
	cfg     config.WecomConfig
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client
	queue   *adapter.SendQueue

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
	aesKey      []byte // 解密用的 AES Key
}

// New 创建企业微信适配器
func New(cfg config.WecomConfig) *WecomAdapter {
	a := &WecomAdapter{
		cfg:    cfg,
		client: httpx.RawClient(httpx.WithRawTimeout(10 * time.Second)),
	}
	// 解码 EncodingAESKey（Base64 + 补 "="）
	if cfg.AESKey != "" {
		key, err := base64.StdEncoding.DecodeString(cfg.AESKey + "=")
		if err == nil {
			a.aesKey = key
		}
	}
	a.queue = adapter.NewPlatformSendQueue(adapter.PlatformWecom, a.sendReplyNow)
	return a
}

func (a *WecomAdapter) Name() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return "wecom"
}
func (a *WecomAdapter) Platform() adapter.Platform { return adapter.PlatformWecom }

// Attach 注册消息处理器，但不启动独立 HTTP 服务器。
func (a *WecomAdapter) Attach(handler adapter.MessageHandler) error {
	a.handler = handler
	if a.cfg.CorpID == "" || a.cfg.Secret == "" {
		return fmt.Errorf("企业微信 CorpID 和 Secret 不能为空")
	}
	return nil
}

// Start 启动回调服务器
func (a *WecomAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	if err := a.Attach(handler); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /wecom/callback", a.handleVerify)
	mux.HandleFunc("POST /wecom/callback", a.handleCallback)

	port := a.cfg.WebhookPort
	if port <= 0 {
		port = 6064
	}
	addr := fmt.Sprintf(":%d", port)

	a.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("error", "error", err)
		}
	}()

	logger.Info("企业微信适配器已启动")
	return nil
}

// Stop 停止适配器
func (a *WecomAdapter) Stop(ctx context.Context) error {
	if a.queue != nil {
		_ = a.queue.Stop(context.Background())
	}
	if a.server != nil {
		return a.server.Shutdown(ctx)
	}
	return nil
}

// Handler 返回统一 ingress 使用的处理器。
func (a *WecomAdapter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			a.handleVerify(w, r)
		case http.MethodPost:
			a.handleCallback(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// Send 发送消息
//
// v0.4.0 F6：reply.Interactive 非空且 flag interactive.render.v1 OFF 时，
// 自动追加文本 fallback 让按钮/选项/审批/卡片在企业微信基础可用。
func (a *WecomAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	adapter.MaybeApplyTextFallback(ctx, reply)
	if a.queue == nil {
		return a.sendReplyNow(ctx, chatID, reply)
	}
	return a.queue.Send(ctx, chatID, reply)
}

func (a *WecomAdapter) sendReplyNow(ctx context.Context, chatID string, reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给终端用户
	return a.sendTextMessage(ctx, chatID, adapter.StripThinking(reply.Content))
}

// SendStream 流式发送（企业微信不支持消息编辑，直接合并后发送）
func (a *WecomAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	var sb strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	// v0.4.0 E2：流式分支同样 strip thinking（绕过 queue 直发）
	return a.sendTextMessage(ctx, chatID, adapter.StripThinking(sb.String()))
}

// ============== 回调处理 ==============

// handleVerify 处理企业微信 URL 验证回调
func (a *WecomAdapter) handleVerify(w http.ResponseWriter, r *http.Request) {
	msgSignature := r.URL.Query().Get("msg_signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	echoStr := r.URL.Query().Get("echostr")

	// 验证签名
	if !a.checkSignature(msgSignature, timestamp, nonce, echoStr) {
		http.Error(w, "name", http.StatusForbidden)
		return
	}

	// W3-27 安全：签名通过后还需校验时间戳重放窗口，拒绝过旧/过新的请求。
	if err := a.checkReplayWindow(timestamp); err != nil {
		logger.Warn("企业微信 URL 验证时间戳超出重放窗口", "error", err)
		http.Error(w, "timestamp out of replay window", http.StatusForbidden)
		return
	}

	// 解密 echostr
	plaintext, err := a.decrypt(echoStr)
	if err != nil {
		http.Error(w, "解密失败", http.StatusInternalServerError)
		return
	}

	_, _ = w.Write([]byte(plaintext))
}

// handleCallback 处理消息回调
func (a *WecomAdapter) handleCallback(w http.ResponseWriter, r *http.Request) {
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

	// 解析加密的 XML
	var encMsg struct {
		ToUserName string `xml:"ToUserName"`
		Encrypt    string `xml:"Encrypt"`
		AgentID    string `xml:"AgentID"`
	}
	if err := xml.Unmarshal(body, &encMsg); err != nil {
		http.Error(w, "解析 XML 失败", http.StatusBadRequest)
		return
	}

	// 验证签名
	msgSignature := r.URL.Query().Get("msg_signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")

	if !a.checkSignature(msgSignature, timestamp, nonce, encMsg.Encrypt) {
		http.Error(w, "name", http.StatusForbidden)
		return
	}

	// W3-27 安全：签名通过后还需校验时间戳重放窗口，拒绝过旧/过新的请求。
	if err := a.checkReplayWindow(timestamp); err != nil {
		logger.Warn("企业微信回调时间戳超出重放窗口", "error", err)
		http.Error(w, "timestamp out of replay window", http.StatusForbidden)
		return
	}

	// 解密消息
	plaintext, err := a.decrypt(encMsg.Encrypt)
	if err != nil {
		http.Error(w, "解密消息失败", http.StatusInternalServerError)
		return
	}

	// 解析明文消息
	var msg wecomMessage
	if err := xml.Unmarshal([]byte(plaintext), &msg); err != nil {
		http.Error(w, "error", http.StatusBadRequest)
		return
	}

	// 立即返回空响应
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("success"))

	// 异步处理消息
	go a.processMessage(msg)
}

// processMessage 异步处理消息
func (a *WecomAdapter) processMessage(msg wecomMessage) {
	// W3-28 功能闭环：校验消息所属应用与配置的 AgentID 一致。
	// 企业微信明文消息携带 AgentID，若与本适配器配置的 AgentID 不符，
	// 说明该回调并非发给本应用（多应用共用回调 URL 场景下可能串号），
	// 直接丢弃并记录，避免错误地把其它应用的消息当作自身消息处理。
	// 仅当配置了 AgentID 时才强制校验（msg.AgentID==0 多为非应用消息，放行兼容）。
	if a.cfg.AgentID != "" && msg.AgentID != 0 &&
		!whauth.ConstantTimeEqualString(strconv.Itoa(msg.AgentID), a.cfg.AgentID) {
		logger.Warn("企业微信消息 AgentID 与配置不一致，丢弃",
			"msg_agent_id", msg.AgentID, "cfg_agent_id", a.cfg.AgentID)
		return
	}

	// W3-28 功能闭环：仅处理文本消息，其它类型记录日志后丢弃，
	// 避免静默吞掉非 text 消息导致排障困难。
	if msg.MsgType != "text" {
		logger.Info("企业微信收到暂不支持的消息类型，已忽略",
			"msg_type", msg.MsgType, "from", msg.FromUserName)
		return
	}

	unified := &adapter.Message{
		ID:         "wecom-" + idgen.ShortID(),
		Platform:   adapter.PlatformWecom,
		InstanceID: a.Name(),
		ChatID:     msg.FromUserName,
		UserID:     msg.FromUserName,
		Content:    msg.Content,
		Timestamp:  time.Now(),
		Metadata: map[string]string{
			"agent_id": a.cfg.AgentID,
			"msg_id":   fmt.Sprintf("%d", msg.MsgID),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reply, err := a.handler(ctx, unified)
	if err != nil {
		logger.Error("error", "error", err)
		return
	}
	if reply != nil {
		sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer sendCancel()
		if err := a.Send(sendCtx, msg.FromUserName, reply); err != nil {
			logger.Error("error", "error", err)
		}
	}
}

// ============== API 调用 ==============

// getAccessToken 获取 Access Token（带缓存）
func (a *WecomAdapter) getAccessToken(ctx context.Context) (string, error) {
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

	url := fmt.Sprintf("%s/gettoken?corpid=%s&corpsecret=%s", wecomAPIBase, a.cfg.CorpID, a.cfg.Secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取企业微信 Access Token 失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析企业微信 Token 响应失败: %w", err)
	}

	if result.ErrCode != 0 {
		return "", fmt.Errorf("企业微信 API 错误 (%d): %s", result.ErrCode, result.ErrMsg)
	}

	a.accessToken = result.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(result.ExpiresIn-300) * time.Second) // 提前 5 分钟过期
	return a.accessToken, nil
}

// sendTextMessage 发送文本消息
func (a *WecomAdapter) sendTextMessage(ctx context.Context, toUser, content string) error {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return err
	}

	body := map[string]any{
		"touser":  toUser,
		"msgtype": "text",
		"agentid": a.cfg.AgentID,
		"text": map[string]string{
			"content": content,
		},
	}
	data, _ := json.Marshal(body)

	url := fmt.Sprintf("%s/message/send?access_token=%s", wecomAPIBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送企业微信消息失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("解析企业微信发送响应失败: %w", err)
	}

	if result.ErrCode != 0 {
		return fmt.Errorf("企业微信发送消息失败 (%d): %s", result.ErrCode, result.ErrMsg)
	}
	return nil
}

// ValidateConfig validates credentials by attempting to fetch an access token.
func (a *WecomAdapter) ValidateConfig(ctx context.Context) error {
	if a.cfg.CorpID == "" || a.cfg.Secret == "" {
		return fmt.Errorf("wecom corp_id/secret 未配置")
	}
	if a.cfg.AgentID == "" {
		return fmt.Errorf("wecom agent_id 未配置")
	}
	if a.cfg.Token == "" {
		return fmt.Errorf("wecom token 未配置")
	}
	if a.cfg.AESKey == "" {
		return fmt.Errorf("wecom encoding_aes_key 未配置")
	}
	_, err := a.getAccessToken(ctx)
	return err
}

// Health 返回适配器健康状态。
func (a *WecomAdapter) Health(_ context.Context) error {
	if a.handler == nil {
		return fmt.Errorf("wecom handler 未附加")
	}
	if a.cfg.CorpID == "" || a.cfg.Secret == "" || a.cfg.AgentID == "" {
		return fmt.Errorf("wecom corp_id/secret/agent_id 未配置")
	}
	if len(a.aesKey) == 0 {
		return fmt.Errorf("wecom aes key 未配置")
	}
	return nil
}

// ============== 加解密 ==============

// checkReplayWindow 校验 webhook 时间戳是否落在重放窗口内，防止重放攻击。
//
// W3-27 安全：复用共享包 whauth.CheckReplayWindow 统一策略，过旧/过新一律拒绝。
// timestamp 为企业微信回调携带的 Unix 秒字符串；无法解析或超窗均返回 error，
// 调用方据此 fail-closed 返回 403。
func (a *WecomAdapter) checkReplayWindow(timestamp string) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("非法 timestamp %q: %w", timestamp, err)
	}
	return whauth.CheckReplayWindow(ts, wecomReplayWindow)
}

// checkSignature 验证签名
func (a *WecomAdapter) checkSignature(msgSignature, timestamp, nonce, encrypt string) bool {
	strs := []string{a.cfg.Token, timestamp, nonce, encrypt}
	sort.Strings(strs)
	combined := strings.Join(strs, "")

	hash := sha1.Sum([]byte(combined))
	expected := fmt.Sprintf("%x", hash)

	return hmac.Equal([]byte(expected), []byte(msgSignature))
}

// decrypt 解密消息
func (a *WecomAdapter) decrypt(encrypted string) (string, error) {
	if len(a.aesKey) == 0 {
		return "", fmt.Errorf("aes key 未配置")
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}

	block, err := aes.NewCipher(a.aesKey)
	if err != nil {
		return "", fmt.Errorf("创建 AES Cipher 失败: %w", err)
	}

	if len(ciphertext) < aes.BlockSize {
		return "", fmt.Errorf("密文太短")
	}

	// W3-25 边界/安全：CBC 解密要求密文长度为块大小的整数倍，
	// 否则 cipher.CryptBlocks 会 panic。攻击者只要知道 Token 即可构造
	// 一个长度 >= 16 但非 16 倍数的密文，签名通过后触发 panic，
	// 在同步请求路径上崩溃处理 goroutine（远程 DoS）。
	// 这里在 CryptBlocks 之前显式校验整块对齐，非整块直接返回错误。
	if len(ciphertext)%aes.BlockSize != 0 {
		return "", fmt.Errorf("密文长度非 AES 块大小整数倍: len=%d", len(ciphertext))
	}

	iv := a.aesKey[:aes.BlockSize]
	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(ciphertext, ciphertext)

	// 去除 PKCS7 填充（完整校验所有填充字节）
	padLen := int(ciphertext[len(ciphertext)-1])
	if padLen > aes.BlockSize || padLen == 0 || padLen > len(ciphertext) {
		return "", fmt.Errorf("无效的 PKCS7 填充")
	}
	for i := 0; i < padLen; i++ {
		if ciphertext[len(ciphertext)-1-i] != byte(padLen) {
			return "", fmt.Errorf("无效的 PKCS7 填充")
		}
	}
	ciphertext = ciphertext[:len(ciphertext)-padLen]

	// 解析消息格式: 16 字节随机串 + 4 字节消息长度 + 消息内容 + CorpID
	if len(ciphertext) < 20 {
		return "", fmt.Errorf("解密后数据太短")
	}

	msgLen := binary.BigEndian.Uint32(ciphertext[16:20])
	if int(msgLen) > len(ciphertext)-20 {
		return "", fmt.Errorf("消息长度不匹配")
	}

	msg := string(ciphertext[20 : 20+msgLen])

	// W3-26 安全：校验信封尾部 CorpID 防伪造来源。
	// 企业微信信封在消息内容之后附带发送方 CorpID，官方规范要求解密后
	// 比对该 CorpID 与自身配置是否一致，否则无法识别伪造来源的请求。
	// 采用 fail-closed 策略：仅当配置了 CorpID 时强制校验，不一致即拒绝；
	// 比较使用常量时间方式（whauth.ConstantTimeEqualString）避免时序侧信道。
	if a.cfg.CorpID != "" {
		gotCorpID := string(ciphertext[20+msgLen:])
		if !whauth.ConstantTimeEqualString(gotCorpID, a.cfg.CorpID) {
			return "", fmt.Errorf("信封 CorpID 与配置不一致，疑似伪造来源")
		}
	}

	return msg, nil
}

// ============== 数据模型 ==============

// wecomMessage 企业微信消息
type wecomMessage struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
	MsgID        int64    `xml:"MsgId"`
	AgentID      int      `xml:"AgentID"`
}
