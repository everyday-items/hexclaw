package wechat

// audit_wave3_test.go —— 第三轮挑刺式审计测试。
//
// 本包是微信公众号平台适配器，共享 adapter 框架（SendQueue / StripThinking 等）已有覆盖，
// 因此本文件聚焦平台特定逻辑：
//   - webhook 签名校验（SHA1 字典序拼接 + hmac.Equal 常量时间比较）
//   - 入站 XML payload 解析（各种消息类型 / 空字段 / 超长 / 非法 XML / Unicode）
//   - 被动回复与超时改走客服消息的状态机
//   - 出站客服消息转换 + getAccessToken 缓存 / 错误路径
//   - ValidateConfig / Health 的配置校验闭环
//
// 全部基于真实公开/包内函数，network 路径通过 httptest.Server 注入到 a.client，
// 不 mock 掉被测逻辑本身。

import (
	"context"
	"crypto/sha1"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// sign 计算微信约定的 SHA1 签名（token/timestamp/nonce 字典序拼接）。
func sign(token, timestamp, nonce string) string {
	s := []string{token, timestamp, nonce}
	sort.Strings(s)
	sum := sha1.Sum([]byte(strings.Join(s, "")))
	return fmt.Sprintf("%x", sum)
}

// sign4 计算微信安全模式 msg_signature（token/timestamp/nonce/encrypt 字典序拼接 SHA1）。
func sign4(token, timestamp, nonce, encrypt string) string {
	s := []string{token, timestamp, nonce, encrypt}
	sort.Strings(s)
	sum := sha1.Sum([]byte(strings.Join(s, "")))
	return fmt.Sprintf("%x", sum)
}

// nowTS 返回当前时间的 Unix 秒字符串。
//
// W3-22 之后 handleVerify/handleMessage 会校验时间戳重放窗口，
// 端到端 handler 测试必须使用"当下"的时间戳才能落入窗口（不再能用固定的 "100"）。
func nowTS() string { return fmt.Sprintf("%d", time.Now().Unix()) }

// =====================================================================
// 1. 签名校验：字典序、边界、空值、Unicode
// =====================================================================

func TestCheckSignature_TableDriven(t *testing.T) {
	const token = "the-token"
	a := New(config.WechatConfig{Token: token})

	tests := []struct {
		name      string
		sig       func() string
		timestamp string
		nonce     string
		want      bool
	}{
		{
			name:      "正确签名通过",
			sig:       func() string { return sign(token, "100", "abc") },
			timestamp: "100", nonce: "abc", want: true,
		},
		{
			name:      "错误签名失败",
			sig:       func() string { return "deadbeef" },
			timestamp: "100", nonce: "abc", want: false,
		},
		{
			name:      "空签名失败",
			sig:       func() string { return "" },
			timestamp: "100", nonce: "abc", want: false,
		},
		{
			name:      "签名大小写敏感（微信下发小写 hex）",
			sig:       func() string { return strings.ToUpper(sign(token, "100", "abc")) },
			timestamp: "100", nonce: "abc", want: false,
		},
		{
			name:      "timestamp 与 nonce 调换会改变拼接结果（除非排序后相同）",
			sig:       func() string { return sign(token, "abc", "100") },
			timestamp: "100", nonce: "abc", want: true, // 字典序排序后两者一致，故仍通过
		},
		{
			name:      "Unicode nonce 也能稳定校验",
			sig:       func() string { return sign(token, "100", "中文nonce") },
			timestamp: "100", nonce: "中文nonce", want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.checkSignature(tt.sig(), tt.timestamp, tt.nonce)
			if got != tt.want {
				t.Fatalf("checkSignature(%q,%q,%q) = %v, want %v",
					tt.sig(), tt.timestamp, tt.nonce, got, tt.want)
			}
		})
	}
}

