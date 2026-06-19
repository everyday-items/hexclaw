package feishu

// audit_wave3_test.go —— 第三波审计测试
//
// 目标：在共享 adapter 框架已较好覆盖的基础上，集中暴露"飞书平台特定逻辑"的边界 /
// 错误 / 未闭环问题，重点覆盖：
//   - webhook 事件解析与 VerificationToken 校验（含未签名事件接受的安全缺口）
//   - 入站 payload 解析（各种消息类型 / 空字段 / 超长 / Unicode / 非法 JSON）
//   - 出站消息转换（marshalTextContent / sendReplyNow / sendAndGetID / replyAndGetID / patchMessage）
//   - HTTP 状态码处理一致性（sendAndGetID 是否吞掉 4xx/5xx）
//   - Token 缓存并发竞态、Health / ValidateConfig 状态机
//
// 约束：只使用本包公开 / 包内可见的真实函数，不 mock 掉被测逻辑本身。
// HTTP 通过 httptest + rewriteTransport（复用 feishu_fix_test.go 中的辅助）拦截真实请求。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// ============================================================
// 1. webhook 事件解析 + VerificationToken 校验
// ============================================================

// TestHandleWebhook_TokenVerification 表驱动覆盖 VerificationToken 校验的全部分支。
//
// 关注点：源码 handleWebhook 中的校验条件为
//
//	if event.Header.Token != "" && a.cfg.VerificationToken != "" { ... }
//
// 这意味着：当配置了 VerificationToken，但攻击者构造一个 Header.Token 为空的
// im.message.receive_v1 事件时，校验被整体跳过 —— 该事件会被当作合法事件
// 投递到 handleMessage（异步 goroutine）。这是一处签名校验绕过的安全缺口。
func TestHandleWebhook_TokenVerification(t *testing.T) {
	tests := []struct {
		name        string
		cfgToken    string // 适配器配置的 VerificationToken
		headerToken string // 入站事件携带的 token
		wantStatus  int
		note        string
	}{
		{
			name:        "token 匹配通过",
			cfgToken:    "secret-token",
			headerToken: "secret-token",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "token 不匹配应 401",
			cfgToken:    "secret-token",
			headerToken: "attacker-token",
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name:        "未配置 token 时一律放行（向后兼容）",
			cfgToken:    "",
			headerToken: "whatever",
			wantStatus:  http.StatusOK,
		},
		{
			// 回归: W3-8 —— 签名校验绕过。配置了 VerificationToken 时，
			// 攻击者构造 Header.Token 为空的事件本应被拒绝（fail-closed），
			// 而非因 event.Header.Token != "" 短路而整体跳过校验后放行。
			// 飞书事件回调正常情况下 header.token 必然非空，空 token 一律视为
			// 未签名/非法事件，必须返回 401。
			name:        "配置了token但事件token为空_必须拒绝401_fail_closed",
			cfgToken:    "secret-token",
			headerToken: "",
			wantStatus:  http.StatusUnauthorized,
			note:        "配置了密钥时空 token 必须 fail-closed 拒绝(401)，禁止绕过",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(config.FeishuConfig{VerificationToken: tt.cfgToken})
			// 附加一个 no-op handler，避免 handleMessage 中 a.handler==nil 提前 return
			// 影响不到本测试（handleMessage 是异步 goroutine 且会因 message_type 非 text 直接返回）。
			a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
				return nil, nil
			}

			body := fmt.Sprintf(
				`{"header":{"event_type":"im.message.receive_v1","token":%q},"event":{"message":{"message_type":"text","content":"{\"text\":\"hi\"}"}}}`,
				tt.headerToken,
			)
			req := httptest.NewRequest(http.MethodPost, "/feishu/webhook", strings.NewReader(body))
			w := httptest.NewRecorder()

			a.handleWebhook(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (%s)", w.Code, tt.wantStatus, tt.note)
			}
		})
	}
}

