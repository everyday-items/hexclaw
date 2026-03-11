// Package wecom 提供企业微信 Bot 适配器
//
// 通过 HTTP 回调接收企业微信应用消息，
// 通过企业微信 API 发送回复。
// 消息体使用 AES-256-CBC 加解密。
//
// 配置方式：
//   1. 企业微信管理后台创建自建应用
//   2. 设置接收消息 URL 为 http://your-host:6064/wecom/callback
//   3. 获取 Token、EncodingAESKey、CorpID、AgentID、Secret
//
// 对标 OpenClaw 企业微信集成。
package wecom

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
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

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/toolkit/util/idgen"
)

const wecomAPIBase = "https://qyapi.weixin.qq.com/cgi-bin"

// WecomAdapter 企业微信适配器
//
// 支持被动回复和主动推送两种消息方式。
// 使用 AES 加密保障消息安全。
type WecomAdapter struct {
	cfg     config.WecomConfig
	handler adapter.MessageHandler
	server  *http.Server
	client  *http.Client

	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
	aesKey      []byte // 解密用的 AES Key
}

// New 创建企业微信适配器
func New(cfg config.WecomConfig) *WecomAdapter {
	a := &WecomAdapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	// 解码 EncodingAESKey（Base64 + 补 "="）
	if cfg.AESKey != "" {
		key, err := base64.StdEncoding.DecodeString(cfg.AESKey + "=")
		if err == nil {
			a.aesKey = key
		}
	}
	return a
}

func (a *WecomAdapter) Name() string              { return "wecom" }
func (a *WecomAdapter) Platform() adapter.Platform { return adapter.PlatformWecom }

// Start 启动回调服务器
func (a *WecomAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler

	if a.cfg.CorpID == "" || a.cfg.Secret == "" {
		return fmt.Errorf("企业微信 CorpID 和 Secret 不能为空")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /wecom/callback", a.handleVerify)
	mux.HandleFunc("POST /wecom/callback", a.handleCallback)

	a.server = &http.Server{
		Addr:              ":6064",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("企业微信回调服务器错误: %v", err)
		}
	}()

	log.Println("企业微信适配器已启动")
	return nil
}

// Stop 停止适配器
func (a *WecomAdapter) Stop(ctx context.Context) error {
	if a.server != nil {
		return a.server.Shutdown(ctx)
	}
	return nil
}

// Send 发送消息
func (a *WecomAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	return a.sendTextMessage(ctx, chatID, reply.Content)
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
	return a.sendTextMessage(ctx, chatID, sb.String())
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
		http.Error(w, "签名验证失败", http.StatusForbidden)
		return
	}

	// 解密 echostr
	plaintext, err := a.decrypt(echoStr)
	if err != nil {
		http.Error(w, "解密失败", http.StatusInternalServerError)
		return
	}

	w.Write([]byte(plaintext))
}

// handleCallback 处理消息回调
func (a *WecomAdapter) handleCallback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
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
		http.Error(w, "签名验证失败", http.StatusForbidden)
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
		http.Error(w, "解析消息失败", http.StatusBadRequest)
		return
	}

	// 立即返回空响应
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("success"))

	// 异步处理消息
	go a.processMessage(msg)
}

// processMessage 异步处理消息
func (a *WecomAdapter) processMessage(msg wecomMessage) {
	if msg.MsgType != "text" {
		return
	}

	unified := &adapter.Message{
		ID:        "wecom-" + idgen.ShortID(),
		Platform:  adapter.PlatformWecom,
		ChatID:    msg.FromUserName,
		UserID:    msg.FromUserName,
		Content:   msg.Content,
		Timestamp: time.Now(),
		Metadata: map[string]string{
			"agent_id": a.cfg.AgentID,
			"msg_id":   fmt.Sprintf("%d", msg.MsgID),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reply, err := a.handler(ctx, unified)
	if err != nil {
		log.Printf("企业微信消息处理失败: %v", err)
		return
	}
	if reply != nil {
		a.sendTextMessage(ctx, msg.FromUserName, reply.Content)
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
	json.NewDecoder(resp.Body).Decode(&result)

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
	json.NewDecoder(resp.Body).Decode(&result)

	if result.ErrCode != 0 {
		return fmt.Errorf("企业微信发送消息失败 (%d): %s", result.ErrCode, result.ErrMsg)
	}
	return nil
}

// ============== 加解密 ==============

// checkSignature 验证签名
func (a *WecomAdapter) checkSignature(msgSignature, timestamp, nonce, encrypt string) bool {
	strs := []string{a.cfg.Token, timestamp, nonce, encrypt}
	sort.Strings(strs)
	combined := strings.Join(strs, "")

	hash := sha1.Sum([]byte(combined))
	expected := fmt.Sprintf("%x", hash)

	return expected == msgSignature
}

// decrypt 解密消息
func (a *WecomAdapter) decrypt(encrypted string) (string, error) {
	if len(a.aesKey) == 0 {
		return "", fmt.Errorf("AES Key 未配置")
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("Base64 解码失败: %w", err)
	}

	block, err := aes.NewCipher(a.aesKey)
	if err != nil {
		return "", fmt.Errorf("创建 AES Cipher 失败: %w", err)
	}

	if len(ciphertext) < aes.BlockSize {
		return "", fmt.Errorf("密文太短")
	}

	iv := a.aesKey[:aes.BlockSize]
	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(ciphertext, ciphertext)

	// 去除 PKCS7 填充
	padLen := int(ciphertext[len(ciphertext)-1])
	if padLen > aes.BlockSize || padLen == 0 {
		return "", fmt.Errorf("无效的 PKCS7 填充")
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
