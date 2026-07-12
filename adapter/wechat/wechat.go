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
	"crypto/aes"
	"crypto/cipher"
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

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/adapter/whauth"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/net/httpx"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const wechatAPIBase = "https://api.weixin.qq.com/cgi-bin"

// replayWindow 微信回调时间戳防重放窗口。
//
// 微信请求在 query 中携带 timestamp（Unix 秒），配合 whauth.CheckReplayWindow
// 校验其是否落在 [now-replayWindow, now+容忍] 内，超窗即视为重放攻击予以拒绝。
// 取 5 分钟在容忍正常网络/时钟抖动的同时保持足够的重放防护强度。
const replayWindow = 5 * time.Minute

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
	aesKey      []byte // 安全模式下消息解密用的 AES Key（由 EncodingAESKey 解码而来）
}

// New 创建微信公众号适配器
func New(cfg config.WechatConfig) *WechatAdapter {
	a := &WechatAdapter{
		cfg:    cfg,
		client: httpx.RawClient(httpx.WithRawTimeout(10 * time.Second)),
	}
	// 安全模式：解码 EncodingAESKey（43 字符 Base64，需补 "=" 后再解码为 32 字节 AES-256 Key）。
	if cfg.AESKey != "" {
		key, err := base64.StdEncoding.DecodeString(cfg.AESKey + "=")
		if err == nil {
			a.aesKey = key
		}
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
			logger.Error("error", "error", err)
		}
	}()

	logger.Info("微信公众号适配器已启动")
	return nil
}

// Stop 停止适配器
func (a *WechatAdapter) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var queueErr error
	if a.queue != nil {
		queueErr = a.queue.Stop(ctx)
	}
	if a.server != nil {
		if err := a.server.Shutdown(ctx); err != nil {
			return err
		}
	}
	return queueErr
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
	// LaTeX 数学降级（BUG-20260712-P）：微信纯文本不渲染 LaTeX，出站前转 Unicode 符号。
	return a.sendCustomMessage(ctx, chatID, adapter.NormalizeMathText(reply.Content))
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
	// v0.4.0 E2：剥离 <think>/<thinking>/<reasoning> 防泄漏给终端用户
	return a.sendCustomMessage(ctx, chatID, adapter.StripThinking(sb.String()))
}

// ============== 回调处理 ==============

// handleVerify 处理微信 URL 验证
func (a *WechatAdapter) handleVerify(w http.ResponseWriter, r *http.Request) {
	signature := r.URL.Query().Get("signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	echoStr := r.URL.Query().Get("echostr")

	// W3-22：先校验时间戳重放窗口，再校验签名，超窗即拒绝。
	if err := a.checkTimestamp(timestamp); err != nil {
		http.Error(w, "name", http.StatusForbidden)
		return
	}
	if a.checkSignature(signature, timestamp, nonce) {
		_, _ = w.Write([]byte(echoStr))
	} else {
		http.Error(w, "name", http.StatusForbidden)
	}
}

// checkTimestamp 校验微信回调携带的时间戳是否落在防重放窗口内。
//
// W3-22：仅校验签名而不校验时间戳时，攻击者可重放抓包到的合法请求。
// 这里把 query 中的 timestamp（Unix 秒字符串）解析后交给
// whauth.CheckReplayWindow 统一校验，超窗（过旧/过新）或非法时间戳一律拒绝。
func (a *WechatAdapter) checkTimestamp(timestamp string) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("微信回调时间戳非法 %q: %w", timestamp, err)
	}
	if err := whauth.CheckReplayWindow(ts, replayWindow); err != nil {
		return fmt.Errorf("微信回调时间戳超出重放窗口: %w", err)
	}
	return nil
}

