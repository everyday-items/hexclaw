package wecom

// audit_wave3_test.go —— 第三波审计测试
//
// 聚焦企业微信适配器的平台特定逻辑（共享 adapter 框架已被其它测试覆盖）：
//   - webhook 签名校验（SHA1 over 排序后的 token/timestamp/nonce/encrypt）
//   - AES-256-CBC + PKCS7 + WeCom 信封（16 随机 + 4 长度 + msg + corpid）解密
//   - 入站 payload 解析（各种消息类型 / 空字段 / 超长 / 非法 JSON / 非法 XML）
//   - 出站消息转换（StripThinking / TextFallback）
//   - 错误路径、边界、并发竞态、状态一致性
//
// 约束：只新增测试，不改源码；仅测本包。

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// ============== 测试辅助 ==============

// buildEnvelope 复刻企业微信信封并 AES-256-CBC 加密（与源码 decrypt 对称）。
//
// 信封格式：16 字节随机串 + 4 字节大端消息长度 + 消息 + CorpID，PKCS7 填充。
func buildEnvelope(t *testing.T, key []byte, msg, corpid string) string {
	t.Helper()
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(msg)))

	plain := append([]byte("0123456789abcdef"), lenBuf[:]...)
	plain = append(plain, msg...)
	plain = append(plain, corpid...)

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

// sign 复刻企业微信签名算法。
func sign(token, timestamp, nonce, encrypt string) string {
	strs := []string{token, timestamp, nonce, encrypt}
	sort.Strings(strs)
	sum := sha1.Sum([]byte(strings.Join(strs, "")))
	return fmt.Sprintf("%x", sum)
}

// freshTS 返回当前时刻的 Unix 秒字符串。
//
// W3-27 修复后，签名通过仍需 timestamp 落在重放窗口内，正常路径测试
// 必须使用新鲜 timestamp（而非历史固定值）才能放行。
func freshTS() string {
	return fmt.Sprintf("%d", time.Now().Unix())
}

// newAdapter 构造一个带合法 AES Key 的适配器。
func newAdapter(t *testing.T, token string) *WecomAdapter {
	t.Helper()
	a := New(config.WecomConfig{Token: token, AESKey: wecomTestAESKey})
	if len(a.aesKey) != 32 {
		t.Fatalf("AES Key 长度应为 32，实际 %d", len(a.aesKey))
	}
	return a
}

// ============== 1. 签名校验 ==============

// TestCheckSignature_Table 表驱动覆盖签名校验各分支。
func TestCheckSignature_Table(t *testing.T) {
	const token = "tk-123"
	a := New(config.WecomConfig{Token: token})

	ts, nonce, enc := "1700000000", "nonce-x", "ENC-PAYLOAD"
	good := sign(token, ts, nonce, enc)

	cases := []struct {
		name string
		sig  string
		ts   string
		non  string
		enc  string
		want bool
	}{
		{"合法签名", good, ts, nonce, enc, true},
		{"错误签名", "deadbeef", ts, nonce, enc, false},
		{"空签名", "", ts, nonce, enc, false},
		{"篡改 timestamp", good, "1700000001", nonce, enc, false},
		{"篡改 nonce", good, ts, "other", enc, false},
		{"篡改 encrypt", good, ts, nonce, "OTHER", false},
		{"全空参数", sign(token, "", "", ""), "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := a.checkSignature(c.sig, c.ts, c.non, c.enc); got != c.want {
				t.Errorf("checkSignature=%v want=%v", got, c.want)
			}
		})
	}
}

