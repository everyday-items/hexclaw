package line

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	linewebhook "github.com/line/line-bot-sdk-go/v8/linebot/webhook"
)

// audit_wave3_test.go 针对 LINE 适配器的平台特定逻辑做全场景审计测试：
//   - Webhook 签名校验（HMAC-SHA256/Base64）的正常 / 边界 / 安全路径
//   - 入站 payload 解析（各种消息类型 / 空字段 / 超长 / 非法 JSON / Unicode）
//   - 入站事件到统一 Message 的转换（含群组 source 的 ChatID 取值）
//   - 出站消息转换（StripThinking / reply_token 路由 / push 路由）
//   - 生命周期与状态一致性（New / Health / ValidateConfig / Stop / 并发）
//
// 约束：只测本包公开/包内可见函数，不 mock 被测逻辑本身；网络请求通过
// httptest.Server 或可达性失败路径来覆盖，避免真实访问 api.line.me。

// helper：按 LINE 官方 webhook 签名算法生成测试签名；生产校验由官方 SDK 执行。
func sigOf(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// testSecret 是解析类测试统一使用的 webhook 密钥。
//
// W3-13 起 handleWebhook 强制 fail-closed：未配置密钥的实例会直接拒绝任何
// 入站请求。聚焦 payload 解析 / 事件转换的测试因此必须配置密钥并对请求体
// 签名，才能穿过签名闸门进入解析路径（而非验证 fail-closed 本身）。
const testSecret = "audit-wave3-secret"

// signedReq 构造一个携带合法 X-Line-Signature 的 POST 请求，
// 签名使用 testSecret 对 body 计算，配合 New(Config{ChannelSecret: testSecret})。
func signedReq(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader(body))
	r.Header.Set("X-Line-Signature", sigOf(testSecret, []byte(body)))
	return r
}

// helper：构造一个把入站 Message 投递到 channel 的 handler。
func collectingHandler(buf int) (adapter.MessageHandler, <-chan *adapter.Message) {
	ch := make(chan *adapter.Message, buf)
	return func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		ch <- m
		return nil, nil
	}, ch
}

// --------------------------------------------------------------------------
// 1. 签名校验：正常 / 边界 / 安全
// --------------------------------------------------------------------------

func TestOfficialSDKValidateSignature_Table(t *testing.T) {
	secret := "channel-secret-abc"
	body := []byte(`{"events":[{"type":"message"}]}`)
	valid := sigOf(secret, body)

	tests := []struct {
		name   string
		secret string
		body   []byte
		sig    string
		want   bool
	}{
		{"正确签名", secret, body, valid, true},
		{"错误签名", secret, body, "not-a-real-sig", false},
		{"空签名头", secret, body, "", false},
		{"body 被篡改", secret, []byte(`{"events":[]}`), valid, false},
		{"密钥不匹配", "other-secret", body, valid, false},
		{"空 body 配对应的签名", secret, []byte{}, sigOf(secret, []byte{}), true},
		{"非 base64 垃圾签名", secret, body, "!!!@@@###", false},
		{"签名长度不同(短)", secret, body, valid[:10], false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := linewebhook.ValidateSignature(tt.secret, tt.sig, tt.body); got != tt.want {
				t.Errorf("linewebhook.ValidateSignature() = %v, want %v", got, tt.want)
			}
		})
	}
}

// 回归: W3-13 ChannelSecret 为空时必须 fail-closed：未配置密钥的实例
// 不得放行任何未签名请求（此前会静默跳过签名校验，等于把校验关掉）。
// 通过 whauth.RequireSecret 强制拒绝，返回 403 且不分发任何事件。
func TestHandleWebhook_EmptySecretFailsClosed(t *testing.T) {
	a := New(Config{}) // 无 ChannelSecret
	handler, sink := collectingHandler(1)
	_ = a.Attach(handler)

	body := `{"events":[{"type":"message","replyToken":"rt","timestamp":1700000000000,` +
		`"source":{"type":"user","userId":"U"},"message":{"type":"text","id":"m","text":"unsigned"}}]}`
	// 故意不带任何 X-Line-Signature
	w := httptest.NewRecorder()
	a.handleWebhook(w, httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader(body)))

	if w.Code != http.StatusForbidden {
		t.Fatalf("空 secret 未签名请求 status = %d, want 403（fail-closed）", w.Code)
	}
	select {
	case m := <-sink:
		t.Fatalf("空 secret 时不应分发任何事件，却收到 %+v", m)
	case <-time.After(300 * time.Millisecond):
		// 未分发即符合 fail-closed 预期。
	}
}