// 回归: W3-21 Token 为空时 checkSignature 必须 fail-closed 拒绝伪造签名。
//
// 旧实现里 Token 未配置（空串）时，签名只取决于 timestamp+nonce，
// 任何外部调用方都能算出"合法"签名绕过鉴权。修复后 checkSignature 复用
// whauth.RequireSecret 强制 Token 非空，空 Token 一律返回 false。
// 本测试钉死该 fail-closed 行为，禁止回退到 fail-open。
func TestCheckSignature_EmptyTokenRejected(t *testing.T) {
	a := New(config.WechatConfig{Token: ""}) // 未配置 Token

	// 攻击者按空 Token 算出的"伪造"签名必须被拒绝（fail-closed）。
	forged := sign("", "100", "abc")
	if a.checkSignature(forged, "100", "abc") {
		t.Fatalf("空 Token 下伪造签名必须被拒绝（fail-closed），但被接受了")
	}
}

// 回归: W3-21 空 Token 隐患贯通到 handleVerify：echostr 握手不可被伪造。
//
// 未配置 Token 时不应放行 echostr 握手，必须返回 403。
func TestHandleVerify_EmptyTokenRejectsForgedHandshake(t *testing.T) {
	a := New(config.WechatConfig{}) // AppID/Secret/Token 全空
	forged := sign("", nowTS(), "abc")
	url := fmt.Sprintf("/cb?signature=%s&timestamp=%s&nonce=abc&echostr=PWNED", forged, nowTS())
	w := httptest.NewRecorder()

	a.handleVerify(w, httptest.NewRequest(http.MethodGet, url, nil))

	if w.Code == http.StatusOK || w.Body.String() == "PWNED" {
		t.Fatalf("空 Token 握手必须被拒（403），但被放行: code=%d body=%q", w.Code, w.Body.String())
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("空 Token 握手 code=%d, want 403", w.Code)
	}
}

// 回归: W3-22 缺时间戳重放窗口会让合法签名被无限重放。
//
// 修复后 handleVerify/handleMessage 在校验签名前先调用 whauth.CheckReplayWindow。
// 即便签名完全合法，只要 timestamp 早于重放窗口（这里取窗口外的过旧时间戳），
// 也必须被拒绝（403），防止攻击者重放抓包到的历史请求。本测试钉死该窗口校验。
func TestReplayWindow_StaleTimestampRejected(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wx", AppSecret: "s"})

	// 过旧时间戳：比当前早一天，远超 replayWindow，落在窗口下界之外。
	staleTS := fmt.Sprintf("%d", time.Now().Add(-24*time.Hour).Unix())
	const nonce = "abc"

	t.Run("handleVerify 拒绝过旧时间戳的合法签名", func(t *testing.T) {
		// 用过旧 timestamp 算出"合法"签名——签名本身正确，只是时间戳过期。
		sig := sign(token, staleTS, nonce)
		url := fmt.Sprintf("/cb?signature=%s&timestamp=%s&nonce=%s&echostr=REPLAY", sig, staleTS, nonce)
		w := httptest.NewRecorder()
		a.handleVerify(w, httptest.NewRequest(http.MethodGet, url, nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("过旧时间戳重放应被拒(403), code=%d body=%q", w.Code, w.Body.String())
		}
		if w.Body.String() == "REPLAY" {
			t.Fatalf("过旧时间戳重放泄漏了 echostr（窗口校验失效）")
		}
	})

	t.Run("handleMessage 拒绝过旧时间戳的合法签名", func(t *testing.T) {
		var handlerCalled bool
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
			handlerCalled = true
			return &adapter.Reply{Content: "ok"}, nil
		}
		sig := sign(token, staleTS, nonce)
		r := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/cb?signature=%s&timestamp=%s&nonce=%s", sig, staleTS, nonce),
			strings.NewReader(buildTextXML("u", "gh", "重放消息", 1)))
		w := httptest.NewRecorder()
		a.handleMessage(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("过旧时间戳重放应被拒(403), code=%d body=%q", w.Code, w.Body.String())
		}
		if handlerCalled {
			t.Fatalf("过旧时间戳重放不应触达业务 handler（窗口校验失效）")
		}
	})
}

// =====================================================================
// 2. handleVerify：缺参数 / 缺 echostr 等边界
// =====================================================================

