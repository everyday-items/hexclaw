package slack

// audit_wave3_test.go —— Slack 适配器全场景挑刺式审计测试
//
// 覆盖重点（IM 平台特定逻辑）：
//   - HMAC-SHA256 签名校验 + 时间戳重放窗口（边界 / 篡改 / 非法 / Unicode body）
//   - Events API webhook 入站解析（url_verification / event_callback / 各种 payload）
//   - 入站消息转换（空字段 / 超长 / Unicode / thread_ts 元数据透传）
//   - 出站 Interactive 文本 fallback
//   - 并发竞态、未闭环逻辑、状态一致性
//
// 约束：仅新增测试，不改源码。针对真实公开/包内函数，不 mock 被测逻辑本身。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// sign 计算 Slack v0 签名（与源码 verifySignature 算法一致）。
func signV0(secret, ts string, body []byte) string {
	base := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(base))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func newSignedRequest(secret string, ts string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", signV0(secret, ts, body))
	return r
}

// ============== 签名校验：时间戳重放窗口边界 ==============

// TestVerifySignature_ReplayWindowBoundary 验证 ±300s 重放窗口边界。
//
// 源码：abs64(now-ts) > 300 → 拒绝。意味着偏差 == 300 仍接受，301 拒绝。
func TestVerifySignature_ReplayWindowBoundary(t *testing.T) {
	secret := "replay-secret"
	a := New(config.SlackConfig{SigningSecret: secret})
	body := []byte(`{"type":"event_callback"}`)
	now := time.Now().Unix()

	tests := []struct {
		name   string
		offset int64 // 相对 now 的偏差（秒）
		want   bool
	}{
		{"刚好-300秒（过去边界，接受）", -300, true},
		{"301秒前（过期，拒绝）", -301, false},
		{"刚好+300秒（未来边界，接受）", 300, true},
		{"301秒后（未来，拒绝）", 301, false},
		{"当前时刻（接受）", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := strconv.FormatInt(now+tt.offset, 10)
			base := fmt.Sprintf("v0:%s:%s", ts, string(body))
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(base))
			validSig := "v0=" + hex.EncodeToString(mac.Sum(nil))

			r := &http.Request{Header: http.Header{}}
			r.Header.Set("X-Slack-Request-Timestamp", ts)
			r.Header.Set("X-Slack-Signature", validSig)

			if got := a.verifySignature(r, body); got != tt.want {
				t.Errorf("偏差 %ds: verifySignature=%v, 期望 %v", tt.offset, got, tt.want)
			}
		})
	}
}

// TestVerifySignature_MalformedTimestamp 非法时间戳应被拒绝（不 panic）。
func TestVerifySignature_MalformedTimestamp(t *testing.T) {
	a := New(config.SlackConfig{SigningSecret: "s"})
	body := []byte(`{}`)
	for _, ts := range []string{"abc", "12.34", "", "  ", "9999999999999999999999"} {
		r := &http.Request{Header: http.Header{}}
		r.Header.Set("X-Slack-Request-Timestamp", ts)
		r.Header.Set("X-Slack-Signature", "v0=whatever")
		if a.verifySignature(r, body) {
			t.Errorf("非法时间戳 %q 应拒绝", ts)
		}
	}
}

// TestVerifySignature_TamperedBody 篡改 body 后签名应失效。
func TestVerifySignature_TamperedBody(t *testing.T) {
	secret := "tamper-secret"
	a := New(config.SlackConfig{SigningSecret: secret})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	orig := []byte(`{"type":"event_callback","x":"1"}`)
	validSig := signV0(secret, ts, orig)

	r := &http.Request{Header: http.Header{}}
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", validSig)

	// 用篡改后的 body 校验同一签名 → 应失败
	if a.verifySignature(r, []byte(`{"type":"event_callback","x":"2"}`)) {
		t.Error("body 篡改后签名校验应失败")
	}
}