// 回归: W3-27 webhook 签名时间戳重放窗口。
//
// 企业微信签名只验证 SHA1(sorted(token,ts,nonce,enc))，timestamp 是被签名参数之一，
// 但源码原先从不比较 timestamp 与当前时间——任何一个曾经合法的（signature, timestamp,
// nonce, encrypt）四元组都可被无限重放。修复后 handleVerify / handleCallback 在签名
// 通过后追加 whauth.CheckReplayWindow 校验：过旧的 timestamp 一律拒绝（403），
// 新鲜的 timestamp 正常放行。
//
// 本测试用一个 10 年前的合法签名四元组证明它被拒绝（修复前会被接受），
// 同时验证新鲜 timestamp 仍能通过——既锁定重放防护，又防止误杀合法请求。
func TestWebhookReplayWindow(t *testing.T) {
	const token = "tk"

	oldTS := fmt.Sprintf("%d", time.Now().AddDate(-10, 0, 0).Unix())
	freshTS := fmt.Sprintf("%d", time.Now().Unix())

	t.Run("handleVerify 拒绝过旧 timestamp", func(t *testing.T) {
		a := newAdapter(t, token)
		enc := buildEnvelope(t, a.aesKey, "echo-ok", "corp")
		// 签名本身是合法的（修复前会被接受），只是时间戳过旧。
		sig := sign(token, oldTS, "abc", enc)
		if !a.checkSignature(sig, oldTS, "abc", enc) {
			t.Fatal("签名实现已改变，重放窗口断言前提不成立")
		}
		w := doVerify(a, sig, oldTS, "abc", enc)
		if w.Code != http.StatusForbidden {
			t.Fatalf("[BUG] 过旧 timestamp 应被拒绝 403，实际 code=%d", w.Code)
		}
	})

	t.Run("handleVerify 接受新鲜 timestamp", func(t *testing.T) {
		a := newAdapter(t, token)
		enc := buildEnvelope(t, a.aesKey, "echo-ok", "corp")
		sig := sign(token, freshTS, "abc", enc)
		w := doVerify(a, sig, freshTS, "abc", enc)
		if w.Code != http.StatusOK || w.Body.String() != "echo-ok" {
			t.Fatalf("新鲜 timestamp 应通过，实际 code=%d body=%q", w.Code, w.Body.String())
		}
	})

	t.Run("handleCallback 拒绝过旧 timestamp", func(t *testing.T) {
		a := newAdapter(t, token)
		enc := makeEncryptedXML(t, a, `<xml><FromUserName>u</FromUserName><MsgType>text</MsgType><Content>x</Content></xml>`)
		envelope := fmt.Sprintf(`<xml><Encrypt>%s</Encrypt></xml>`, enc)
		w := postCallback(a, token, envelope, oldTS, "abc", enc)
		if w.Code != http.StatusForbidden {
			t.Fatalf("[BUG] 过旧 timestamp 回调应被拒绝 403，实际 code=%d", w.Code)
		}
	})
}

// ============== 2. 解密路径 ==============

// TestDecrypt_RoundTrip_Variants 表驱动解密往返：空消息 / Unicode / 超长 / 二进制。
func TestDecrypt_RoundTrip_Variants(t *testing.T) {
	a := newAdapter(t, "tk")

	cases := []struct {
		name string
		msg  string
	}{
		{"普通文本", "hello"},
		{"空消息", ""},
		{"中文 Unicode", "你好，企业微信👋 表情符号"},
		{"超长消息", strings.Repeat("A", 100000)},
		{"含换行制表", "line1\nline2\tend"},
		{"恰好对齐边界", strings.Repeat("x", 16)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc := buildEnvelope(t, a.aesKey, c.msg, "corp-A")
			got, err := a.decrypt(enc)
			if err != nil {
				t.Fatalf("decrypt 失败: %v", err)
			}
			if got != c.msg {
				t.Fatalf("decrypt=%q want=%q", got, c.msg)
			}
		})
	}
}

// 回归: W3-25 decrypt 缺 AES 整块对齐校验，非整块密文 panic（远程 DoS）。
//
// 密文长度 >= AES.BlockSize 但不是 16 的整数倍时，CBC CryptBlocks 会 panic。
// 源码 decrypt 原先只有 `len(ciphertext) < aes.BlockSize` 一道护栏（16 字节下限），
// 缺少 `len(ciphertext)%aes.BlockSize != 0` 的整块校验。攻击者只要知道 Token，
// 就能构造一个解码后长度为 24/17/... 字节的 base64，签名通过后触发 panic，
// 在 handleVerify / handleCallback 同步路径上崩溃请求处理 goroutine。
//
// 期望：decrypt 返回错误而非 panic。若发生 panic，本测试失败 —— 暴露真实缺陷。
func TestDecrypt_NonBlockAlignedCiphertext(t *testing.T) {
	a := newAdapter(t, "tk")

	// 解码后 24 字节：>=16 但不是 16 倍数
	raw := make([]byte, 24)
	for i := range raw {
		raw[i] = byte(i)
	}
	enc := base64.StdEncoding.EncodeToString(raw)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("[BUG] decrypt 对非整块密文发生 panic（应返回 error）: %v", r)
		}
	}()

	if _, err := a.decrypt(enc); err == nil {
		t.Error("[BUG] decrypt 对非整块密文应返回错误，实际无错（且未 panic？）")
	}
}