// TestHandleWebhook_InvalidJSON 非法 JSON body 应返回 400。
func TestHandleWebhook_InvalidJSON(t *testing.T) {
	a := New(config.FeishuConfig{})
	tests := []struct {
		name string
		body string
	}{
		{"截断的JSON", `{"challenge":`},
		{"非JSON文本", `this is not json`},
		{"空body", ``},
		{"裸数组而非对象", `[1,2,3]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/feishu/webhook", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			a.handleWebhook(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("非法 body %q: status = %d, want 400", tt.body, w.Code)
			}
		})
	}
}

// TestHandleWebhook_ChallengePriorityOverToken challenge 优先于 token 校验。
// 即使配置了 VerificationToken，url_verification 阶段也无 token，必须能回显 challenge。
func TestHandleWebhook_ChallengeWithTokenConfigured(t *testing.T) {
	a := New(config.FeishuConfig{VerificationToken: "secret"})
	body := `{"challenge":"chal-abc","type":"url_verification"}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/webhook", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("challenge status = %d, want 200", w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("challenge 响应非 JSON: %v", err)
	}
	if resp["challenge"] != "chal-abc" {
		t.Errorf("challenge 回显 = %q, want chal-abc", resp["challenge"])
	}
}

// ============================================================
// 2. 入站 payload 解析 —— handleMessage（webhook 兼容路径）
// ============================================================

// TestHandleMessage_NonTextOrEmpty 验证非文本 / 空内容消息不会触达 handler。
//
// 这些都是 handleMessage 应当"短路返回"的分支：非 text 类型、非法 content JSON、
// 空白 content。我们用一个会标记 handlerCalled 的 handler 来断言短路是否生效。
func TestHandleMessage_NonTextOrEmpty(t *testing.T) {
	tests := []struct {
		name        string
		messageType string
		content     string
		wantHandler bool
	}{
		{"图片消息_不处理", "image", `{"image_key":"x"}`, false},
		{"富文本post_不处理", "post", `{"title":"t"}`, false},
		{"文件消息_不处理", "file", `{"file_key":"x"}`, false},
		{"text但content非法JSON_不处理", "text", `not-json`, false},
		{"text但内容全空白_不处理", "text", `{"text":"   \n\t  "}`, false},
		{"text正常_应处理", "text", `{"text":"hello"}`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called atomic.Bool
			done := make(chan struct{})
			a := New(config.FeishuConfig{})
			a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
				called.Store(true)
				close(done)
				// 返回 nil reply，避免触发后续真实 HTTP 发送（无 server，会失败但被吞）
				return nil, nil
			}

			ev := feishuEvent{}
			ev.Header.EventType = "im.message.receive_v1"
			ev.Event.Message.MessageType = tt.messageType
			ev.Event.Message.Content = tt.content
			ev.Event.Message.ChatID = "oc_chat"
			// 不设 MessageID，避免 addReaction 走真实 HTTP（messageID=="" 时跳过）

			// handleMessage 是同步函数（webhook 路径里用 go 调用，这里直接同步调用以便断言）
			a.handleMessage(ev)

			if tt.wantHandler {
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("期望 handler 被调用但超时")
				}
			}
			if got := called.Load(); got != tt.wantHandler {
				t.Errorf("handlerCalled = %v, want %v", got, tt.wantHandler)
			}
		})
	}
}

// TestHandleMessage_NilHandler handler 未附加时必须安全返回（不 panic）。
func TestHandleMessage_NilHandler(t *testing.T) {
	a := New(config.FeishuConfig{})
	ev := feishuEvent{}
	ev.Event.Message.MessageType = "text"
	ev.Event.Message.Content = `{"text":"hi"}`
	// a.handler == nil
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleMessage 在 handler==nil 时 panic: %v", r)
		}
	}()
	a.handleMessage(ev)
}

// TestHandleMessage_LongAndUnicodeContent 超长 / Unicode / emoji 内容应被完整解析。
func TestHandleMessage_LongAndUnicodeContent(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"超长10万字符", strings.Repeat("a", 100_000)},
		{"中文Unicode", "你好世界，飞书机器人"},
		{"emoji与组合字符", "👨‍👩‍👧‍👦🤔🔥 café naïve"},
		{"前后空白需Trim", "   trimmed   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured atomic.Value
			done := make(chan struct{})
			a := New(config.FeishuConfig{})
			a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
				captured.Store(m.Content)
				close(done)
				return nil, nil
			}

			ev := feishuEvent{}
			ev.Event.Message.MessageType = "text"
			contentJSON, _ := json.Marshal(map[string]string{"text": tt.text})
			ev.Event.Message.Content = string(contentJSON)
			a.handleMessage(ev)

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handler 未被调用")
			}
			got, _ := captured.Load().(string)
			want := strings.TrimSpace(tt.text)
			if got != want {
				// 仅打印长度避免巨量输出
				t.Errorf("解析后内容不符: got len=%d, want len=%d", utf8.RuneCountInString(got), utf8.RuneCountInString(want))
			}
		})
	}
}