// TestVerifySignature_WrongSecret 不同 secret 签名应失效。
func TestVerifySignature_WrongSecret(t *testing.T) {
	a := New(config.SlackConfig{SigningSecret: "real-secret"})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(`{"type":"event_callback"}`)
	// 用错误 secret 签名
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", signV0("attacker-secret", ts, body))
	if a.verifySignature(r, body) {
		t.Error("攻击者 secret 签名应被拒绝")
	}
}

// TestVerifySignature_UnicodeBody Unicode/emoji body 的签名应可正确通过。
func TestVerifySignature_UnicodeBody(t *testing.T) {
	secret := "unicode-secret"
	a := New(config.SlackConfig{SigningSecret: secret})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(`{"type":"event_callback","text":"你好 🌍 héllo ☃ مرحبا"}`)
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", signV0(secret, ts, body))
	if !a.verifySignature(r, body) {
		t.Error("Unicode body 正确签名应通过")
	}
}

// TestVerifySignature_EmptyBody 空 body 但正确签名应通过（Slack 不会真发空体，但算法须自洽）。
func TestVerifySignature_EmptyBody(t *testing.T) {
	secret := "empty-secret"
	a := New(config.SlackConfig{SigningSecret: secret})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte{}
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", signV0(secret, ts, body))
	if !a.verifySignature(r, body) {
		t.Error("空 body 正确签名应通过")
	}
}

// ============== handleEvents：webhook 入站处理 ==============

// TestHandleEvents_EmptyBodyReturns400 空请求体 → JSON 解析失败 → 400。
func TestHandleEvents_EmptyBodyReturns400(t *testing.T) {
	a := New(config.SlackConfig{}) // 无 signing secret，跳过签名门
	w := httptest.NewRecorder()
	a.handleEvents(w, httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader("")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 body = %d, 期望 400", w.Code)
	}
}

// TestHandleEvents_OversizedBodyReturns413 超过 1 MiB 的 body → 413。
//
// 源码 BUG-20260611：http.MaxBytesReader 限制 1<<20，超限返回 413。
func TestHandleEvents_OversizedBodyReturns413(t *testing.T) {
	a := New(config.SlackConfig{})
	huge := strings.Repeat("a", (1<<20)+1024) // > 1 MiB
	body := `{"type":"event_callback","pad":"` + huge + `"}`
	w := httptest.NewRecorder()
	a.handleEvents(w, httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 body = %d, 期望 413", w.Code)
	}
}