func TestHandleVerify_EdgeCases(t *testing.T) {
	const token = "tk"
	ts := nowTS() // W3-22：用当下时间戳通过重放窗口校验

	tests := []struct {
		name     string
		query    string
		wantCode int
		wantBody string // 仅在 200 时校验
	}{
		{
			name:     "缺 echostr 但签名正确 → 200 空 body",
			query:    fmt.Sprintf("signature=%s&timestamp=%s&nonce=abc", sign(token, ts, "abc"), ts),
			wantCode: http.StatusOK,
			wantBody: "",
		},
		{
			name:     "全部缺失 → 403",
			query:    "",
			wantCode: http.StatusForbidden,
		},
		{
			name:     "只有 echostr 无签名 → 403",
			query:    "echostr=hello",
			wantCode: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(config.WechatConfig{Token: token})
			w := httptest.NewRecorder()
			a.handleVerify(w, httptest.NewRequest(http.MethodGet, "/cb?"+tt.query, nil))
			if w.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d", w.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK && w.Body.String() != tt.wantBody {
				t.Fatalf("body = %q, want %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

// =====================================================================
// 3. handleMessage：入站 payload 解析 + 被动回复状态机
// =====================================================================

// buildTextXML 构造文本消息 XML。
func buildTextXML(from, to, content string, msgID int64) string {
	return fmt.Sprintf(
		`<xml><ToUserName><![CDATA[%s]]></ToUserName>`+
			`<FromUserName><![CDATA[%s]]></FromUserName>`+
			`<CreateTime>1700000000</CreateTime>`+
			`<MsgType><![CDATA[text]]></MsgType>`+
			`<Content><![CDATA[%s]]></Content>`+
			`<MsgId>%d</MsgId></xml>`,
		to, from, content, msgID)
}

// postSigned 构造带合法签名的 POST 请求（使用当下时间戳以通过重放窗口校验）。
func postSigned(token, body string) *http.Request {
	ts := nowTS()
	sig := sign(token, ts, "abc")
	return httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/cb?signature=%s&timestamp=%s&nonce=abc", sig, ts),
		strings.NewReader(body))
}

// TestHandleMessage_TextPassiveReply 正常路径：handler 在 4.5s 内返回，
// 走被动回复，HTTP 响应应是 XML，且 ToUserName/FromUserName 正确互换。
func TestHandleMessage_TextPassiveReply(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wx", AppSecret: "s"})

	var gotMsg *adapter.Message
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		gotMsg = m
		return &adapter.Reply{Content: "你好，回复内容"}, nil
	}

	body := buildTextXML("openid-user", "gh-account", "原始问题", 42)
	w := httptest.NewRecorder()
	a.handleMessage(w, postSigned(token, body))

	if w.Code != http.StatusOK {
		t.Fatalf("被动回复 code = %d, want 200", w.Code)
	}

	// 校验入站解析
	if gotMsg == nil {
		t.Fatal("handler 未被调用")
	}
	if gotMsg.Content != "原始问题" {
		t.Errorf("Content = %q, want 原始问题", gotMsg.Content)
	}
	if gotMsg.ChatID != "openid-user" || gotMsg.UserID != "openid-user" {
		t.Errorf("ChatID/UserID = %q/%q, want openid-user", gotMsg.ChatID, gotMsg.UserID)
	}
	if gotMsg.Platform != adapter.PlatformWechat {
		t.Errorf("Platform = %q", gotMsg.Platform)
	}
	if gotMsg.Metadata["msg_id"] != "42" {
		t.Errorf("metadata msg_id = %q, want 42", gotMsg.Metadata["msg_id"])
	}

	// 校验出站 XML
	var reply wechatReplyText
	if err := xml.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatalf("回复 XML 无法解析: %v\nbody=%s", err, w.Body.String())
	}
	if reply.MsgType != "text" {
		t.Errorf("回复 MsgType = %q, want text", reply.MsgType)
	}
	if reply.ToUserName != "openid-user" {
		t.Errorf("回复 ToUserName = %q, want openid-user（应回给发送方）", reply.ToUserName)
	}
	if reply.FromUserName != "gh-account" {
		t.Errorf("回复 FromUserName = %q, want gh-account（应为公众号）", reply.FromUserName)
	}
	if reply.Content != "你好，回复内容" {
		t.Errorf("回复 Content = %q", reply.Content)
	}
}

// TestHandleMessage_HandlerNilReplyTimesOut 当 handler 返回 nil reply 时，
// replyCh 永远不会被写入 → 走超时分支返回 "success"。
func TestHandleMessage_HandlerNilReply(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wx", AppSecret: "s"})
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return nil, nil // 不产生回复
	}

	// 缩短被动回复窗口不可行（硬编码 4.5s），但 nil reply 会立即让 goroutine 结束，
	// 此时 select 既无 replyCh 也无 ctx.Done（ctx 是 4.5s），会阻塞到 4.5s 超时。
	// 为避免 120s 默认超时拖慢，这里直接断言：执行不超过 ~5s 且最终落到 success。
	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		a.handleMessage(w, postSigned(token, buildTextXML("u", "gh", "q", 1)))
		close(done)
	}()
	select {
	case <-done:
		if w.Code != http.StatusOK || w.Body.String() != "success" {
			t.Fatalf("nil reply 超时分支 code=%d body=%q, want 200/success", w.Code, w.Body.String())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("handleMessage 在 nil reply 下未在 ~4.5s 超时窗口内返回")
	}
}