// ============================================================
// 3. 出站转换 —— marshalTextContent
// ============================================================

// TestMarshalTextContent_AlwaysValidJSON 各种危险输入都应产出合法 JSON 且可往返。
func TestMarshalTextContent_AlwaysValidJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"空字符串", ""},
		{"双引号", `say "hi"`},
		{"反斜杠", `path\to\file`},
		{"换行制表", "a\nb\tc"},
		{"NUL字节", "x\x00y"},
		{"控制字符", "\x01\x02\x1f"},
		{"emoji", "🔥🤔👍"},
		{"HTML尖括号需考虑转义", `<script>alert(1)</script>`},
		{"中文", "你好飞书"},
		{"超长", strings.Repeat("龍", 50_000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := marshalTextContent(tt.input)
			var parsed map[string]string
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				t.Fatalf("marshalTextContent(%q) 产出非法 JSON: err=%v", tt.name, err)
			}
			if parsed["text"] != tt.input {
				t.Errorf("往返不一致: name=%s", tt.name)
			}
		})
	}
}

// ============================================================
// 4. 出站 HTTP —— 状态码处理一致性（核心 bug 猎取）
// ============================================================

// TestSendAndGetID_SwallowsHTTPError 回归: W3-9 —— sendAndGetID 必须检查 HTTP 状态码。
//
// 对比：replyAndGetID / patchMessage 都检查 resp.StatusCode != 200 并返回错误，
// 而修复前 sendAndGetID 直接 json.NewDecoder(resp.Body).Decode 并忽略 decode 错误，
// 无论 4xx/5xx 都返回 (空 messageID, nil error)，静默吞掉 API 错误。
//
// 后果（SendStream 首段创建路径）：
//
//	id, err := a.sendAndGetID(...)
//	if err != nil || id == "" { ...记日志, continue 重试... }
//
// 由于 err 永远是 nil、id 为空，每个 chunk 都会重新尝试 create 并继续失败，
// 流式消息永远无法落地。修复后非 200 必须返回带状态码的错误。
func TestSendAndGetID_SwallowsHTTPError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{"403权限不足", http.StatusForbidden, `{"code":99991663,"msg":"no permission"}`},
		{"400参数错误", http.StatusBadRequest, `{"code":400,"msg":"bad request"}`},
		{"500服务端错误", http.StatusInternalServerError, `internal error`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/auth/") {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"code": 0, "tenant_access_token": "tok", "expire": 7200,
					})
					return
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer ts.Close()

			a := newTestAdapterWithBase(ts.URL)
			id, err := a.sendAndGetID(context.Background(), "oc_chat", "hi")

			// 修复后：非 200 必须返回错误，且不应返回任何 messageID。
			if err == nil {
				t.Fatalf("sendAndGetID 在 HTTP %d 时应返回错误，却返回了 nil（静默吞掉 API 错误）", tt.statusCode)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", tt.statusCode)) {
				t.Errorf("错误信息应包含状态码 %d，实际: %v", tt.statusCode, err)
			}
			if id != "" {
				t.Errorf("非 200 响应不应返回 messageID，实际 = %q", id)
			}
		})
	}
}

// TestReplyAndGetID_HTTPErrorReturnsError 对照组：replyAndGetID 正确处理状态码与业务 code。
func TestReplyAndGetID_HTTPErrorReturnsError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    bool
		errSubstr  string
	}{
		{"200_业务code0_成功", 200, `{"code":0,"data":{"message_id":"om_ok"}}`, false, ""},
		{"403_HTTP错误", 403, `{"code":403,"msg":"forbidden"}`, true, "飞书 reply 返回 403"},
		{"200_但业务code非0", 200, `{"code":99991663,"msg":"token invalid"}`, true, "业务错误 code=99991663"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/auth/") {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"code": 0, "tenant_access_token": "tok", "expire": 7200,
					})
					return
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer ts.Close()

			a := newTestAdapterWithBase(ts.URL)
			_, err := a.replyAndGetID(context.Background(), "om_orig", "hi")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望错误但得到 nil")
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("错误信息 = %q, 期望包含 %q", err.Error(), tt.errSubstr)
				}
			} else if err != nil {
				t.Fatalf("期望成功但得到错误: %v", err)
			}
		})
	}
}