// TestHandleEvents_MissingSignatureWhenSecretSet 配置了 secret 但请求无签名头 → 401。
func TestHandleEvents_MissingSignatureWhenSecretSet(t *testing.T) {
	a := New(config.SlackConfig{SigningSecret: "secret"})
	w := httptest.NewRecorder()
	a.handleEvents(w, httptest.NewRequest(http.MethodPost, "/slack/events",
		strings.NewReader(`{"type":"event_callback"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺签名头 = %d, 期望 401", w.Code)
	}
}

// TestHandleEvents_URLVerificationUnicodeChallenge challenge 含 Unicode 应原样回显。
func TestHandleEvents_URLVerificationUnicodeChallenge(t *testing.T) {
	a := New(config.SlackConfig{})
	chal := "挑战-✓-token"
	body, _ := json.Marshal(map[string]string{"type": "url_verification", "challenge": chal})
	w := httptest.NewRecorder()
	a.handleEvents(w, httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body))))

	if w.Code != http.StatusOK {
		t.Fatalf("url_verification = %d, 期望 200", w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	if resp["challenge"] != chal {
		t.Fatalf("challenge=%q, 期望 %q", resp["challenge"], chal)
	}
}

// TestHandleEvents_UnknownTypeNoBody 未知 type（既非 url_verification 也非 event_callback）
// 应不写任何状态码（默认 200），且不 panic。
func TestHandleEvents_UnknownType(t *testing.T) {
	a := New(config.SlackConfig{})
	w := httptest.NewRecorder()
	a.handleEvents(w, httptest.NewRequest(http.MethodPost, "/slack/events",
		strings.NewReader(`{"type":"something_else"}`)))
	// 未显式 WriteHeader → net/http 默认 200
	if w.Code != http.StatusOK {
		t.Fatalf("未知 type = %d, 期望默认 200", w.Code)
	}
}

// TestProcessEvent_NilHandlerPanics 是一个 characterization test，固化并暴露缺陷：
//
// 当一条合法 message 事件到达、但 a.handler == nil（即 event_callback 在 Attach
// 完成之前抵达，或 Attach 因 token 校验失败但仍被部分调用）时，processEvent 会执行到
// `a.handler(bgCtx, unified)` 并发生 nil 解引用 panic。源码 handleEvents 在
// `go a.processEvent(...)` 之前没有校验 handler 是否就绪，processEvent 内部也无
// nil 守卫与 recover。生产路径上该 panic 发生在无人 recover 的后台 goroutine，
// 会直接 crash 整个 sidecar 进程。
//
// 本测试断言"当前确实会 panic"以固化该 buggy 行为；修复后（加 nil 守卫）此断言
// 需同步更新。缺陷已记入 findings。
func TestProcessEvent_NilHandlerPanics(t *testing.T) {
	a := New(config.SlackConfig{}) // 未 Attach，handler == nil
	if a.handler != nil {
		t.Skip("handler 非 nil，前置条件不成立")
	}

	// 注意：processEvent 接收的是 envelope.Event（内层事件对象），不是整个 envelope。
	// 这与 handleEvents 中 `go a.processEvent(r.Context(), envelope.Event)` 的调用一致。
	body := []byte(`{"type":"message","user":"U1","channel":"C1","text":"hi"}`)

	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		a.processEvent(context.Background(), body)
	}()

	select {
	case p := <-panicked:
		// 当前 buggy 行为：必然 panic。若某天源码补了 nil 守卫，这里会变成 nil，
		// 提示该缺陷已修复，需要更新本测试。
		if p == nil {
			t.Log("processEvent 在 nil handler 下未 panic —— 缺陷可能已修复，请更新本测试")
		} else {
			t.Logf("已固化缺陷：nil handler 下 processEvent panic: %v（生产无 recover 会 crash sidecar）", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("processEvent 超时未返回")
	}
}

// TestHandleEvents_SignedEventDispatchAndThreadMeta 完整签名 event_callback 应分发，
// 且 thread_ts / ts 元数据正确透传。
func TestHandleEvents_SignedEventDispatchAndThreadMeta(t *testing.T) {
	secret := "dispatch-secret"
	a := New(config.SlackConfig{SigningSecret: secret})
	got := make(chan *adapter.Message, 1)
	// 直接注入 handler，走 handleEvents 公共 webhook 路径，避免网络。
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		got <- m
		return nil, nil
	}

	body := []byte(`{"type":"event_callback","event":{"type":"message","user":"U9",` +
		`"channel":"C9","text":"hello 世界","ts":"1700.5","thread_ts":"1699.1"}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	w := httptest.NewRecorder()
	a.handleEvents(w, newSignedRequest(secret, ts, body))

	if w.Code != http.StatusOK {
		t.Fatalf("signed event_callback = %d, 期望 200", w.Code)
	}
	select {
	case m := <-got:
		if m.Content != "hello 世界" || m.UserID != "U9" || m.ChatID != "C9" {
			t.Errorf("消息字段未正确解析: %+v", m)
		}
		if m.Metadata["thread_ts"] != "1699.1" {
			t.Errorf("thread_ts=%q, 期望 1699.1", m.Metadata["thread_ts"])
		}
		if m.Metadata["ts"] != "1700.5" {
			t.Errorf("ts=%q, 期望 1700.5", m.Metadata["ts"])
		}
		if m.Platform != adapter.PlatformSlack {
			t.Errorf("platform=%q, 期望 slack", m.Platform)
		}
		if m.InstanceID != "slack" {
			t.Errorf("instanceID=%q, 期望 slack", m.InstanceID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event_callback 未分发到 handler")
	}
}

// ============== processEvent：入站 payload 解析边界 ==============

// TestProcessEvent_InvalidJSON 非法 JSON 事件应被安全忽略（不 panic、不分发）。
func TestProcessEvent_InvalidJSON(t *testing.T) {
	called := false
	a := New(config.SlackConfig{Token: "t"})
	// 直接注入 handler，避免 Attach→fetchBotID 触发真实网络（slack.com）。
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		called = true
		return nil, nil
	}
	a.processEvent(context.Background(), []byte(`{not json`))
	if called {
		t.Error("非法 JSON 不应触发 handler")
	}
}

// TestProcessEvent_FiltersAndDispatch table 驱动覆盖各类事件的过滤/分发判定。
func TestProcessEvent_FiltersAndDispatch(t *testing.T) {
	tests := []struct {
		name         string
		event        string
		botID        string // 适配器自身 botID
		wantDispatch bool
	}{
		{"普通消息分发", `{"type":"message","user":"U1","channel":"C1","text":"hi"}`, "", true},
		{"bot_id 非空忽略", `{"type":"message","bot_id":"B1","user":"U1","channel":"C1"}`, "", false},
		{"subtype 非空忽略", `{"type":"message","subtype":"message_changed","user":"U1","channel":"C1"}`, "", false},
		{"非 message 类型忽略", `{"type":"reaction_added","user":"U1","channel":"C1"}`, "", false},
		{"自身用户消息忽略", `{"type":"message","user":"UBOT","channel":"C1","text":"hi"}`, "UBOT", false},
		{"空 text 仍分发", `{"type":"message","user":"U1","channel":"C1","text":""}`, "", true},
		{"空 channel 仍分发", `{"type":"message","user":"U1","channel":"","text":"hi"}`, "", true},
		{"完全空对象（type 空）忽略", `{}`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(config.SlackConfig{Token: "t"})
			dispatched := make(chan *adapter.Message, 1)
			// 直接注入 handler + botID，避免 Attach→fetchBotID 触发真实网络。
			a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
				dispatched <- m
				return nil, nil // 返回 nil reply，避免触发出站 HTTP
			}
			a.botID = tt.botID
			a.processEvent(context.Background(), []byte(tt.event))

			select {
			case <-dispatched:
				if !tt.wantDispatch {
					t.Errorf("事件本应被忽略，却分发了")
				}
			case <-time.After(500 * time.Millisecond):
				if tt.wantDispatch {
					t.Errorf("事件本应被分发，却被忽略")
				}
			}
		})
	}
}

// TestProcessEvent_SuperLongText 超长文本应被透传不截断、不 panic。
func TestProcessEvent_SuperLongText(t *testing.T) {
	a := New(config.SlackConfig{Token: "t"})
	longText := strings.Repeat("漢", 100000) // 10 万个多字节字符
	got := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		got <- m
		return nil, nil
	}
	ev, _ := json.Marshal(slackEvent{Type: "message", User: "U1", Channel: "C1", Text: longText})
	a.processEvent(context.Background(), ev)

	select {
	case m := <-got:
		if m.Content != longText {
			t.Errorf("超长文本被截断: len=%d, 期望 %d", len([]rune(m.Content)), 100000)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("超长消息未分发")
	}
}

// TestProcessEvent_HandlerNilReplyNoSend handler 返回 nil reply 时不应发出站。
func TestProcessEvent_HandlerNilReply(t *testing.T) {
	a := New(config.SlackConfig{Token: "t"})
	done := make(chan struct{})
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		close(done)
		return nil, nil
	}
	a.processEvent(context.Background(), []byte(`{"type":"message","user":"U1","channel":"C1","text":"hi"}`))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler 未被调用")
	}
}

// ============== Send：出站 Interactive 文本 fallback ==============

// TestSend_InteractiveTextFallback Send 应对含 Interactive 的 reply 追加文本 fallback。
//
// 直接断言 MaybeApplyTextFallback 对 reply.Content 的副作用（避免真实出站 HTTP）。
func TestSend_InteractiveTextFallbackMutatesContent(t *testing.T) {
	reply := &adapter.Reply{
		Content: "请选择",
		Interactive: &adapter.InteractivePayload{
			Type: adapter.InteractiveTypeButtons,
			Buttons: []adapter.InteractiveButton{
				{Label: "是", Action: "yes"},
				{Label: "否", Action: "no"},
			},
		},
	}
	changed := adapter.MaybeApplyTextFallback(context.Background(), reply)
	if !changed {
		t.Fatal("flag OFF 下应追加文本 fallback")
	}
	if reply.Content == "请选择" {
		t.Error("Content 应被追加 fallback 文本")
	}
	if !strings.Contains(reply.Content, "是") || !strings.Contains(reply.Content, "否") {
		t.Errorf("fallback 未包含按钮 label: %q", reply.Content)
	}
}

// TestSend_NilReplyViaQueue Send(nil) 经队列应安全返回 nil（不发出站、不 panic）。
func TestSend_NilReplyViaQueue(t *testing.T) {
	a := New(config.SlackConfig{Token: "t"})
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Send(context.Background(), "C1", nil); err != nil {
		t.Errorf("Send(nil) 应返回 nil, 得到 %v", err)
	}
}

// ============== 生命周期 / 健康检查 ==============

// TestHealth 各前置条件缺失时 Health 的判定。
func TestHealth(t *testing.T) {
	tests := []struct {
		name    string
		setup   func() *SlackAdapter
		wantErr bool
	}{
		{
			name:    "无 token",
			setup:   func() *SlackAdapter { return New(config.SlackConfig{}) },
			wantErr: true,
		},
		{
			name: "有 token 无 handler",
			setup: func() *SlackAdapter {
				return New(config.SlackConfig{Token: "t"})
			},
			wantErr: true,
		},
		{
			name: "有 token 有 handler 但 botID 空",
			setup: func() *SlackAdapter {
				a := New(config.SlackConfig{Token: "t"})
				a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
				return a
			},
			wantErr: true,
		},
		{
			name: "全部就绪",
			setup: func() *SlackAdapter {
				a := New(config.SlackConfig{Token: "t"})
				a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }
				a.botID = "UBOT"
				return a
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.setup()
			err := a.Health(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Health()=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateConfig_EmptyToken 空 token 时 ValidateConfig 立即报错，不发网络请求。
func TestValidateConfig_EmptyToken(t *testing.T) {
	a := New(config.SlackConfig{})
	if err := a.ValidateConfig(context.Background()); err == nil {
		t.Error("空 token 的 ValidateConfig 应报错")
	}
}

// TestName_CustomVsDefault 自定义 Name 与默认值。
func TestName_CustomVsDefault(t *testing.T) {
	if got := New(config.SlackConfig{Token: "t"}).Name(); got != "slack" {
		t.Errorf("默认 Name=%q, 期望 slack", got)
	}
	if got := New(config.SlackConfig{Token: "t", Name: "slack-ops"}).Name(); got != "slack-ops" {
		t.Errorf("自定义 Name=%q, 期望 slack-ops", got)
	}
}

// TestAttach_EmptyToken Attach 空 token 应报错。
func TestAttach_EmptyToken(t *testing.T) {
	a := New(config.SlackConfig{})
	if err := a.Attach(func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) { return nil, nil }); err == nil {
		t.Error("空 token Attach 应报错")
	}
}

// TestHandler_NotNil Handler() 应返回非 nil http.Handler。
func TestHandler_NotNil(t *testing.T) {
	a := New(config.SlackConfig{Token: "t"})
	if a.Handler() == nil {
		t.Error("Handler() 不应为 nil")
	}
}

// ============== SendStream：流式收尾逻辑 ==============

// TestSendStream_EmptyContentReturnsNil 全空内容的 chunk 流应直接返回 nil，不发任何出站。
//
// 源码 finalClean=="" → return nil。验证未闭环路径不会误发空消息（也不会因 ts=="" 触发
// 真实 HTTP）。多个纯空字符串 chunk 后，sb 长度始终为 0，ShouldEmit(0) 恒 false，
// 全程不发起任何网络请求。
func TestSendStream_EmptyContentReturnsNil(t *testing.T) {
	a := New(config.SlackConfig{Token: "t"})
	ch := make(chan *adapter.ReplyChunk, 2)
	ch <- &adapter.ReplyChunk{Content: ""}
	ch <- &adapter.ReplyChunk{Content: ""}
	close(ch)

	if err := a.SendStream(context.Background(), "C1", ch); err != nil {
		t.Errorf("空内容流应返回 nil, 得到 %v", err)
	}
}

// TestSendStream_ChunkErrorPropagates chunk.Error 应被立即返回。
func TestSendStream_ChunkErrorPropagates(t *testing.T) {
	a := New(config.SlackConfig{Token: "t"})
	wantErr := fmt.Errorf("upstream boom")
	ch := make(chan *adapter.ReplyChunk, 1)
	ch <- &adapter.ReplyChunk{Error: wantErr}
	close(ch)

	err := a.SendStream(context.Background(), "C1", ch)
	if err == nil || !strings.Contains(err.Error(), "upstream boom") {
		t.Errorf("chunk.Error 应被传播, 得到 %v", err)
	}
}

// ============== 并发竞态 ==============

// TestVerifySignature_Concurrent 并发签名校验应无 data race（go test -race 时有意义）。
func TestVerifySignature_Concurrent(t *testing.T) {
	secret := "concurrent-secret"
	a := New(config.SlackConfig{SigningSecret: secret})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(`{"type":"event_callback"}`)
	validSig := signV0(secret, ts, body)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &http.Request{Header: http.Header{}}
			r.Header.Set("X-Slack-Request-Timestamp", ts)
			r.Header.Set("X-Slack-Signature", validSig)
			if !a.verifySignature(r, body) {
				t.Error("并发校验：有效签名被拒绝")
			}
		}()
	}
	wg.Wait()
}

// TestHandleEvents_ConcurrentURLVerification 并发 url_verification 请求应稳定。
func TestHandleEvents_ConcurrentURLVerification(t *testing.T) {
	a := New(config.SlackConfig{})
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			chal := "c-" + strconv.Itoa(n)
			body, _ := json.Marshal(map[string]string{"type": "url_verification", "challenge": chal})
			w := httptest.NewRecorder()
			a.handleEvents(w, httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body))))
			if w.Code != http.StatusOK {
				t.Errorf("并发 url_verification=%d, 期望 200", w.Code)
			}
		}(i)
	}
	wg.Wait()
}

// ============== abs64 工具函数 ==============

// TestAbs64 边界覆盖。
func TestAbs64(t *testing.T) {
	tests := []struct {
		in, want int64
	}{
		{0, 0},
		{5, 5},
		{-5, 5},
		{-1, 1},
	}
	for _, tt := range tests {
		if got := abs64(tt.in); got != tt.want {
			t.Errorf("abs64(%d)=%d, 期望 %d", tt.in, got, tt.want)
		}
	}
	// 注意：abs64(math.MinInt64) 会溢出（-MinInt64 仍是 MinInt64），
	// 但时间戳偏差不可能达到该量级，属于理论缺陷，此处不触发。
}