// 回归: W3-26 decrypt 校验信封尾部 CorpID 防伪造来源。
//
// 企业微信官方规范要求解密后比对尾部 CorpID 是否与自身一致，防止伪造来源。
// 源码 decrypt 原先直接丢弃 CorpID 段从不校验——任意伪造来源的密文都能解密成功。
// 修复后采用 fail-closed：配置了 CorpID 时强制比对，不一致返回 error；
// 比对使用 whauth.ConstantTimeEqualString 常量时间方式。
func TestDecrypt_CorpIDValidation(t *testing.T) {
	t.Run("CorpID 不一致应拒绝", func(t *testing.T) {
		a := New(config.WecomConfig{
			CorpID: "MY-REAL-CORP",
			AESKey: wecomTestAESKey,
		})
		// 用攻击者 CorpID 加密，签名/AES 都合法，仅来源伪造。
		enc := buildEnvelope(t, a.aesKey, "spoofed", "ATTACKER-CORP")

		if _, err := a.decrypt(enc); err == nil {
			t.Fatal("[BUG] decrypt 对伪造 CorpID 应返回错误（防伪造来源），实际无错")
		}
	})

	t.Run("CorpID 一致应通过", func(t *testing.T) {
		a := New(config.WecomConfig{
			CorpID: "MY-REAL-CORP",
			AESKey: wecomTestAESKey,
		})
		enc := buildEnvelope(t, a.aesKey, "legit", "MY-REAL-CORP")

		got, err := a.decrypt(enc)
		if err != nil {
			t.Fatalf("合法 CorpID 不应报错: %v", err)
		}
		if got != "legit" {
			t.Fatalf("decrypt=%q want legit", got)
		}
	})

	t.Run("未配置 CorpID 时跳过校验（向后兼容）", func(t *testing.T) {
		// fail-closed 仅对已配置 CorpID 生效；空配置不强制校验。
		a := New(config.WecomConfig{AESKey: wecomTestAESKey})
		enc := buildEnvelope(t, a.aesKey, "any", "WHATEVER-CORP")

		got, err := a.decrypt(enc)
		if err != nil {
			t.Fatalf("未配置 CorpID 不应因尾部 CorpID 报错: %v", err)
		}
		if got != "any" {
			t.Fatalf("decrypt=%q want any", got)
		}
	})
}

// TestDecrypt_TamperedLengthPrefix 边界：消息长度前缀大于实际剩余长度应报错。
func TestDecrypt_TamperedLengthPrefix(t *testing.T) {
	a := newAdapter(t, "tk")

	// 手工构造一个长度前缀夸大的明文信封
	plain := append([]byte("0123456789abcdef"), 0xff, 0xff, 0xff, 0xff) // msgLen = 4G
	plain = append(plain, []byte("short")...)
	padLen := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < padLen; i++ {
		plain = append(plain, byte(padLen))
	}
	block, _ := aes.NewCipher(a.aesKey)
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, a.aesKey[:aes.BlockSize]).CryptBlocks(ct, plain)
	enc := base64.StdEncoding.EncodeToString(ct)

	if _, err := a.decrypt(enc); err == nil {
		t.Error("夸大的消息长度前缀应返回错误")
	}
}

// TestDecrypt_InvalidPKCS7 边界：被破坏的 PKCS7 填充应报错（不 panic）。
func TestDecrypt_InvalidPKCS7(t *testing.T) {
	a := newAdapter(t, "tk")

	// 构造一个 32 字节明文，最后一字节填一个非法 padLen=20（>16）
	plain := make([]byte, 32)
	plain[31] = 20
	block, _ := aes.NewCipher(a.aesKey)
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, a.aesKey[:aes.BlockSize]).CryptBlocks(ct, plain)
	enc := base64.StdEncoding.EncodeToString(ct)

	if _, err := a.decrypt(enc); err == nil {
		t.Error("非法 PKCS7 填充（padLen>16）应返回错误")
	}
}

// TestDecrypt_PostPaddingTooShort 边界：去填充后不足 20 字节（缺少 16+4 头）应报错。
func TestDecrypt_PostPaddingTooShort(t *testing.T) {
	a := newAdapter(t, "tk")

	// 一个 16 字节全 0x10 的块：PKCS7 解出 padLen=16，去填充后剩 0 字节 -> <20
	plain := make([]byte, 16)
	for i := range plain {
		plain[i] = 16
	}
	block, _ := aes.NewCipher(a.aesKey)
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, a.aesKey[:aes.BlockSize]).CryptBlocks(ct, plain)
	enc := base64.StdEncoding.EncodeToString(ct)

	if _, err := a.decrypt(enc); err == nil {
		t.Error("去填充后数据不足 20 字节应返回错误")
	}
}

// ============== 3. handleVerify ==============