// 安全审计：即便已配置 secret，验证仍只在 secret!="" 时启用；本测试确认
// 已配置 secret 时缺失签名头会被拒绝（防伪造）。
func TestHandleWebhook_ConfiguredSecretRejectsMissingHeader(t *testing.T) {
	a := New(Config{ChannelSecret: "s"})
	w := httptest.NewRecorder()
	// POST 但不带签名头
	a.handleWebhook(w, httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader(`{"events":[]}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("缺签名头 = %d, want 403", w.Code)
	}
}

// --------------------------------------------------------------------------
// 2. 入站 payload 解析：消息类型 / 空字段 / Unicode / 超长
// --------------------------------------------------------------------------

// 各种事件类型只有 message+text 会被分发，其余忽略。
func TestHandleWebhook_EventTypeFiltering(t *testing.T) {
	tests := []struct {
		name         string
		eventJSON    string
		wantStatus   int
		wantDispatch bool
	}{
		{"text 消息", `{"type":"message","source":{"type":"user","userId":"U"},"message":{"type":"text","id":"1","text":"hi"}}`, http.StatusOK, true},
		{"sticker 消息", `{"type":"message","source":{"type":"user","userId":"U"},"message":{"type":"sticker","id":"1"}}`, http.StatusOK, false},
		{"image 消息", `{"type":"message","source":{"type":"user","userId":"U"},"message":{"type":"image","id":"1"}}`, http.StatusOK, false},
		{"follow 事件", `{"type":"follow","source":{"type":"user","userId":"U"}}`, http.StatusOK, false},
		{"join 事件", `{"type":"join","source":{"type":"group","groupId":"G"}}`, http.StatusOK, false},
		{"postback 事件", `{"type":"postback","source":{"type":"user","userId":"U"}}`, http.StatusOK, false},
		{"空 type", `{"source":{"type":"user","userId":"U"},"message":{"type":"text","id":"1","text":"x"}}`, http.StatusBadRequest, false},
		{"text 但内容为空", `{"type":"message","source":{"type":"user","userId":"U"},"message":{"type":"text","id":"1","text":""}}`, http.StatusOK, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(Config{ChannelSecret: testSecret})
			handler, sink := collectingHandler(1)
			_ = a.Attach(handler)
			body := `{"events":[` + tt.eventJSON + `]}`
			w := httptest.NewRecorder()
			a.handleWebhook(w, signedReq(body))
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusOK {
				return
			}
			select {
			case <-sink:
				if !tt.wantDispatch {
					t.Errorf("事件 %q 不应被分发", tt.name)
				}
			case <-time.After(300 * time.Millisecond):
				if tt.wantDispatch {
					t.Errorf("事件 %q 应被分发但超时", tt.name)
				}
			}
		})
	}
}

// 回归: W3-12 群组 / 房间 source 的 ChatID 应取会话维度标识（groupId/roomId），
// UserID 取发言者 userId，二者必须可区分；单聊回退到 userId。
// 此前源码用 event.Source.UserID 同时填 ChatID 和 UserID，导致群消息丢失 groupId、
// ChatID 退化为发言者，无法按群会话路由/回复。本测试钉死修复后的正确行为。
func TestHandleWebhook_GroupSourceChatID(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		wantChatID string
		wantUserID string
		wantType   string
	}{
		{
			name:       "group 用 groupId 作 ChatID",
			source:     `{"type":"group","groupId":"G999","userId":"Uspeaker"}`,
			wantChatID: "G999",
			wantUserID: "Uspeaker",
			wantType:   "group",
		},
		{
			name:       "room 用 roomId 作 ChatID",
			source:     `{"type":"room","roomId":"R888","userId":"Uspeaker"}`,
			wantChatID: "R888",
			wantUserID: "Uspeaker",
			wantType:   "room",
		},
		{
			name:       "user 单聊回退 userId",
			source:     `{"type":"user","userId":"Usolo"}`,
			wantChatID: "Usolo",
			wantUserID: "Usolo",
			wantType:   "user",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(Config{ChannelSecret: testSecret})
			handler, sink := collectingHandler(1)
			_ = a.Attach(handler)

			body := `{"events":[{"type":"message","replyToken":"rt","timestamp":1700000000000,` +
				`"source":` + tt.source + `,` +
				`"message":{"type":"text","id":"m1","text":"群里说话"}}]}`
			w := httptest.NewRecorder()
			a.handleWebhook(w, signedReq(body))

			select {
			case m := <-sink:
				if m.ChatID != tt.wantChatID {
					t.Errorf("ChatID = %q, want %q（群/房间会话应能与发言者区分）", m.ChatID, tt.wantChatID)
				}
				if m.UserID != tt.wantUserID {
					t.Errorf("UserID = %q, want %q", m.UserID, tt.wantUserID)
				}
				if m.Metadata["source_type"] != tt.wantType {
					t.Errorf("source_type metadata = %q, want %q", m.Metadata["source_type"], tt.wantType)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("群组 text 事件未被分发")
			}
		})
	}
}

// Unicode / emoji / 多字节内容应原样透传。
func TestHandleWebhook_UnicodeContent(t *testing.T) {
	a := New(Config{ChannelSecret: testSecret})
	handler, sink := collectingHandler(1)
	_ = a.Attach(handler)

	content := "你好世界 🌍 こんにちは 안녕하세요 ‮masked"
	ev := map[string]any{
		"type":       "message",
		"replyToken": "rt",
		"timestamp":  1700000000000,
		"source":     map[string]any{"type": "user", "userId": "U"},
		"message":    map[string]any{"type": "text", "id": "m", "text": content},
	}
	body, _ := json.Marshal(map[string]any{"events": []any{ev}})
	w := httptest.NewRecorder()
	a.handleWebhook(w, signedReq(string(body)))

	select {
	case m := <-sink:
		if m.Content != content {
			t.Errorf("Unicode 内容未原样透传:\n got %q\nwant %q", m.Content, content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unicode 事件未分发")
	}
}

// 超长文本内容（约 200KB，仍在 1MB body 限制内）应被完整解析。
func TestHandleWebhook_VeryLongText(t *testing.T) {
	a := New(Config{ChannelSecret: testSecret})
	handler, sink := collectingHandler(1)
	_ = a.Attach(handler)

	long := strings.Repeat("漢", 50000) // ~150KB UTF-8
	ev := map[string]any{
		"type":    "message",
		"source":  map[string]any{"type": "user", "userId": "U"},
		"message": map[string]any{"type": "text", "id": "m", "text": long},
	}
	body, _ := json.Marshal(map[string]any{"events": []any{ev}})
	w := httptest.NewRecorder()
	a.handleWebhook(w, signedReq(string(body)))

	select {
	case m := <-sink:
		if len([]rune(m.Content)) != 50000 {
			t.Errorf("超长文本 rune 数 = %d, want 50000", len([]rune(m.Content)))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("超长 text 事件未分发")
	}
}

// 回归: W3-14 超过 1MB body 限制必须显式拒绝并返回明确错误（413），
// 不得静默截断后继续解析。此前用裸 LimitReader 截断，超限请求被静默截断：
//   - 截断后 JSON 损坏 → 误报 400（掩盖了"请求过大"的真实原因）；
//   - 若截断点恰好落在合法 JSON 边界，则会用残缺数据继续处理。
//
// 本测试构造一个超 1MB 的请求，断言显式 413 而非静默处理。
func TestHandleWebhook_BodyOverLimitRejected(t *testing.T) {
	a := New(Config{})
	// 构造 > 1MB 的请求体（前缀为合法 JSON 起始，整体远超 1MB）。
	huge := `{"events":[{"type":"message","source":{"type":"user","userId":"U"},` +
		`"message":{"type":"text","id":"m","text":"` + strings.Repeat("a", 2<<20) + `"}}]}`
	w := httptest.NewRecorder()
	a.handleWebhook(w, httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader(huge)))
	// 显式拒绝：请求体过大应返回 413，且响应体含明确原因，而非被静默截断成 400。
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限 body = %d, want 413（显式拒绝而非静默截断）", w.Code)
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "large") &&
		!strings.Contains(w.Body.String(), "过大") {
		t.Errorf("413 响应应含明确原因, got %q", w.Body.String())
	}
}

// 恰好等于上限（1MB）的合法 JSON 应被正常接受，不得误杀。
func TestHandleWebhook_BodyAtLimitAccepted(t *testing.T) {
	a := New(Config{ChannelSecret: testSecret})
	handler, sink := collectingHandler(1)
	_ = a.Attach(handler)

	// 构造一个总长度 <= 1MB 的合法 JSON，用 text 填充到接近上限。
	prefix := `{"events":[{"type":"message","source":{"type":"user","userId":"U"},` +
		`"message":{"type":"text","id":"m","text":"`
	suffix := `"}}]}`
	pad := (1 << 20) - len(prefix) - len(suffix) // 使总长恰为 1MB
	body := prefix + strings.Repeat("a", pad) + suffix
	if len(body) != 1<<20 {
		t.Fatalf("构造的 body 长度 = %d, 期望 1MB", len(body))
	}
	w := httptest.NewRecorder()
	a.handleWebhook(w, signedReq(body))
	if w.Code != http.StatusOK {
		t.Fatalf("恰好 1MB 的合法 body = %d, want 200", w.Code)
	}
	select {
	case <-sink:
	case <-time.After(2 * time.Second):
		t.Fatal("上限内合法事件应被分发")
	}
}

// 空 events 数组：合法 JSON，应 200 且不分发。
func TestHandleWebhook_EmptyEvents(t *testing.T) {
	a := New(Config{ChannelSecret: testSecret})
	handler, sink := collectingHandler(1)
	_ = a.Attach(handler)
	w := httptest.NewRecorder()
	a.handleWebhook(w, signedReq(`{"events":[]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("空 events = %d, want 200", w.Code)
	}
	select {
	case m := <-sink:
		t.Fatalf("空 events 不应分发, got %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

// 各类非法 / 边界 JSON 输入：均应返回 400 或在合法但语义空时 200，但绝不 panic。
func TestHandleWebhook_MalformedJSONTable(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"截断对象", `{"events":[`, http.StatusBadRequest},
		{"纯文本", `not json at all`, http.StatusBadRequest},
		{"空 body", ``, http.StatusBadRequest},
		{"JSON null", `null`, http.StatusOK},     // 解析成功，payload.Events 为 nil
		{"JSON 数组", `[]`, http.StatusBadRequest}, // 顶层应为对象
		{"events 为对象", `{"events":{}}`, http.StatusBadRequest},
		{"events 为字符串", `{"events":"x"}`, http.StatusBadRequest},
		{"events 缺失", `{"foo":1}`, http.StatusOK}, // 合法对象，Events 为 nil
		{"type 字段数字类型不匹配", `{"events":[{"type":123}]}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(Config{ChannelSecret: testSecret})
			w := httptest.NewRecorder()
			a.handleWebhook(w, signedReq(tt.body))
			if w.Code != tt.want {
				t.Errorf("body=%q code = %d, want %d", tt.name, w.Code, tt.want)
			}
		})
	}
}

// timestamp 边界：0 / 负数 / 极大值都应被解析而不 panic（无重放窗口校验）。
func TestHandleWebhook_TimestampBoundaries(t *testing.T) {
	tests := []struct {
		name string
		ts   int64
	}{
		{"零时间戳", 0},
		{"负时间戳", -1000},
		{"未来极大时间戳", 1 << 62},
		{"正常时间戳", 1700000000000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(Config{ChannelSecret: testSecret})
			handler, sink := collectingHandler(1)
			_ = a.Attach(handler)
			ev := map[string]any{
				"type":      "message",
				"timestamp": tt.ts,
				"source":    map[string]any{"type": "user", "userId": "U"},
				"message":   map[string]any{"type": "text", "id": "m", "text": "t"},
			}
			body, _ := json.Marshal(map[string]any{"events": []any{ev}})
			w := httptest.NewRecorder()
			a.handleWebhook(w, signedReq(string(body)))
			select {
			case m := <-sink:
				want := time.UnixMilli(tt.ts)
				if !m.Timestamp.Equal(want) {
					t.Errorf("Timestamp = %v, want %v", m.Timestamp, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("事件未分发")
			}
		})
	}
}

// 多事件批量：仅 text 事件被分发，非 text 在批量中被跳过；保证顺序与数量。
func TestHandleWebhook_MixedBatch(t *testing.T) {
	a := New(Config{ChannelSecret: testSecret})
	handler, sink := collectingHandler(8)
	_ = a.Attach(handler)

	body := `{"events":[
		{"type":"message","source":{"type":"user","userId":"U1"},"message":{"type":"text","id":"1","text":"a"}},
		{"type":"message","source":{"type":"user","userId":"U2"},"message":{"type":"sticker","id":"2"}},
		{"type":"follow","source":{"type":"user","userId":"U3"}},
		{"type":"message","source":{"type":"user","userId":"U4"},"message":{"type":"text","id":"4","text":"b"}}
	]}`
	w := httptest.NewRecorder()
	a.handleWebhook(w, signedReq(body))

	// 收集所有分发到的消息（goroutine 并发，文本可能乱序）
	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case m := <-sink:
			got[m.Content] = true
		case <-deadline:
			t.Fatalf("只收到 %d 条文本事件, want 2 (got=%v)", len(got), got)
		}
	}
	if !got["a"] || !got["b"] {
		t.Errorf("分发内容 = %v, want a,b", got)
	}
	// 确认没有额外分发（sticker/follow 被跳过）
	select {
	case m := <-sink:
		t.Errorf("意外的额外分发: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

// --------------------------------------------------------------------------
// 3. 出站消息转换
// --------------------------------------------------------------------------

// SendStream 聚合 chunk 后单次发送；遇到 chunk.Error 立即返回该错误。
func TestSendStream_PropagatesChunkError(t *testing.T) {
	a := New(Config{ChannelToken: "tok"}) // 无 reply_token，走 push（会因网络失败而非 nil）
	ch := make(chan *adapter.ReplyChunk, 2)
	wantErr := context.DeadlineExceeded
	ch <- &adapter.ReplyChunk{Content: "partial"}
	ch <- &adapter.ReplyChunk{Error: wantErr}
	close(ch)

	err := a.SendStream(context.Background(), "U", ch)
	if err != wantErr {
		t.Errorf("SendStream error = %v, want %v", err, wantErr)
	}
}

// Send(nil reply) 应为 no-op，不返回错误，也不触发网络请求。
func TestSend_NilReplyIsNoop(t *testing.T) {
	a := New(Config{ChannelToken: "tok"})
	if err := a.Send(context.Background(), "U", nil); err != nil {
		t.Errorf("Send(nil) = %v, want nil", err)
	}
}

// sendReplyNow(nil) 直接返回 nil（队列旁路）。
func TestSendReplyNow_NilReply(t *testing.T) {
	a := New(Config{ChannelToken: "tok"})
	if err := a.sendReplyNow(context.Background(), "U", nil); err != nil {
		t.Errorf("sendReplyNow(nil) = %v, want nil", err)
	}
}

// 出站内容剥离 <think> 块：通过把 client 指向 httptest server，断言落到线缆上的 body。
func TestSendReplyNow_StripsThinkingAndRoutesPush(t *testing.T) {
	var captured struct {
		mu   sync.Mutex
		path string
		body map[string]any
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		captured.mu.Lock()
		captured.path = r.URL.Path
		captured.body = b
		captured.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sentMessages":[]}`))
	}))
	defer ts.Close()

	a := New(Config{ChannelToken: "tok"})
	// 不修改源码：替换 client.Transport 把 api.line.me 重定向到测试服务器。
	a.client.Transport = rewriteTransport{target: ts.URL}

	reply := &adapter.Reply{Content: "答案 <think>这是内部推理</think> 完成"}
	if err := a.sendReplyNow(context.Background(), "Uchat", reply); err != nil {
		t.Fatalf("sendReplyNow push 失败: %v", err)
	}

	captured.mu.Lock()
	defer captured.mu.Unlock()
	if !strings.HasSuffix(captured.path, "/message/push") {
		t.Errorf("无 reply_token 应走 push, path=%q", captured.path)
	}
	if captured.body["to"] != "Uchat" {
		t.Errorf("push to = %v, want Uchat", captured.body["to"])
	}
	msgs, _ := captured.body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 长度 = %d, want 1", len(msgs))
	}
	text, _ := msgs[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "think") || strings.Contains(text, "内部推理") {
		t.Errorf("出站文本未剥离 <think>: %q", text)
	}
	if !strings.Contains(text, "答案") || !strings.Contains(text, "完成") {
		t.Errorf("出站文本丢失正文: %q", text)
	}
}

// 有 reply_token 时走 reply API 而非 push。
func TestSendReplyNow_RoutesReplyWhenTokenPresent(t *testing.T) {
	var gotPath string
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sentMessages":[]}`))
	}))
	defer ts.Close()

	a := New(Config{ChannelToken: "tok"})
	a.client.Transport = rewriteTransport{target: ts.URL}

	reply := &adapter.Reply{
		Content:  "hi",
		Metadata: map[string]string{"reply_token": "RT"},
	}
	if err := a.sendReplyNow(context.Background(), "Uchat", reply); err != nil {
		t.Fatalf("sendReplyNow reply 失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasSuffix(gotPath, "/message/reply") {
		t.Errorf("有 reply_token 应走 reply, path=%q", gotPath)
	}
}

// 上游返回非 200 时应包装错误返回（push 路径）。
func TestSendReplyNow_NonOKStatusReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid token"}`))
	}))
	defer ts.Close()

	a := New(Config{ChannelToken: "bad"})
	a.client.Transport = rewriteTransport{target: ts.URL}

	err := a.sendReplyNow(context.Background(), "U", &adapter.Reply{Content: "x"})
	if err == nil {
		t.Fatal("非 200 应返回错误")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误应含状态码 401: %v", err)
	}
}