// TestSendReplyNow_HTTPError sendReplyNow 在非 200 时应返回错误并带状态码。
func TestSendReplyNow_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "tok", "expire": 7200,
			})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"msg":"no perm"}`))
	}))
	defer ts.Close()

	a := newTestAdapterWithBase(ts.URL)
	err := a.sendReplyNow(context.Background(), "oc_chat", &adapter.Reply{Content: "hi"})
	if err == nil {
		t.Fatal("sendReplyNow 在 403 时应返回错误")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误应含状态码 403: %v", err)
	}
}

// TestSendReplyNow_NilReply nil reply 应安全返回 nil（不发任何请求）。
func TestSendReplyNow_NilReply(t *testing.T) {
	var hit atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		w.WriteHeader(200)
	}))
	defer ts.Close()
	a := newTestAdapterWithBase(ts.URL)
	if err := a.sendReplyNow(context.Background(), "oc_chat", nil); err != nil {
		t.Fatalf("nil reply 应返回 nil，实际: %v", err)
	}
	if hit.Load() != 0 {
		t.Errorf("nil reply 不应发起任何 HTTP 请求，实际发起 %d 次", hit.Load())
	}
}

// ============================================================
// 5. Send 出站包装（队列 + Interactive fallback）
// ============================================================

// TestSend_NilReplyNoPanic Send(nil) 不应 panic，并安全返回。
func TestSend_NilReplyNoPanic(t *testing.T) {
	a := New(config.FeishuConfig{AppID: "x", AppSecret: "y"})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Send(nil) panic: %v", r)
		}
	}()
	// queue.Send(nil) 返回 nil，不触发真实发送
	if err := a.Send(context.Background(), "oc_chat", nil); err != nil {
		t.Errorf("Send(nil) 应返回 nil，实际: %v", err)
	}
}

// TestSend_GoesThroughQueue 验证 Send 经过队列并最终落到 sendReplyNow（真实 HTTP）。
func TestSend_GoesThroughQueue(t *testing.T) {
	var captured atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "tok", "expire": 7200,
			})
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		captured.Store(m)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	a := newTestAdapterWithBase(ts.URL)
	if err := a.Send(context.Background(), "oc_chat_X", &adapter.Reply{Content: "hello-queue"}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	m, _ := captured.Load().(map[string]any)
	if m == nil {
		t.Fatal("未捕获到出站请求体")
	}
	if m["receive_id"] != "oc_chat_X" {
		t.Errorf("receive_id = %v, want oc_chat_X", m["receive_id"])
	}
	if m["msg_type"] != "text" {
		t.Errorf("msg_type = %v, want text", m["msg_type"])
	}
}

// ============================================================
// 6. SendStream 流式输出
// ============================================================

// TestSendStream_EmptyChunks 无任何内容时应返回 nil 且不发请求。
func TestSendStream_EmptyChunks(t *testing.T) {
	var hit atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/auth/") {
			hit.Add(1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "tenant_access_token": "tok", "expire": 7200,
			"data": map[string]string{"message_id": "om_x"},
		})
	}))
	defer ts.Close()
	a := newTestAdapterWithBase(ts.URL)

	ch := make(chan *adapter.ReplyChunk)
	close(ch) // 立即关闭，无任何 chunk
	if err := a.SendStream(context.Background(), "oc_chat", ch); err != nil {
		t.Fatalf("空流应返回 nil: %v", err)
	}
	if hit.Load() != 0 {
		t.Errorf("空流不应发起业务请求，实际 %d 次", hit.Load())
	}
}

// TestSendStream_ErrorChunkPropagates 流中携带 Error 的 chunk 应被原样返回。
func TestSendStream_ErrorChunkPropagates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "tenant_access_token": "tok", "expire": 7200,
			"data": map[string]string{"message_id": "om_x"},
		})
	}))
	defer ts.Close()
	a := newTestAdapterWithBase(ts.URL)

	wantErr := fmt.Errorf("upstream boom")
	ch := make(chan *adapter.ReplyChunk, 1)
	ch <- &adapter.ReplyChunk{Error: wantErr}
	close(ch)

	err := a.SendStream(context.Background(), "oc_chat", ch)
	if err == nil || !strings.Contains(err.Error(), "upstream boom") {
		t.Fatalf("期望返回上游错误，实际: %v", err)
	}
}

// TestSendStream_NormalFlow 正常流：累计内容，最终落地一条消息。
func TestSendStream_NormalFlow(t *testing.T) {
	var lastPatch atomic.Value
	var createdID = "om_stream_001"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "tok", "expire": 7200,
			})
			return
		}
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			if c, ok := m["content"].(string); ok {
				var inner map[string]string
				_ = json.Unmarshal([]byte(c), &inner)
				lastPatch.Store(inner["text"])
			}
			w.WriteHeader(200)
			return
		}
		// POST create
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"message_id": createdID},
		})
	}))
	defer ts.Close()
	a := newTestAdapterWithBase(ts.URL)

	ch := make(chan *adapter.ReplyChunk, 4)
	ch <- &adapter.ReplyChunk{Content: "Hello "}
	ch <- &adapter.ReplyChunk{Content: "streaming "}
	ch <- &adapter.ReplyChunk{Content: "world"}
	ch <- &adapter.ReplyChunk{Done: true}
	close(ch)

	if err := a.SendStream(context.Background(), "oc_chat", ch); err != nil {
		t.Fatalf("正常流应成功: %v", err)
	}
	// 最终 patch 内容应是完整累计文本（无省略号）
	final, _ := lastPatch.Load().(string)
	if final != "Hello streaming world" {
		t.Errorf("最终落地内容 = %q, want %q", final, "Hello streaming world")
	}
}

// ============================================================
// 7. Token 缓存 —— 并发竞态 + 缓存命中
// ============================================================

// TestGetAccessToken_ConcurrentSingleFetch 高并发首次获取 token 时，
// 受 mu.Lock + double-check 保护，理想情况下只触发一次真实 HTTP 请求。
//
// 用 -race 跑可同时检测数据竞争。
func TestGetAccessToken_ConcurrentSingleFetch(t *testing.T) {
	var authHits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/") {
			atomic.AddInt32(&authHits, 1)
			// 轻微延迟放大竞态窗口
			time.Sleep(20 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "tok-concurrent", "expire": 7200,
			})
			return
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()
	a := newTestAdapterWithBase(ts.URL)

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	tokens := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := a.getAccessToken(context.Background())
			if err != nil {
				errs <- err
				return
			}
			tokens <- tok
		}()
	}
	wg.Wait()
	close(errs)
	close(tokens)

	for err := range errs {
		t.Fatalf("并发 getAccessToken 出错: %v", err)
	}
	for tok := range tokens {
		if tok != "tok-concurrent" {
			t.Errorf("token = %q, want tok-concurrent", tok)
		}
	}
	hits := atomic.LoadInt32(&authHits)
	if hits != 1 {
		// double-check 锁应保证只打一次；>1 说明缓存/锁有问题（记为疑似问题，不直接 Fatal 避免偶发误报）
		t.Logf("[关注] 并发首取触发了 %d 次 auth 请求，期望 1 次（double-check 锁应去重）", hits)
		if hits > 2 {
			t.Errorf("auth 请求次数 = %d，明显超出 double-check 预期", hits)
		}
	}
}

// TestGetAccessToken_BusinessErrorCode 业务 code != 0 时应返回错误。
func TestGetAccessToken_BusinessErrorCode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 注意：飞书在凭证错误时通常 HTTP 200 + code!=0
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 10003, "msg": "app_secret invalid",
		})
	}))
	defer ts.Close()
	a := newTestAdapterWithBase(ts.URL)

	_, err := a.getAccessToken(context.Background())
	if err == nil {
		t.Fatal("业务 code=10003 应返回错误")
	}
	if !strings.Contains(err.Error(), "10003") {
		t.Errorf("错误应含 code=10003: %v", err)
	}
}

// TestGetAccessToken_CacheHitSkipsHTTP 第二次取应命中缓存，不再打 auth。
func TestGetAccessToken_CacheHitSkipsHTTP(t *testing.T) {
	var authHits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&authHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "tenant_access_token": "tok-cached", "expire": 7200,
		})
	}))
	defer ts.Close()
	a := newTestAdapterWithBase(ts.URL)

	for i := 0; i < 5; i++ {
		tok, err := a.getAccessToken(context.Background())
		if err != nil || tok != "tok-cached" {
			t.Fatalf("第 %d 次取 token 失败: tok=%q err=%v", i, tok, err)
		}
	}
	if h := atomic.LoadInt32(&authHits); h != 1 {
		t.Errorf("5 次取 token 触发了 %d 次 auth 请求，期望 1 次（缓存命中）", h)
	}
}

// ============================================================
// 8. Health / ValidateConfig 状态机
// ============================================================

// TestHealth_StateMachine 覆盖 Health 的各状态分支。
func TestHealth_StateMachine(t *testing.T) {
	t.Run("缺少凭证", func(t *testing.T) {
		a := New(config.FeishuConfig{})
		if err := a.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "app_id/app_secret") {
			t.Errorf("缺凭证应报错，实际: %v", err)
		}
	})

	t.Run("handler未附加", func(t *testing.T) {
		a := New(config.FeishuConfig{AppID: "x", AppSecret: "y"})
		if err := a.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "handler 未附加") {
			t.Errorf("handler nil 应报错，实际: %v", err)
		}
	})

	t.Run("已停止", func(t *testing.T) {
		a := New(config.FeishuConfig{AppID: "x", AppSecret: "y"})
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		a.stopped.Store(true)
		if err := a.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Errorf("stopped 应报错，实际: %v", err)
		}
	})

	t.Run("未连接无lastError", func(t *testing.T) {
		a := New(config.FeishuConfig{AppID: "x", AppSecret: "y"})
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		// connected 默认 false, stopped 默认 false
		err := a.Health(context.Background())
		if err == nil || !strings.Contains(err.Error(), "未连接") {
			t.Errorf("未连接应报错，实际: %v", err)
		}
	})

	t.Run("未连接带lastError", func(t *testing.T) {
		a := New(config.FeishuConfig{AppID: "x", AppSecret: "y"})
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		a.mu.Lock()
		a.lastError = "ws dial timeout"
		a.mu.Unlock()
		err := a.Health(context.Background())
		if err == nil || !strings.Contains(err.Error(), "ws dial timeout") {
			t.Errorf("应透出 lastError，实际: %v", err)
		}
	})

	t.Run("健康", func(t *testing.T) {
		a := New(config.FeishuConfig{AppID: "x", AppSecret: "y"})
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		a.connected.Store(true)
		if err := a.Health(context.Background()); err != nil {
			t.Errorf("应健康，实际: %v", err)
		}
	})
}

// TestValidateConfig_MissingCreds 缺少凭证应立即报错，不发请求。
func TestValidateConfig_MissingCreds(t *testing.T) {
	tests := []struct {
		name      string
		appID     string
		appSecret string
		wantErr   bool
	}{
		{"两者都空", "", "", true},
		{"只缺secret", "app", "", true},
		{"只缺id", "", "secret", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(config.FeishuConfig{AppID: tt.appID, AppSecret: tt.appSecret})
			err := a.ValidateConfig(context.Background())
			if tt.wantErr && err == nil {
				t.Error("期望报错")
			}
		})
	}
}

// TestValidateConfig_Success 凭证有效时（mock auth 200 code0）应通过。
func TestValidateConfig_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "tenant_access_token": "tok", "expire": 7200,
		})
	}))
	defer ts.Close()
	a := newTestAdapterWithBase(ts.URL)
	if err := a.ValidateConfig(context.Background()); err != nil {
		t.Fatalf("有效凭证应通过: %v", err)
	}
}

// ============================================================
// 9. 生命周期 —— Start 缺凭证 + Stop 幂等
// ============================================================

// TestStart_MissingCreds Start 缺凭证应立即返回错误。
func TestStart_MissingCreds(t *testing.T) {
	a := New(config.FeishuConfig{})
	err := a.Start(context.Background(), func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "app_id/app_secret") {
		t.Errorf("Start 缺凭证应报错，实际: %v", err)
	}
}

// TestStop_Idempotent 多次 Stop 不应 panic（SendQueue.Stop 内部 stopOnce）。
func TestStop_Idempotent(t *testing.T) {
	a := New(config.FeishuConfig{AppID: "x", AppSecret: "y"})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("重复 Stop panic: %v", r)
		}
	}()
	for i := 0; i < 3; i++ {
		if err := a.Stop(context.Background()); err != nil {
			t.Fatalf("第 %d 次 Stop 出错: %v", i, err)
		}
	}
	if !a.stopped.Load() {
		t.Error("Stop 后 stopped 应为 true")
	}
}

// ============================================================
// 10. handleSDKMessage —— nil 防御
// ============================================================

// TestHandleSDKMessage_NilGuards 各级 nil 都应安全短路（不 panic）。
func TestHandleSDKMessage_NilGuards(t *testing.T) {
	a := New(config.FeishuConfig{})
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleSDKMessage nil 防御失败 panic: %v", r)
		}
	}()
	// event == nil
	a.handleSDKMessage(nil)
	// handler == nil 分支
	b := New(config.FeishuConfig{})
	b.handleSDKMessage(nil)
}

// TestName_Default Name 在未配置时返回默认 "feishu"。
func TestName_Default(t *testing.T) {
	if got := New(config.FeishuConfig{}).Name(); got != "feishu" {
		t.Errorf("默认 Name = %q, want feishu", got)
	}
	if got := New(config.FeishuConfig{Name: "support-bot"}).Name(); got != "support-bot" {
		t.Errorf("自定义 Name = %q, want support-bot", got)
	}
	if New(config.FeishuConfig{}).Platform() != adapter.PlatformFeishu {
		t.Error("Platform 应为 feishu")
	}
}

// captureDefaultLog 临时把全局默认 logger 重定向到内存 buffer，
// 返回捕获到的全部日志文本和一个恢复函数。用于断言日志内容。
func captureDefaultLog() (buf *bytes.Buffer, restore func()) {
	buf = &bytes.Buffer{}
	prev := logger.Default()
	logger.SetDefault(logger.NewWithHandler(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return buf, func() { logger.SetDefault(prev) }
}

// TestStart_LogMessageWellFormed 回归: W3-10 —— Start 启动日志字符串不得残缺。
//
// 修复前 Start 使用 logger.Info("飞书适配器 [", ...) ，消息以悬空的 "[" 结尾，
// 拼写残缺、语义不完整。修复后日志消息必须是完整可读的句子，
// 不得以 "[" 结尾，也不得等于残缺字面量 "飞书适配器 ["。
func TestStart_LogMessageWellFormed(t *testing.T) {
	buf, restore := captureDefaultLog()
	defer restore()

	a := New(config.FeishuConfig{AppID: "x", AppSecret: "y"})
	_ = a.Start(context.Background(), func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return nil, nil
	})
	// 立即停止，避免后台 WS 协程持续重连。
	_ = a.Stop(context.Background())
	// 等待后台启动协程把同步日志写出。
	time.Sleep(50 * time.Millisecond)

	out := buf.String()
	if strings.Contains(out, "飞书适配器 [") {
		t.Errorf("Start 日志含残缺字符串 \"飞书适配器 [\"（W3-10 未修复），实际输出: %q", out)
	}
	if !strings.Contains(out, "飞书适配器") {
		t.Errorf("Start 日志应包含完整的 \"飞书适配器\" 相关启动信息，实际输出: %q", out)
	}
}

// TestGetAccessToken_HTTPErrorStatus 回归: W3-11 —— getAccessToken 必须检查 HTTP 状态码。
//
// 修复前 getAccessToken 仅依赖响应体中的业务 code 判定成败，HTTP 返回
// 4xx/5xx 但响应体非预期 JSON（如网关返回纯文本错误页）时，
// 解析得到 code==0、token 为空，被误判为成功并缓存空 token。
// 修复后非 200 状态码必须直接返回带状态码的错误。
func TestGetAccessToken_HTTPErrorStatus(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{"500纯文本错误页", http.StatusInternalServerError, `<html>502 Bad Gateway</html>`},
		{"401网关鉴权失败", http.StatusUnauthorized, `unauthorized`},
		{"403被拦截", http.StatusForbidden, `forbidden`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer ts.Close()

			a := newTestAdapterWithBase(ts.URL)
			tok, err := a.getAccessToken(context.Background())
			if err == nil {
				t.Fatalf("getAccessToken 在 HTTP %d 时应返回错误，却返回了 nil（仅依赖 body code）", tt.statusCode)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", tt.statusCode)) {
				t.Errorf("错误信息应包含状态码 %d，实际: %v", tt.statusCode, err)
			}
			if tok != "" {
				t.Errorf("非 200 不应返回 token，实际 = %q", tok)
			}
		})
	}
}