// TestHandleMessage_HandlerError handler 返回 error 时不应 panic，
// 走超时分支（replyCh 无写入）。
func TestHandleMessage_HandlerError(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wx", AppSecret: "s"})
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return nil, fmt.Errorf("boom")
	}

	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		a.handleMessage(w, postSigned(token, buildTextXML("u", "gh", "q", 1)))
		close(done)
	}()
	select {
	case <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("handler error 下 code=%d, want 200", w.Code)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("handler error 下 handleMessage 未返回")
	}
}

// TestHandleMessage_MsgTypeVariants 各种 MsgType 的解析与放行。
func TestHandleMessage_MsgTypeVariants(t *testing.T) {
	const token = "tk"

	tests := []struct {
		name     string
		body     string
		wantCode int
		wantBody string
	}{
		{
			name:     "image 非文本 → success",
			body:     `<xml><FromUserName>u</FromUserName><ToUserName>gh</ToUserName><MsgType>image</MsgType></xml>`,
			wantCode: http.StatusOK, wantBody: "success",
		},
		{
			name:     "event 事件 → success",
			body:     `<xml><FromUserName>u</FromUserName><MsgType>event</MsgType><Event>subscribe</Event></xml>`,
			wantCode: http.StatusOK, wantBody: "success",
		},
		{
			name:     "voice 非文本 → success",
			body:     `<xml><FromUserName>u</FromUserName><MsgType>voice</MsgType></xml>`,
			wantCode: http.StatusOK, wantBody: "success",
		},
		{
			name:     "空 MsgType → success（不等于 text）",
			body:     `<xml><FromUserName>u</FromUserName><Content>hi</Content></xml>`,
			wantCode: http.StatusOK, wantBody: "success",
		},
		{
			name:     "MsgType 大小写敏感 Text != text → success",
			body:     `<xml><FromUserName>u</FromUserName><MsgType>Text</MsgType><Content>hi</Content></xml>`,
			wantCode: http.StatusOK, wantBody: "success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(config.WechatConfig{Token: token})
			// 不设 handler：这些路径在解析后于 handler 调用前 return，故 handler 不会触发
			w := httptest.NewRecorder()
			a.handleMessage(w, postSigned(token, tt.body))
			if w.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d", w.Code, tt.wantCode)
			}
			if w.Body.String() != tt.wantBody {
				t.Fatalf("body = %q, want %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

// TestHandleMessage_MalformedAndEmptyXML XML 解析错误路径。
func TestHandleMessage_MalformedAndEmptyXML(t *testing.T) {
	const token = "tk"

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{name: "未闭合标签", body: "<broken", wantCode: http.StatusBadRequest},
		{name: "纯文本非 XML", body: "this is not xml at all", wantCode: http.StatusBadRequest},
		{name: "JSON 误投", body: `{"msg":"hi"}`, wantCode: http.StatusBadRequest},
		// 空 body: xml.Unmarshal([]byte("")) 返回 EOF 错误 → 400
		{name: "空 body", body: "", wantCode: http.StatusBadRequest},
		// 合法 XML 但根元素不匹配：Go 的 xml 不强制根名，仍解析为零值 → MsgType 为空 → 走非文本 success(200)
		{name: "合法 XML 但无字段", body: "<xml></xml>", wantCode: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(config.WechatConfig{Token: token})
			w := httptest.NewRecorder()
			a.handleMessage(w, postSigned(token, tt.body))
			if w.Code != tt.wantCode {
				t.Fatalf("body=%q code = %d, want %d", tt.body, w.Code, tt.wantCode)
			}
		})
	}
}

// TestHandleMessage_OversizedBodyRejected 超过 1 MiB 的 payload 应返回 413。
func TestHandleMessage_OversizedBodyRejected(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token})

	// 构造 > 1 MiB 的 XML（Content 填充大量字符）
	huge := strings.Repeat("A", (1<<20)+1024)
	body := buildTextXML("u", "gh", huge, 1)
	w := httptest.NewRecorder()
	a.handleMessage(w, postSigned(token, body))

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超长 body code = %d, want 413", w.Code)
	}
}