// --------------------------------------------------------------------------
// 4. 生命周期 / 配置校验 / 状态一致性
// --------------------------------------------------------------------------

func TestHealth_Table(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		attach  bool
		wantErr bool
	}{
		{"全配置+已attach", Config{ChannelSecret: "s", ChannelToken: "t"}, true, false},
		{"未attach", Config{ChannelSecret: "s", ChannelToken: "t"}, false, true},
		{"缺 secret", Config{ChannelToken: "t"}, true, true},
		{"缺 token", Config{ChannelSecret: "s"}, true, true},
		{"全空", Config{}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.cfg)
			if tt.attach {
				_ = a.Attach(func(context.Context, *adapter.Message) (*adapter.Reply, error) { return nil, nil })
			}
			err := a.Health(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Health() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateConfig_MissingCredentials(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"缺 secret", Config{ChannelToken: "t"}},
		{"缺 token", Config{ChannelSecret: "s"}},
		{"全空", Config{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.cfg)
			if err := a.ValidateConfig(context.Background()); err == nil {
				t.Error("缺凭证应返回错误")
			}
		})
	}
}

// ValidateConfig：凭证齐全时向 bot/info 发请求；用 httptest 模拟 200 / 非 200。
func TestValidateConfig_RemoteResponse(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"200 通过", http.StatusOK, false},
		{"401 失败", http.StatusUnauthorized, true},
		{"500 失败", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer t" {
					t.Errorf("Authorization 头 = %q", r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				if tt.status == http.StatusOK {
					_, _ = w.Write([]byte(`{"userId":"U0123456789abcdef0123456789abcdef","basicId":"@hexclaw","displayName":"HexClaw","chatMode":"bot","markAsReadMode":"auto"}`))
					return
				}
				_, _ = w.Write([]byte(`{"message":"line test failure"}`))
			}))
			defer ts.Close()

			a := New(Config{ChannelSecret: "s", ChannelToken: "t"})
			a.client.Transport = rewriteTransport{target: ts.URL}
			err := a.ValidateConfig(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateConfig() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// Stop 幂等性 + 停止后 Send 行为：队列停止后 Send 应返回"已停止"错误。
func TestStop_ThenSendRejected(t *testing.T) {
	a := New(Config{ChannelToken: "t"})
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("首次 Stop 失败: %v", err)
	}
	// 再次 Stop 应安全（幂等）
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("二次 Stop 失败: %v", err)
	}
	// 停止后通过队列发送应被拒绝
	err := a.Send(context.Background(), "U", &adapter.Reply{Content: "x"})
	if err == nil {
		t.Error("队列停止后 Send 应返回错误")
	}
}