// TestHandleVerify_Table 表驱动覆盖 URL 验证回调。
func TestHandleVerify_Table(t *testing.T) {
	const token = "tk"

	t.Run("合法签名返回明文", func(t *testing.T) {
		a := newAdapter(t, token)
		enc := buildEnvelope(t, a.aesKey, "echo-ok", "corp")
		ts := freshTS()
		sig := sign(token, ts, "abc", enc)
		w := doVerify(a, sig, ts, "abc", enc)
		if w.Code != http.StatusOK || w.Body.String() != "echo-ok" {
			t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
		}
	})

	t.Run("错误签名 403", func(t *testing.T) {
		a := newAdapter(t, token)
		w := doVerify(a, "bad", freshTS(), "abc", "whatever")
		if w.Code != http.StatusForbidden {
			t.Fatalf("code=%d want 403", w.Code)
		}
	})

	t.Run("签名过但 echostr 非法 base64 -> 500", func(t *testing.T) {
		a := newAdapter(t, token)
		echo := "!!!not base64!!!"
		ts := freshTS()
		sig := sign(token, ts, "abc", echo)
		w := doVerify(a, sig, ts, "abc", echo)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d want 500", w.Code)
		}
	})

	t.Run("空 echostr 但签名匹配 -> 解密失败 500", func(t *testing.T) {
		a := newAdapter(t, token)
		ts := freshTS()
		sig := sign(token, ts, "abc", "")
		w := doVerify(a, sig, ts, "abc", "")
		// 空 base64 解出 0 字节，decrypt 报 "密文太短" -> 500
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d want 500", w.Code)
		}
	})
}

func doVerify(a *WecomAdapter, sig, ts, nonce, echo string) *httptest.ResponseRecorder {
	q := url.Values{}
	q.Set("msg_signature", sig)
	q.Set("timestamp", ts)
	q.Set("nonce", nonce)
	q.Set("echostr", echo)
	w := httptest.NewRecorder()
	a.handleVerify(w, httptest.NewRequest(http.MethodGet, "/wh?"+q.Encode(), nil))
	return w
}

// TestHandleVerify_NonAlignedEchostrShouldNotPanic 安全/边界：
// echostr 解码后非整块时，handleVerify -> decrypt 不应 panic。
func TestHandleVerify_NonAlignedEchostrShouldNotPanic(t *testing.T) {
	const token = "tk"
	a := newAdapter(t, token)

	raw := make([]byte, 17) // 非整块
	echo := base64.StdEncoding.EncodeToString(raw)
	// 用新鲜 timestamp 确保越过 W3-27 重放窗口，真正走到 decrypt 非整块分支。
	ts := freshTS()
	sig := sign(token, ts, "abc", echo)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("[BUG] handleVerify 对非整块 echostr panic（应优雅 500）: %v", r)
		}
	}()
	w := doVerify(a, sig, ts, "abc", echo)
	// 期望优雅返回（500/forbidden），而不是 panic
	if w.Code == http.StatusOK {
		t.Errorf("非整块 echostr 不应返回 200，实际 code=%d", w.Code)
	}
}

// ============== 4. handleCallback / 入站解析 ==============

// makeEncryptedXML 构造一个加密后的回调 XML 信封。
func makeEncryptedXML(t *testing.T, a *WecomAdapter, innerXML string) string {
	t.Helper()
	enc := buildEnvelope(t, a.aesKey, innerXML, "corp")
	return enc
}

// postCallback 发起一次回调请求，返回 recorder。
func postCallback(a *WecomAdapter, token, body string, ts, nonce, encField string) *httptest.ResponseRecorder {
	sig := sign(token, ts, nonce, encField)
	q := url.Values{}
	q.Set("msg_signature", sig)
	q.Set("timestamp", ts)
	q.Set("nonce", nonce)
	r := httptest.NewRequest(http.MethodPost, "/wh?"+q.Encode(), strings.NewReader(body))
	w := httptest.NewRecorder()
	a.handleCallback(w, r)
	return w
}

