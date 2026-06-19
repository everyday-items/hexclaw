package wechat

// webhook_decrypt_test.go —— W3-23 安全模式（EncodingAESKey）解密链路回归测试。
//
// 覆盖：
//   - decrypt() 的 AES-256-CBC + PKCS7 解密 round-trip 与错误守卫
//   - decryptCallbackBody() 的 msg_signature 校验 -> 解密链路
//   - handleMessage() 在安全模式下端到端：密文请求经 msg_signature 校验、AES 解密、
//     XML 解析后正常触达业务 handler；签名/密文非法时分别按 403/500 拒绝。
//
// 加密信封格式与企业微信一致（wecom 已有同构测试）：
//
//	16 字节随机串 + 4 字节消息长度(BigEndian) + 消息内容 + AppID
//
// 全部基于包内真实函数，不 mock 被测逻辑本身。

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// 43 字符 EncodingAESKey，补 "=" 后 base64 解码为 32 字节 AES-256 Key。
const wechatTestAESKey = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY"

// wechatEncrypt 构造微信安全模式密文：
// 16 字节随机串 + 4 字节 BE 长度 + 消息 + AppID，PKCS7 填充后 AES-256-CBC 加密（IV = key[:16]）。
func wechatEncrypt(t *testing.T, key []byte, msg, appID string) string {
	t.Helper()
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(msg)))

	plain := append([]byte("0123456789abcdef"), lenBuf[:]...)
	plain = append(plain, msg...)
	plain = append(plain, appID...)

	padLen := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < padLen; i++ {
		plain = append(plain, byte(padLen))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(ct, plain)
	return base64.StdEncoding.EncodeToString(ct)
}

// msgSign 计算微信安全模式 msg_signature（token/timestamp/nonce/encrypt 字典序拼接 SHA1）。
func msgSign(token, timestamp, nonce, encrypt string) string {
	return sign4(token, timestamp, nonce, encrypt)
}

// 回归: W3-23 安全模式 EncodingAESKey 解密 round-trip。
//
// 旧实现未实现 AES 解密，安全模式消息无法解析。修复后 decrypt() 完整实现
// AES-256-CBC + PKCS7 解密并剥离随机串/长度头/AppID 尾部，返回消息正文。
func TestDecrypt_RoundTrip(t *testing.T) {
	a := New(config.WechatConfig{AESKey: wechatTestAESKey})
	if !a.secureModeEnabled() {
		t.Fatal("配置了 AESKey 应启用安全模式")
	}
	enc := wechatEncrypt(t, a.aesKey, "你好微信安全模式", "wxAppID123")

	got, err := a.decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt round-trip 失败: %v", err)
	}
	if got != "你好微信安全模式" {
		t.Fatalf("decrypt = %q, want 你好微信安全模式", got)
	}
}

// 回归: W3-23 decrypt 错误守卫——非法 base64 / 过短密文应报错而非 panic。
func TestDecrypt_ErrorGuards(t *testing.T) {
	a := New(config.WechatConfig{AESKey: wechatTestAESKey})

	if _, err := a.decrypt("!!!not-base64!!!"); err == nil {
		t.Fatal("非 base64 密文应返回错误")
	}
	short := base64.StdEncoding.EncodeToString([]byte("tiny"))
	if _, err := a.decrypt(short); err == nil {
		t.Fatal("不足一个分组的密文应返回错误")
	}
}

// 回归: W3-23 安全模式 handleMessage 端到端——密文经 msg_signature 校验 + AES 解密
// 后正常触达业务 handler，且解密出的消息内容/发送方被正确解析。
func TestHandleMessage_SecureModeEndToEnd(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wxAppID123", AppSecret: "s", AESKey: wechatTestAESKey})

	var got *adapter.Message
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		got = m
		return &adapter.Reply{Content: "已收到"}, nil
	}

	plainXML := buildTextXML("openid-secure", "gh-account", "加密问题", 99)
	enc := wechatEncrypt(t, a.aesKey, plainXML, "wxAppID123")

	ts := nowTS()
	const nonce = "abc"
	// 安全模式下外层 signature 与内层 msg_signature 都需校验。
	outer := sign(token, ts, nonce)
	msgSig := msgSign(token, ts, nonce, enc)

	body := fmt.Sprintf(`<xml><ToUserName><![CDATA[gh-account]]></ToUserName>`+
		`<Encrypt><![CDATA[%s]]></Encrypt></xml>`, enc)

	q := url.Values{}
	q.Set("signature", outer)
	q.Set("msg_signature", msgSig)
	q.Set("timestamp", ts)
	q.Set("nonce", nonce)
	r := httptest.NewRequest(http.MethodPost, "/cb?"+q.Encode(), strings.NewReader(body))
	w := httptest.NewRecorder()

	a.handleMessage(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("安全模式合法请求 code=%d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if got == nil {
		t.Fatal("安全模式解密后应触达 handler，但 handler 未被调用")
	}
	if got.Content != "加密问题" {
		t.Fatalf("解密后 Content = %q, want 加密问题", got.Content)
	}
	if got.ChatID != "openid-secure" {
		t.Fatalf("解密后 ChatID = %q, want openid-secure", got.ChatID)
	}
}