// TestHandleMessage_UnicodeContentPreserved 正常路径下 Unicode/emoji 内容应原样传递给 handler。
func TestHandleMessage_UnicodeContentPreserved(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wx", AppSecret: "s"})

	const content = "你好👋 こんにちは 🎉 <b>tag</b>"
	var got string
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		got = m.Content
		return &adapter.Reply{Content: "ok"}, nil
	}

	body := buildTextXML("u", "gh", content, 7)
	w := httptest.NewRecorder()
	a.handleMessage(w, postSigned(token, body))

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	if got != content {
		t.Fatalf("Unicode content = %q, want %q", got, content)
	}
}

// failingResponseWriter 是一个 Write 永远失败的 ResponseWriter，
// 用于触发被动回复 XML 编码/写入失败路径（W3-24）。
type failingResponseWriter struct {
	header http.Header
	code   int
}

func (f *failingResponseWriter) Header() http.Header {
	if f.header == nil {
		f.header = make(http.Header)
	}
	return f.header
}
func (f *failingResponseWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("simulated write failure")
}
func (f *failingResponseWriter) WriteHeader(code int) { f.code = code }

// 回归: W3-24 被动回复 XML 编码/写入失败必须记日志，不得被静默吞掉。
//
// 旧实现以 `_ = xml.NewEncoder(w).Encode(replyXML)` 丢弃了编码/写入错误，
// 写入失败时无任何日志，故障难以排查。修复后改为检查 error 并经 logger.Error 记录。
// 本测试把日志重定向到临时文件，用一个 Write 必失败的 ResponseWriter 触发该路径，
// 断言日志中确实出现了 W3-24 的错误记录（钉死"不再静默吞错误"）。
func TestPassiveReply_EncodeErrorLogged(t *testing.T) {
	// 将默认 logger 输出重定向到临时文件，测试结束后恢复。
	logPath := filepath.Join(t.TempDir(), "wechat.log")
	if err := logger.Init(&logger.Config{Level: "error", Format: "json", Output: logPath}); err != nil {
		t.Fatalf("初始化测试 logger 失败: %v", err)
	}
	t.Cleanup(func() { _ = logger.Init(logger.DefaultConfig()) })

	const token = "tk"
	a := New(config.WechatConfig{Token: token, AppID: "wx", AppSecret: "s"})
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "回复内容"}, nil
	}

	fw := &failingResponseWriter{}
	a.handleMessage(fw, postSigned(token, buildTextXML("openid-x", "gh", "问题", 1)))

	// 读取日志文件，确认编码/写入失败被记录（出现 W3-24 的错误消息关键字）。
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	logContent := string(data)
	if !strings.Contains(logContent, "微信被动回复 XML 编码写入失败") {
		t.Fatalf("被动回复编码/写入失败未记日志（被静默吞掉）, 日志内容=%q", logContent)
	}
}

// =====================================================================
// 4. Handler() 路由：HTTP 方法分发
// =====================================================================

func TestHandler_MethodDispatch(t *testing.T) {
	const token = "tk"
	a := New(config.WechatConfig{Token: token})
	h := a.Handler()

	t.Run("GET 走 verify", func(t *testing.T) {
		ts := nowTS() // W3-22：当下时间戳
		url := fmt.Sprintf("/?signature=%s&timestamp=%s&nonce=abc&echostr=E", sign(token, ts, "abc"), ts)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		if w.Code != http.StatusOK || w.Body.String() != "E" {
			t.Fatalf("GET verify code=%d body=%q", w.Code, w.Body.String())
		}
	})

	t.Run("PUT 不支持 → 405", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("PUT code=%d, want 405", w.Code)
		}
	})

	t.Run("DELETE 不支持 → 405", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE code=%d, want 405", w.Code)
		}
	})
}