// TestHandleCallback_TextMessageRoundTrip 正常路径：
// 文本消息 -> 校验 -> 解密 -> 解析 -> 200 success，且 handler 被异步调用。
func TestHandleCallback_TextMessageRoundTrip(t *testing.T) {
	const token = "tk"
	a := newAdapter(t, token)

	gotCh := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		gotCh <- m
		return nil, nil // 不触发 Send（无真实 API）
	}

	inner := `<xml><ToUserName>corp</ToUserName><FromUserName>user-1</FromUserName>` +
		`<CreateTime>1700000000</CreateTime><MsgType>text</MsgType>` +
		`<Content>hi there</Content><MsgId>1234567890</MsgId><AgentID>10</AgentID></xml>`
	enc := makeEncryptedXML(t, a, inner)

	envelope := fmt.Sprintf(`<xml><ToUserName>corp</ToUserName><Encrypt>%s</Encrypt><AgentID>10</AgentID></xml>`, enc)
	w := postCallback(a, token, envelope, freshTS(), "abc", enc)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d want 200; body=%q", w.Code, w.Body.String())
	}
	if w.Body.String() != "success" {
		t.Fatalf("body=%q want success", w.Body.String())
	}

	select {
	case m := <-gotCh:
		if m.Content != "hi there" {
			t.Errorf("Content=%q want 'hi there'", m.Content)
		}
		if m.ChatID != "user-1" || m.UserID != "user-1" {
			t.Errorf("ChatID/UserID=%q/%q want user-1", m.ChatID, m.UserID)
		}
		if m.Platform != adapter.PlatformWecom {
			t.Errorf("Platform=%q", m.Platform)
		}
		if m.Metadata["msg_id"] != "1234567890" {
			t.Errorf("msg_id=%q", m.Metadata["msg_id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler 未在超时内被调用（异步处理可能未触发）")
	}
}

// 回归: W3-28 processMessage 校验 AgentID + 非 text 类型记录后丢弃。
//
// 修复前 processMessage 只判断 MsgType != "text"，从不校验解析出的 AgentID
// 与配置是否一致——多应用共用回调 URL 时其它应用的消息会被错误处理。
// 修复后：配置了 AgentID 时，AgentID 不一致的消息直接丢弃，handler 不被调用；
// AgentID 一致的 text 消息正常处理。
func TestProcessMessage_AgentIDValidation(t *testing.T) {
	t.Run("AgentID 不一致应丢弃，handler 不被调用", func(t *testing.T) {
		a := New(config.WecomConfig{Token: "tk", AESKey: wecomTestAESKey, AgentID: "10"})
		called := make(chan struct{}, 1)
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
			called <- struct{}{}
			return nil, nil
		}

		// 一条合法的 text 消息，但 AgentID=99 与配置 10 不符。
		a.processMessage(wecomMessage{MsgType: "text", FromUserName: "u", Content: "hi", AgentID: 99})

		select {
		case <-called:
			t.Error("[BUG] AgentID 不一致的消息不应触发 handler")
		case <-time.After(200 * time.Millisecond):
			// 符合预期：被丢弃
		}
	})

	t.Run("AgentID 一致的 text 消息正常处理", func(t *testing.T) {
		a := New(config.WecomConfig{Token: "tk", AESKey: wecomTestAESKey, AgentID: "10"})
		got := make(chan string, 1)
		a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
			got <- m.Content
			return nil, nil
		}

		a.processMessage(wecomMessage{MsgType: "text", FromUserName: "u", Content: "hello", AgentID: 10})

		select {
		case c := <-got:
			if c != "hello" {
				t.Errorf("Content=%q want hello", c)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("AgentID 一致的 text 消息应触发 handler，实际超时")
		}
	})

	t.Run("非 text 类型被丢弃（记录日志，不触发 handler）", func(t *testing.T) {
		a := New(config.WecomConfig{Token: "tk", AESKey: wecomTestAESKey, AgentID: "10"})
		called := make(chan struct{}, 1)
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
			called <- struct{}{}
			return nil, nil
		}

		a.processMessage(wecomMessage{MsgType: "image", FromUserName: "u", AgentID: 10})

		select {
		case <-called:
			t.Error("非 text 类型不应触发 handler")
		case <-time.After(200 * time.Millisecond):
			// 符合预期
		}
	})
}

// TestHandleCallback_NonTextDropped 入站：非 text 消息类型被静默丢弃，handler 不应被调用。
func TestHandleCallback_NonTextDropped(t *testing.T) {
	const token = "tk"
	types := []string{"image", "voice", "event", "video", "location", ""}

	for _, mt := range types {
		t.Run("type="+mt, func(t *testing.T) {
			a := newAdapter(t, token)
			called := make(chan struct{}, 1)
			a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
				called <- struct{}{}
				return nil, nil
			}

			inner := fmt.Sprintf(`<xml><FromUserName>u</FromUserName><MsgType>%s</MsgType><Content>x</Content></xml>`, mt)
			enc := makeEncryptedXML(t, a, inner)
			envelope := fmt.Sprintf(`<xml><Encrypt>%s</Encrypt></xml>`, enc)
			w := postCallback(a, token, envelope, freshTS(), "abc", enc)

			if w.Code != http.StatusOK {
				t.Fatalf("code=%d want 200", w.Code)
			}
			select {
			case <-called:
				t.Errorf("非 text 类型 %q 不应触发 handler", mt)
			case <-time.After(300 * time.Millisecond):
				// 符合预期：被丢弃
			}
		})
	}
}