// 回归: W3-23 安全模式 msg_signature 不匹配必须按 403 拒绝（不解密、不触达 handler）。
func TestHandleMessage_SecureModeBadMsgSignature(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wxAppID123", AppSecret: "s", AESKey: wechatTestAESKey})

	var handlerCalled bool
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		handlerCalled = true
		return &adapter.Reply{Content: "x"}, nil
	}

	enc := wechatEncrypt(t, a.aesKey, buildTextXML("u", "gh", "q", 1), "wxAppID123")
	ts := nowTS()
	const nonce = "abc"
	outer := sign(token, ts, nonce) // 外层签名合法，仅 msg_signature 错误

	body := fmt.Sprintf(`<xml><Encrypt><![CDATA[%s]]></Encrypt></xml>`, enc)
	q := url.Values{}
	q.Set("signature", outer)
	q.Set("msg_signature", "deadbeef") // 故意错误
	q.Set("timestamp", ts)
	q.Set("nonce", nonce)
	r := httptest.NewRequest(http.MethodPost, "/cb?"+q.Encode(), strings.NewReader(body))
	w := httptest.NewRecorder()

	a.handleMessage(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("msg_signature 不匹配应被拒(403), code=%d", w.Code)
	}
	if handlerCalled {
		t.Fatal("msg_signature 不匹配不应触达 handler")
	}
}

// 回归: W3-23 安全模式密文无法解密时按 500 拒绝（签名合法但密文损坏）。
func TestHandleMessage_SecureModeUndecryptable(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wxAppID123", AppSecret: "s", AESKey: wechatTestAESKey})
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "x"}, nil
	}

	// 合法 base64 但非有效密文（长度非 16 倍数会在 decrypt 内被拒）。
	enc := base64.StdEncoding.EncodeToString([]byte("not-a-valid-ciphertext-block-xx"))
	ts := nowTS()
	const nonce = "abc"
	outer := sign(token, ts, nonce)
	msgSig := msgSign(token, ts, nonce, enc) // msg_signature 对该密文合法，故进入解密阶段

	body := fmt.Sprintf(`<xml><Encrypt><![CDATA[%s]]></Encrypt></xml>`, enc)
	q := url.Values{}
	q.Set("signature", outer)
	q.Set("msg_signature", msgSig)
	q.Set("timestamp", ts)
	q.Set("nonce", nonce)
	r := httptest.NewRequest(http.MethodPost, "/cb?"+q.Encode(), strings.NewReader(body))
	w := httptest.NewRecorder()

	a.handleMessage(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("密文无法解密应返回 500, code=%d body=%q", w.Code, w.Body.String())
	}
}

// secureModeReplayGuard 确认安全模式同样受 W3-22 重放窗口保护：
// 过旧时间戳即便外层签名合法也应在解密前被拒。
func TestHandleMessage_SecureModeReplayRejected(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wxAppID123", AppSecret: "s", AESKey: wechatTestAESKey})
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "x"}, nil
	}
	enc := wechatEncrypt(t, a.aesKey, buildTextXML("u", "gh", "q", 1), "wxAppID123")
	staleTS := fmt.Sprintf("%d", time.Now().Add(-24*time.Hour).Unix())
	const nonce = "abc"
	outer := sign(token, staleTS, nonce)
	msgSig := msgSign(token, staleTS, nonce, enc)

	body := fmt.Sprintf(`<xml><Encrypt><![CDATA[%s]]></Encrypt></xml>`, enc)
	q := url.Values{}
	q.Set("signature", outer)
	q.Set("msg_signature", msgSig)
	q.Set("timestamp", staleTS)
	q.Set("nonce", nonce)
	r := httptest.NewRequest(http.MethodPost, "/cb?"+q.Encode(), strings.NewReader(body))
	w := httptest.NewRecorder()

	a.handleMessage(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("安全模式过旧时间戳应被拒(403), code=%d", w.Code)
	}
}