// =====================================================================
// 5. getAccessToken：缓存、双检锁、错误路径、并发
// =====================================================================

// newWithServer 创建一个把 client 指向 httptest.Server 的适配器，
// 并把 wechatAPIBase 的请求重写到该 server。由于 wechatAPIBase 是包级常量
// 无法改写，这里直接替换 a.client.Transport 把所有出站请求劫持到测试 server。
func newWithServer(t *testing.T, cfg config.WechatConfig, srv *httptest.Server) *WechatAdapter {
	t.Helper()
	a := New(cfg)
	// 用一个把 host 重定向到测试 server 的 RoundTripper
	a.client = srv.Client()
	a.client.Transport = &redirectTransport{base: srv.URL, inner: srv.Client().Transport}
	return a
}

// redirectTransport 把所有请求重写到 base（测试 server），保留 path+query。
type redirectTransport struct {
	base  string
	inner http.RoundTripper
}

func (rt *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// 解析测试 server 地址，替换 scheme+host
	srvReq := req.Clone(req.Context())
	u := *req.URL
	// base 形如 http://127.0.0.1:port
	trimmed := strings.TrimPrefix(rt.base, "http://")
	u.Scheme = "http"
	u.Host = trimmed
	srvReq.URL = &u
	srvReq.Host = trimmed
	inner := rt.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	return inner.RoundTrip(srvReq)
}

func TestGetAccessToken_SuccessAndCache(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"TKN-123","expires_in":7200}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := newWithServer(t, config.WechatConfig{AppID: "wx", AppSecret: "s"}, srv)

	tok, err := a.getAccessToken(context.Background())
	if err != nil {
		t.Fatalf("首次取 token 出错: %v", err)
	}
	if tok != "TKN-123" {
		t.Fatalf("token = %q, want TKN-123", tok)
	}

	// 第二次应命中缓存，不再打 server
	tok2, err := a.getAccessToken(context.Background())
	if err != nil || tok2 != "TKN-123" {
		t.Fatalf("缓存取 token: %q err=%v", tok2, err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("token 端点命中 %d 次, want 1（应命中缓存）", got)
	}

	// 校验过期时间被设为 expires_in-300 秒后
	a.mu.RLock()
	exp := a.tokenExpiry
	a.mu.RUnlock()
	if time.Until(exp) > 7200*time.Second || time.Until(exp) < (7200-300-5)*time.Second {
		t.Fatalf("tokenExpiry 不在预期窗口: %v", time.Until(exp))
	}
}

func TestGetAccessToken_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errcode":40013,"errmsg":"invalid appid"}`)
	}))
	defer srv.Close()

	a := newWithServer(t, config.WechatConfig{AppID: "bad", AppSecret: "s"}, srv)
	_, err := a.getAccessToken(context.Background())
	if err == nil {
		t.Fatal("errcode!=0 应返回错误")
	}
	if !strings.Contains(err.Error(), "40013") {
		t.Fatalf("错误未包含 errcode: %v", err)
	}
}

func TestGetAccessToken_Concurrent(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(20 * time.Millisecond) // 放大竞态窗口
		fmt.Fprint(w, `{"access_token":"CONC","expires_in":7200}`)
	}))
	defer srv.Close()

	a := newWithServer(t, config.WechatConfig{AppID: "wx", AppSecret: "s"}, srv)

	const N = 30
	var wg sync.WaitGroup
	errs := make([]error, N)
	toks := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			toks[idx], errs[idx] = a.getAccessToken(context.Background())
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d 出错: %v", i, errs[i])
		}
		if toks[i] != "CONC" {
			t.Fatalf("goroutine %d token=%q", i, toks[i])
		}
	}
	// 双检锁应让网络请求次数远小于 N（理想 1 次，允许首批并发竞态导致的少量重复）。
	if h := atomic.LoadInt32(&hits); h > 5 {
		t.Fatalf("token 端点被打了 %d 次, 双检锁未生效（应接近 1）", h)
	}
}