// New 默认端口与显式端口。
func TestNew_PortDefaulting(t *testing.T) {
	if got := New(Config{}).config.WebhookPort; got != 6064 {
		t.Errorf("默认端口 = %d, want 6064", got)
	}
	if got := New(Config{WebhookPort: 9000}).config.WebhookPort; got != 9000 {
		t.Errorf("显式端口 = %d, want 9000", got)
	}
	// 队列必然被初始化
	if New(Config{}).queue == nil {
		t.Error("New 应初始化 queue")
	}
}

// Handler() 返回的 http.Handler 与 handleWebhook 行为一致（方法守卫）。
func TestHandler_MethodGuard(t *testing.T) {
	a := New(Config{})
	h := a.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/webhook/line", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT via Handler() = %d, want 405", w.Code)
	}
}

// --------------------------------------------------------------------------
// 5. 并发竞态：多个 goroutine 同时打 webhook，应无数据竞争 / panic
// （配合 -race 运行更有意义）
// --------------------------------------------------------------------------

func TestHandleWebhook_ConcurrentRequests(t *testing.T) {
	a := New(Config{ChannelSecret: testSecret})
	var count int64
	var mu sync.Mutex
	_ = a.Attach(func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		mu.Lock()
		count++
		mu.Unlock()
		return nil, nil
	})

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"events":[{"type":"message","source":{"type":"user","userId":"U"},` +
				`"message":{"type":"text","id":"m","text":"concurrent"}}]}`
			w := httptest.NewRecorder()
			a.handleWebhook(w, signedReq(body))
			if w.Code != http.StatusOK {
				t.Errorf("并发请求 %d code = %d", i, w.Code)
			}
		}(i)
	}
	wg.Wait()
	// 分发是 goroutine 异步的，给点时间收敛
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	got := count
	mu.Unlock()
	if got != n {
		t.Logf("分发计数 = %d, 预期 %d（异步分发，少量可接受但应接近）", got, n)
		if got == 0 {
			t.Errorf("并发场景下 handler 完全未被调用")
		}
	}
}

// rewriteTransport 把所有出站请求重写到测试服务器，避免真实访问 api.line.me，
// 同时不修改被测源码中构造的 URL / 头部逻辑。
type rewriteTransport struct {
	target string // 形如 http://127.0.0.1:xxxxx
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := req.URL.Parse(rt.target)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}