// TestHandleCallback_ErrorPaths 表驱动错误路径。
func TestHandleCallback_ErrorPaths(t *testing.T) {
	const token = "tk"

	t.Run("非法外层 XML -> 400", func(t *testing.T) {
		a := newAdapter(t, token)
		w := postCallback(a, token, "<broken", "100", "abc", "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code=%d want 400", w.Code)
		}
	})

	t.Run("签名不匹配 -> 403", func(t *testing.T) {
		a := newAdapter(t, token)
		envelope := `<xml><Encrypt>ABC</Encrypt></xml>`
		// 故意用错误 token 生成签名
		badSig := sign("wrong-token", "100", "abc", "ABC")
		q := url.Values{}
		q.Set("msg_signature", badSig)
		q.Set("timestamp", "100")
		q.Set("nonce", "abc")
		r := httptest.NewRequest(http.MethodPost, "/wh?"+q.Encode(), strings.NewReader(envelope))
		w := httptest.NewRecorder()
		a.handleCallback(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code=%d want 403", w.Code)
		}
	})

	t.Run("Encrypt 非法 base64 -> 解密失败 500", func(t *testing.T) {
		a := newAdapter(t, token)
		enc := "!!!bad-base64!!!"
		envelope := fmt.Sprintf(`<xml><Encrypt>%s</Encrypt></xml>`, enc)
		w := postCallback(a, token, envelope, freshTS(), "abc", enc)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code=%d want 500", w.Code)
		}
	})

	t.Run("解密成功但内层非法 XML -> 400", func(t *testing.T) {
		a := newAdapter(t, token)
		enc := makeEncryptedXML(t, a, "this-is-not-xml-<<<")
		envelope := fmt.Sprintf(`<xml><Encrypt>%s</Encrypt></xml>`, enc)
		w := postCallback(a, token, envelope, freshTS(), "abc", enc)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code=%d want 400; body=%q", w.Code, w.Body.String())
		}
	})

	t.Run("空字段消息可解析 -> 200（丢弃）", func(t *testing.T) {
		a := newAdapter(t, token)
		enc := makeEncryptedXML(t, a, `<xml></xml>`)
		envelope := fmt.Sprintf(`<xml><Encrypt>%s</Encrypt></xml>`, enc)
		w := postCallback(a, token, envelope, freshTS(), "abc", enc)
		if w.Code != http.StatusOK {
			t.Fatalf("code=%d want 200", w.Code)
		}
	})
}

// TestHandleCallback_OversizedBody 边界：超过 1 MiB 的请求体返回 413。
//
// 注意：MaxBytesReader 在外层 XML 解析时才真正读爆；body 必须是“看起来像合法
// 开头但超长”才能让 io.ReadAll 触发 MaxBytesError。这里直接喂 >1MiB 的内容。
func TestHandleCallback_OversizedBody(t *testing.T) {
	const token = "tk"
	a := newAdapter(t, token)

	big := strings.Repeat("A", (1<<20)+1024) // 略大于 1 MiB
	q := url.Values{}
	q.Set("msg_signature", "x")
	q.Set("timestamp", "100")
	q.Set("nonce", "abc")
	r := httptest.NewRequest(http.MethodPost, "/wh?"+q.Encode(), strings.NewReader(big))
	w := httptest.NewRecorder()
	a.handleCallback(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 body code=%d want 413", w.Code)
	}
}

// TestHandleCallback_NonAlignedEncryptShouldNotPanic 安全/边界：
// Encrypt 字段解码后非整块时不应 panic（同 decrypt 缺陷）。
func TestHandleCallback_NonAlignedEncryptShouldNotPanic(t *testing.T) {
	const token = "tk"
	a := newAdapter(t, token)

	raw := make([]byte, 24) // 非整块
	enc := base64.StdEncoding.EncodeToString(raw)
	envelope := fmt.Sprintf(`<xml><Encrypt>%s</Encrypt></xml>`, enc)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("[BUG] handleCallback 对非整块 Encrypt panic（应优雅 500）: %v", r)
		}
	}()
	// 用新鲜 timestamp 越过 W3-27 重放窗口，真正走到 decrypt 非整块分支。
	w := postCallback(a, token, envelope, freshTS(), "abc", enc)
	if w.Code == http.StatusOK {
		t.Errorf("非整块 Encrypt 不应返回 200，实际 code=%d", w.Code)
	}
}