// =====================================================================
// 6. sendCustomMessage / Send / SendStream
// =====================================================================

func TestSendCustomMessage_Success(t *testing.T) {
	var gotBody string
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			fmt.Fprint(w, `{"access_token":"T","expires_in":7200}`)
		case strings.Contains(r.URL.Path, "/message/custom/send"):
			gotToken = r.URL.Query().Get("access_token")
			b, _ := readAll(r.Body)
			gotBody = b
			fmt.Fprint(w, `{"errcode":0,"errmsg":"ok"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := newWithServer(t, config.WechatConfig{AppID: "wx", AppSecret: "s"}, srv)
	if err := a.sendCustomMessage(context.Background(), "openid-x", "你好"); err != nil {
		t.Fatalf("sendCustomMessage 出错: %v", err)
	}
	if gotToken != "T" {
		t.Errorf("access_token query = %q, want T", gotToken)
	}
	if !strings.Contains(gotBody, `"touser":"openid-x"`) {
		t.Errorf("请求体未含 touser: %s", gotBody)
	}
	if !strings.Contains(gotBody, "你好") {
		t.Errorf("请求体未含内容: %s", gotBody)
	}
}

func TestSendCustomMessage_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"T","expires_in":7200}`)
			return
		}
		fmt.Fprint(w, `{"errcode":45015,"errmsg":"response out of time limit"}`)
	}))
	defer srv.Close()

	a := newWithServer(t, config.WechatConfig{AppID: "wx", AppSecret: "s"}, srv)
	err := a.sendCustomMessage(context.Background(), "u", "hi")
	if err == nil || !strings.Contains(err.Error(), "45015") {
		t.Fatalf("客服消息 errcode 应透传, got %v", err)
	}
}

func TestSendReplyNow_NilReply(t *testing.T) {
	a := New(config.WechatConfig{AppID: "wx", AppSecret: "s"})
	// nil reply 应直接返回 nil，不触发任何网络调用
	if err := a.sendReplyNow(context.Background(), "u", nil); err != nil {
		t.Fatalf("nil reply 应返回 nil, got %v", err)
	}
}

func TestSend_NilReplyViaQueue(t *testing.T) {
	a := New(config.WechatConfig{AppID: "wx", AppSecret: "s"})
	defer a.Stop(context.Background())
	// queue.Send(nil) 直接返回 nil
	if err := a.Send(context.Background(), "u", nil); err != nil {
		t.Fatalf("Send nil reply 应返回 nil, got %v", err)
	}
}