// handleMessage 处理消息回调
func (a *WechatAdapter) handleMessage(w http.ResponseWriter, r *http.Request) {
	// 验证签名
	signature := r.URL.Query().Get("signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	// W3-22：先校验时间戳重放窗口，再校验签名，超窗即拒绝。
	if err := a.checkTimestamp(timestamp); err != nil {
		http.Error(w, "name", http.StatusForbidden)
		return
	}
	if !a.checkSignature(signature, timestamp, nonce) {
		http.Error(w, "name", http.StatusForbidden)
		return
	}

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

	// W3-23：安全模式下消息体经过 AES 加密，需先校验 msg_signature 再解密。
	// 明文模式下 body 即为明文 XML，直接解析。
	if a.secureModeEnabled() {
		var err error
		body, err = a.decryptCallbackBody(r, body)
		if err != nil {
			// 解密/签名校验失败统一按拒绝处理，区分签名(403)与解密(500)。
			if errors.Is(err, whauth.ErrSignatureMismatch) {
				http.Error(w, "name", http.StatusForbidden)
			} else {
				http.Error(w, "解密消息失败", http.StatusInternalServerError)
			}
			return
		}
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

		// H7: Detach(ctx) 保留 logger/Values，脱离 4.5s 被动回复超时 —— handler 可能要跑更久
		reply, err := a.handler(trace.Detach(ctx), unified)
		if err != nil {
			logger.Error("error", "error", err)
			return
		}
		if reply != nil {
			select {
			case replyCh <- adapter.NormalizeMathText(reply.Content): // 被动回复同样降级（BUG-20260712-P）
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
		// W3-24：被动回复 XML 编码/写入失败不应被静默吞掉，记录日志便于排障。
		if err := xml.NewEncoder(w).Encode(replyXML); err != nil {
			logger.Error("微信被动回复 XML 编码写入失败", "error", err, "to_user", msg.FromUserName)
		}

	case <-ctx.Done():
		// 超时，先返回空响应，后续通过客服消息推送
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))

		// 等待异步结果并通过客服消息发送
		go func() {
			select {
			case content := <-replyCh:
				// H7: Detach 保留 logger/Values，脱离已取消的 ctx 发送客服消息
				_ = a.Send(trace.Detach(ctx), msg.FromUserName, &adapter.Reply{Content: content})
			case <-time.After(120 * time.Second):
				// 超时放弃
			}
		}()
	}
}

// ============== 签名验证 ==============

// checkSignature 验证微信请求签名
//
// 微信约定：将 Token、timestamp、nonce 三者按字典序排序拼接后做 SHA1，
// 与请求携带的 signature 比对。本方法保留该原生算法，但在安全上做两点加固：
//
//   - W3-21：Token 未配置（空串）时一律拒绝（fail-closed）。否则签名只取决于
//     timestamp+nonce，任何外部调用方都能伪造出"合法"签名绕过鉴权。
//     这里复用 whauth.RequireSecret 统一强制密钥非空。
//   - 比对改用 whauth.ConstantTimeEqualString 做常量时间比较，避免时序侧信道泄露。
func (a *WechatAdapter) checkSignature(signature, timestamp, nonce string) bool {
	// fail-closed：Token 为空直接拒绝，杜绝空密钥下的签名伪造。
	if whauth.RequireSecret(a.Name(), a.cfg.Token) != nil {
		return false
	}

	strs := []string{a.cfg.Token, timestamp, nonce}
	sort.Strings(strs)
	combined := strings.Join(strs, "")

	hash := sha1.Sum([]byte(combined))
	expected := fmt.Sprintf("%x", hash)

	return whauth.ConstantTimeEqualString(expected, signature)
}

// checkMsgSignature 验证安全模式下的 msg_signature。
//
// 微信安全模式约定：将 Token、timestamp、nonce、密文 encrypt 四者按字典序排序
// 拼接后做 SHA1，与请求携带的 msg_signature 比对。与 checkSignature 同样的
// 安全加固：Token 为空 fail-closed（W3-21），并用常量时间比较防时序侧信道。
func (a *WechatAdapter) checkMsgSignature(msgSignature, timestamp, nonce, encrypt string) bool {
	if whauth.RequireSecret(a.Name(), a.cfg.Token) != nil {
		return false
	}

	strs := []string{a.cfg.Token, timestamp, nonce, encrypt}
	sort.Strings(strs)
	combined := strings.Join(strs, "")

	hash := sha1.Sum([]byte(combined))
	expected := fmt.Sprintf("%x", hash)

	return whauth.ConstantTimeEqualString(expected, msgSignature)
}

// secureModeEnabled 报告是否启用了安全模式（已配置可用的 EncodingAESKey）。
func (a *WechatAdapter) secureModeEnabled() bool {
	return len(a.aesKey) > 0
}

// decrypt 解密微信安全模式下的消息密文（AES-256-CBC + PKCS7）。
//
// W3-23：实现安全模式解密。明文格式与企业微信一致：
//
//	16 字节随机串 + 4 字节消息长度(BigEndian) + 消息内容 + AppID
//
// IV 取 AES Key 的前 16 字节。返回去除随机串/长度头/AppID 尾部后的消息正文。
func (a *WechatAdapter) decrypt(encrypted string) (string, error) {
	if len(a.aesKey) == 0 {
		return "", fmt.Errorf("微信 aes key 未配置")
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}

	block, err := aes.NewCipher(a.aesKey)
	if err != nil {
		return "", fmt.Errorf("创建 AES Cipher 失败: %w", err)
	}

	if len(ciphertext) < aes.BlockSize || len(ciphertext)%aes.BlockSize != 0 {
		return "", fmt.Errorf("密文长度非法")
	}

	iv := a.aesKey[:aes.BlockSize]
	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(ciphertext, ciphertext)

	// 去除 PKCS7 填充（完整校验所有填充字节，避免被构造的非法填充蒙混）。
	padLen := int(ciphertext[len(ciphertext)-1])
	if padLen <= 0 || padLen > aes.BlockSize || padLen > len(ciphertext) {
		return "", fmt.Errorf("无效的 PKCS7 填充")
	}
	for i := 0; i < padLen; i++ {
		if ciphertext[len(ciphertext)-1-i] != byte(padLen) {
			return "", fmt.Errorf("无效的 PKCS7 填充")
		}
	}
	ciphertext = ciphertext[:len(ciphertext)-padLen]

	// 解析格式：16 字节随机串 + 4 字节消息长度 + 消息内容 + AppID。
	if len(ciphertext) < 20 {
		return "", fmt.Errorf("解密后数据太短")
	}
	msgLen := binary.BigEndian.Uint32(ciphertext[16:20])
	if int(msgLen) > len(ciphertext)-20 {
		return "", fmt.Errorf("消息长度不匹配")
	}

	return string(ciphertext[20 : 20+msgLen]), nil
}

// decryptCallbackBody 校验并解密安全模式下的回调消息体。
//
// W3-23：安全模式消息体形如 <xml><Encrypt><![CDATA[...]]></Encrypt>...</xml>。
// 校验链：解析 Encrypt 密文 -> 用 msg_signature 校验(checkMsgSignature) -> AES 解密。
// 校验失败返回包装 whauth.ErrSignatureMismatch 的错误，解密失败返回原始错误，
// 由调用方据此映射 403/500。返回解密后的明文 XML 字节供后续统一解析。
func (a *WechatAdapter) decryptCallbackBody(r *http.Request, body []byte) ([]byte, error) {
	var enc struct {
		Encrypt string `xml:"Encrypt"`
	}
	if err := xml.Unmarshal(body, &enc); err != nil {
		return nil, fmt.Errorf("解析加密 XML 失败: %w", err)
	}

	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	msgSignature := r.URL.Query().Get("msg_signature")
	if !a.checkMsgSignature(msgSignature, timestamp, nonce, enc.Encrypt) {
		return nil, fmt.Errorf("安全模式 msg_signature 校验失败: %w", whauth.ErrSignatureMismatch)
	}

	plaintext, err := a.decrypt(enc.Encrypt)
	if err != nil {
		return nil, fmt.Errorf("安全模式解密失败: %w", err)
	}
	return []byte(plaintext), nil
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