// TestHandler_MethodRouting 验证统一 ingress Handler 的方法路由。
func TestHandler_MethodRouting(t *testing.T) {
	a := newAdapter(t, "tk")
	h := a.Handler()

	t.Run("PUT -> 405", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/wh", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("code=%d want 405", w.Code)
		}
	})

	t.Run("DELETE -> 405", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/wh", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("code=%d want 405", w.Code)
		}
	})

	t.Run("GET 路由到 handleVerify", func(t *testing.T) {
		// 错误签名应得到 403（证明确实进了 verify 分支）
		w := httptest.NewRecorder()
		q := url.Values{}
		q.Set("msg_signature", "bad")
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/wh?"+q.Encode(), nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("GET code=%d want 403", w.Code)
		}
	})
}

// ============== 5. 出站转换 ==============

// TestSend_StripThinking 出站：Send 应剥离 <think> 块。
// 通过 mock handler + 拦截真实 API 不可达：这里用 sendReplyNow 直发会因无网络失败，
// 但我们关注的是“StripThinking 是否被调用”。改为直接验证 StripThinking 行为契约，
// 并验证 Send 在 reply.Content 全是 thinking 时不会 panic。
func TestSend_StripThinkingContract(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"答案 <think>推理</think> 完", "答案  完"},
		{"<thinking>x</thinking>纯净", "纯净"},
		{"无标签", "无标签"},
		{"", ""},
		{"半截 <think>没闭合", "半截"},
	}
	for _, c := range cases {
		if got := adapter.StripThinking(c.in); got != c.want {
			t.Errorf("StripThinking(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestSendStream_ErrorPropagation 流式：chunk.Error 应中断并向上返回。
func TestSendStream_ErrorPropagation(t *testing.T) {
	a := newAdapter(t, "tk")
	ch := make(chan *adapter.ReplyChunk, 2)
	ch <- &adapter.ReplyChunk{Content: "part"}
	ch <- &adapter.ReplyChunk{Error: fmt.Errorf("boom")}
	close(ch)

	err := a.SendStream(context.Background(), "u", ch)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("SendStream 应传播 chunk 错误，实际 err=%v", err)
	}
}

// TestSendStream_EmptyChannel 流式：空 channel（无 chunk）应尝试发送空字符串。
// 无真实 API，sendTextMessage 会因 getAccessToken 失败返回错误——验证不 panic。
func TestSendStream_EmptyChannel(t *testing.T) {
	a := New(config.WecomConfig{Token: "tk", AESKey: wecomTestAESKey, CorpID: "c", Secret: "s"})
	ch := make(chan *adapter.ReplyChunk)
	close(ch)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// 期望：不 panic（返回值不强求，取决于网络）
	_ = a.SendStream(ctx, "u", ch)
}

// TestSend_NilReply Send 对 nil reply 的处理（经队列）。
func TestSend_NilReply(t *testing.T) {
	a := newAdapter(t, "tk")
	if err := a.Send(context.Background(), "u", nil); err != nil {
		t.Errorf("Send(nil) 应无错（队列层短路），实际 err=%v", err)
	}
}

// ============== 6. 生命周期 / 配置校验 ==============

// TestValidateConfig_Table 表驱动覆盖配置校验各缺失分支。
func TestValidateConfig_Table(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.WecomConfig
		wantSub string // 期望错误子串（空表示不校验子串，只要求有错）
	}{
		{"缺 CorpID", config.WecomConfig{Secret: "s", AgentID: "a", Token: "t", AESKey: "k"}, "corp_id"},
		{"缺 Secret", config.WecomConfig{CorpID: "c", AgentID: "a", Token: "t", AESKey: "k"}, "secret"},
		{"缺 AgentID", config.WecomConfig{CorpID: "c", Secret: "s", Token: "t", AESKey: "k"}, "agent_id"},
		{"缺 Token", config.WecomConfig{CorpID: "c", Secret: "s", AgentID: "a", AESKey: "k"}, "token"},
		{"缺 AESKey", config.WecomConfig{CorpID: "c", Secret: "s", AgentID: "a", Token: "t"}, "aes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := New(c.cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			err := a.ValidateConfig(ctx)
			if err == nil {
				t.Fatalf("缺失字段应返回错误")
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("err=%q 不含 %q", err.Error(), c.wantSub)
			}
		})
	}
}

// TestHealth_Table 表驱动覆盖 Health 各状态。
func TestHealth_Table(t *testing.T) {
	t.Run("handler 未附加", func(t *testing.T) {
		a := New(config.WecomConfig{CorpID: "c", Secret: "s", AgentID: "a", AESKey: wecomTestAESKey})
		if err := a.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "handler") {
			t.Fatalf("err=%v want handler 未附加", err)
		}
	})

	t.Run("配置不全", func(t *testing.T) {
		a := New(config.WecomConfig{AESKey: wecomTestAESKey})
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		if err := a.Health(context.Background()); err == nil {
			t.Fatal("配置不全应返回错误")
		}
	})

	t.Run("AES Key 缺失", func(t *testing.T) {
		a := New(config.WecomConfig{CorpID: "c", Secret: "s", AgentID: "a"})
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		if err := a.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "aes") {
			t.Fatalf("err=%v want aes key 未配置", err)
		}
	})

	t.Run("全部就绪", func(t *testing.T) {
		a := New(config.WecomConfig{CorpID: "c", Secret: "s", AgentID: "a", AESKey: wecomTestAESKey})
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		if err := a.Health(context.Background()); err != nil {
			t.Fatalf("全部就绪应无错，实际 %v", err)
		}
	})
}