func TestSendStream_MergesChunksAndStripsThinking(t *testing.T) {
	var gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"T","expires_in":7200}`)
			return
		}
		b, _ := readAll(r.Body)
		// 提取 content 字段
		gotContent = b
		fmt.Fprint(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	a := newWithServer(t, config.WechatConfig{AppID: "wx", AppSecret: "s"}, srv)

	ch := make(chan *adapter.ReplyChunk, 4)
	ch <- &adapter.ReplyChunk{Content: "答案：<think>"}
	ch <- &adapter.ReplyChunk{Content: "内部推理</think>"}
	ch <- &adapter.ReplyChunk{Content: "最终结果"}
	close(ch)

	if err := a.SendStream(context.Background(), "u", ch); err != nil {
		t.Fatalf("SendStream 出错: %v", err)
	}
	// <think>...</think> 应被剥离，只剩 "答案：最终结果"（首尾 TrimSpace）
	if !strings.Contains(gotContent, "答案") || !strings.Contains(gotContent, "最终结果") {
		t.Fatalf("合并内容不符: %s", gotContent)
	}
	if strings.Contains(gotContent, "内部推理") || strings.Contains(gotContent, "think") {
		t.Fatalf("thinking 未被剥离, 泄漏内容: %s", gotContent)
	}
}

func TestSendStream_PropagatesChunkError(t *testing.T) {
	// chunk.Error 非空应立即返回该错误，且不应触发网络发送
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("出错 chunk 不应触发网络调用, 但请求到了 %s", r.URL.Path)
	}))
	defer srv.Close()

	a := newWithServer(t, config.WechatConfig{AppID: "wx", AppSecret: "s"}, srv)

	ch := make(chan *adapter.ReplyChunk, 2)
	ch <- &adapter.ReplyChunk{Content: "部分"}
	ch <- &adapter.ReplyChunk{Error: fmt.Errorf("stream boom")}
	close(ch)

	err := a.SendStream(context.Background(), "u", ch)
	if err == nil || !strings.Contains(err.Error(), "stream boom") {
		t.Fatalf("SendStream 应透传 chunk 错误, got %v", err)
	}
}

// =====================================================================
// 7. ValidateConfig / Health：配置闭环
// =====================================================================

func TestValidateConfig_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.WechatConfig
	}{
		{"全空", config.WechatConfig{}},
		{"缺 AppSecret", config.WechatConfig{AppID: "wx"}},
		{"缺 Token", config.WechatConfig{AppID: "wx", AppSecret: "s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.cfg)
			if err := a.ValidateConfig(context.Background()); err == nil {
				t.Fatalf("缺字段应报错: %+v", tt.cfg)
			}
		})
	}
}

func TestValidateConfig_FetchesToken(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		fmt.Fprint(w, `{"access_token":"T","expires_in":7200}`)
	}))
	defer srv.Close()

	a := newWithServer(t, config.WechatConfig{AppID: "wx", AppSecret: "s", Token: "tk"}, srv)
	if err := a.ValidateConfig(context.Background()); err != nil {
		t.Fatalf("配置齐全且 token 成功, 应无错: %v", err)
	}
	if !hit {
		t.Fatal("ValidateConfig 应真正尝试获取 token")
	}
}

func TestHealth(t *testing.T) {
	t.Run("未 Attach handler → 错误", func(t *testing.T) {
		a := New(config.WechatConfig{AppID: "wx", AppSecret: "s", Token: "tk"})
		if err := a.Health(context.Background()); err == nil {
			t.Fatal("未 attach handler 应报错")
		}
	})

	t.Run("attached 且配置齐全 → nil", func(t *testing.T) {
		a := New(config.WechatConfig{AppID: "wx", AppSecret: "s", Token: "tk"})
		_ = a.Attach(func(context.Context, *adapter.Message) (*adapter.Reply, error) { return nil, nil })
		if err := a.Health(context.Background()); err != nil {
			t.Fatalf("配置齐全应健康: %v", err)
		}
	})

	t.Run("attached 但缺 Token → 错误", func(t *testing.T) {
		a := New(config.WechatConfig{AppID: "wx", AppSecret: "s"})
		_ = a.Attach(func(context.Context, *adapter.Message) (*adapter.Reply, error) { return nil, nil })
		if err := a.Health(context.Background()); err == nil {
			t.Fatal("缺 Token 应报错")
		}
	})
}

// =====================================================================
// 8. Attach / Name / Stop 边界
// =====================================================================

func TestAttach_EmptyCredentials(t *testing.T) {
	a := New(config.WechatConfig{})
	if err := a.Attach(func(context.Context, *adapter.Message) (*adapter.Reply, error) { return nil, nil }); err == nil {
		t.Fatal("空 AppID/AppSecret 应报错")
	}
	// 即便报错，handler 仍被设置（实现先赋值后校验）—— 记录此行为
	if a.handler == nil {
		t.Fatal("Attach 实现会先设置 handler 再校验, handler 不应为 nil")
	}
}

func TestName_CustomAndDefault(t *testing.T) {
	if got := New(config.WechatConfig{}).Name(); got != "wechat" {
		t.Errorf("默认名 = %q, want wechat", got)
	}
	if got := New(config.WechatConfig{Name: "wx-prod"}).Name(); got != "wx-prod" {
		t.Errorf("自定义名 = %q, want wx-prod", got)
	}
}

func TestStop_Idempotent(t *testing.T) {
	a := New(config.WechatConfig{AppID: "wx", AppSecret: "s"})
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("首次 Stop 出错: %v", err)
	}
	// 二次 Stop 不应 panic（queue 已停止）
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("二次 Stop 出错: %v", err)
	}
}

// readAll 读取并返回请求体字符串。
func readAll(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil && !errors.Is(err, io.EOF) {
		return string(b), err
	}
	return string(b), nil
}