// TestAttach_EmptyCorpSecret Attach 对空 CorpID/Secret 应报错。
func TestAttach_EmptyCorpSecret(t *testing.T) {
	a := New(config.WecomConfig{})
	if err := a.Attach(func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }); err == nil {
		t.Error("空 CorpID/Secret 应报错")
	}
	// handler 仍被赋值（即使后续报错）——验证状态一致性
	if a.handler == nil {
		t.Error("Attach 即使报错也应已赋值 handler（当前实现先赋值后校验）")
	}
}

// TestStop_Idempotent Stop 多次调用应幂等不 panic。
func TestStop_Idempotent(t *testing.T) {
	a := New(config.WecomConfig{CorpID: "c", Secret: "s"})
	for i := 0; i < 3; i++ {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("第 %d 次 Stop 应无错，实际 %v", i+1, err)
		}
	}
}

// TestSendAfterStop Stop 后再 Send 应被队列拒绝。
func TestSendAfterStop(t *testing.T) {
	a := newAdapter(t, "tk")
	_ = a.Stop(context.Background())

	err := a.Send(context.Background(), "u", &adapter.Reply{Content: "hi"})
	if err == nil || !strings.Contains(err.Error(), "停止") {
		t.Fatalf("Stop 后 Send 应返回'已停止'错误，实际 err=%v", err)
	}
}

// ============== 7. 并发竞态 ==============

// TestGetAccessToken_ConcurrentCacheRace 并发获取 token 验证双检锁缓存无数据竞争。
//
// 预先注入一个有效缓存 token，使所有 goroutine 命中缓存路径（不发网络）。
// 用 -race 运行可暴露 mu 保护下的字段竞争。
func TestGetAccessToken_ConcurrentCacheRace(t *testing.T) {
	a := New(config.WecomConfig{CorpID: "c", Secret: "s"})
	a.mu.Lock()
	a.accessToken = "cached-token"
	a.tokenExpiry = time.Now().Add(1 * time.Hour)
	a.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := a.getAccessToken(context.Background())
			if err != nil || tok != "cached-token" {
				t.Errorf("并发取缓存 token=%q err=%v", tok, err)
			}
		}()
	}
	wg.Wait()
}

// TestConcurrentDecrypt 并发解密不同密文（decrypt 内部对 aesKey 只读）。
//
// 注意：decrypt 复用同一 aesKey 派生 IV，且 CryptBlocks 原地修改传入的 ciphertext
// 切片（base64 解码产生的独立切片），因此并发安全。用 -race 验证。
func TestConcurrentDecrypt(t *testing.T) {
	a := newAdapter(t, "tk")

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			msg := fmt.Sprintf("msg-%d", n)
			enc := buildEnvelope(t, a.aesKey, msg, "corp")
			got, err := a.decrypt(enc)
			if err != nil || got != msg {
				t.Errorf("并发 decrypt got=%q err=%v want=%q", got, err, msg)
			}
		}(i)
	}
	wg.Wait()
}

// ============== 8. 数据模型 / 名称 ==============

// TestName_Custom 自定义 Name 优先于默认。
func TestName_Custom(t *testing.T) {
	a := New(config.WecomConfig{Name: "wecom-support"})
	if a.Name() != "wecom-support" {
		t.Errorf("Name=%q want wecom-support", a.Name())
	}
}

// TestWecomMessage_XMLUnmarshal 数据模型：明文 XML 解析各字段。
func TestWecomMessage_XMLUnmarshal(t *testing.T) {
	raw := `<xml><ToUserName>corp</ToUserName><FromUserName>u9</FromUserName>` +
		`<CreateTime>1700</CreateTime><MsgType>text</MsgType>` +
		`<Content>你好</Content><MsgId>987654321</MsgId><AgentID>42</AgentID></xml>`
	var m wecomMessage
	if err := xml.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.FromUserName != "u9" || m.Content != "你好" || m.MsgID != 987654321 || m.AgentID != 42 {
		t.Errorf("解析字段不符: %+v", m)
	}
}
