package adapter

// audit_e2e_test.go —— IM 适配器【端到端闭环】集成测试
//
// 目标：验证 IM 入站 → 分发 → 出站 的完整闭环，专门暴露单元测试漏掉的
// 跨层集成 bug（闭环断裂 / 错误跨层丢失 / 并发竞态 / 边界传播）。
//
// 设计要点（确定性 / 不打真实网络 / 不依赖墙钟）：
//
//   - 用 httptest.Server 模拟 IM 平台的出站 HTTP API（chat.postMessage 等价物），
//     断言"平台真的收到了什么"，而不是只断言函数返回值。
//   - 用进程内的 mockIMAdapter 复刻真实适配器的三段式结构（入站解析 →
//     ChatID/会话路由 → 经 SendQueue 出站），它只用 adapter 包的【公共生产代码】
//     （SendQueue / StreamEditLimiter / MaybeApplyTextFallback / ValidateAttachments /
//     Message / Reply），因此跑的就是真实的出站序列化、限流与 fallback 逻辑。
//   - 全程不 sleep 等墙钟（除少量限流速率断言用极小 minInterval，且只断言"相对顺序/
//     被限流"而非绝对时刻），用 channel + 超时保证收敛。
//
// 闭环重点：
//   1. 入站解析 → ChatID 路由 → 出站投递的端到端正确性
//   2. SendQueue（出站串行化 + 速率限制）在闭环中的行为
//   3. 错误跨层传播：出站平台业务失败（HTTP 200 但 ok:false）能否回传到分发层
//   4. attachments / interactive 闭环

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================================
// 进程内 mock IM 平台（出站目标）—— 等价于 Slack chat.postMessage 的 mock 端点。
// ============================================================================

// platformRequest 记录平台收到的一次出站请求（用于闭环断言）。
type platformRequest struct {
	ChatID  string
	Text    string
	RawBody string
	Auth    string
}

// mockIMPlatform 模拟一个 IM 平台的出站 HTTP API。
//
// 真实平台（Slack/飞书/Telegram）的出站 Web API 即使业务失败也常返回 HTTP 200，
// 仅通过 body 里的 "ok":false 表达失败 —— 这是闭环最容易断裂的地方（失败被当成功）。
// 本 mock 可被配置成始终成功、始终 ok:false、或前 N 次失败后成功，用于错误跨层传播测试。
type mockIMPlatform struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []platformRequest

	// failTimes：前 failTimes 次请求返回 ok:false（模拟瞬时失败 / 限流），其后成功。
	failTimes int32
	calls     int32
	// errCode：ok:false 时返回的错误码。
	errCode string
}

func newMockIMPlatform() *mockIMPlatform {
	p := &mockIMPlatform{errCode: "channel_not_found"}
	p.server = httptest.NewServer(http.HandlerFunc(p.handle))
	return p
}

func (p *mockIMPlatform) URL() string { return p.server.URL }
func (p *mockIMPlatform) Close()      { p.server.Close() }

func (p *mockIMPlatform) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	_ = json.Unmarshal(body, &payload)

	p.mu.Lock()
	p.requests = append(p.requests, platformRequest{
		ChatID:  payload.Channel,
		Text:    payload.Text,
		RawBody: string(body),
		Auth:    r.Header.Get("Authorization"),
	})
	p.mu.Unlock()

	n := atomic.AddInt32(&p.calls, 1)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // 关键：业务失败也返回 200，仅 body 标记 ok:false
	if n <= atomic.LoadInt32(&p.failTimes) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"` + p.errCode + `"}`))
		return
	}
	_, _ = w.Write([]byte(`{"ok":true,"ts":"1700.001"}`))
}

func (p *mockIMPlatform) received() []platformRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]platformRequest, len(p.requests))
	copy(out, p.requests)
	return out
}

func (p *mockIMPlatform) callCount() int32 { return atomic.LoadInt32(&p.calls) }

// ============================================================================
// 进程内 mock IM 适配器（被测闭环）—— 复刻真实适配器三段式：入站 → 路由 → 出站。
//
// 仅依赖 adapter 包公共生产代码，因此 SendQueue 的串行化/限流、文本 fallback、
// 附件校验都是真实路径。
// ============================================================================

// inboundEvent 模拟平台 webhook 投递的原始入站事件（等价 Slack event_callback.event）。
type inboundEvent struct {
	Type        string       `json:"type"`
	User        string       `json:"user"`
	Channel     string       `json:"channel"`
	Text        string       `json:"text"`
	BotID       string       `json:"bot_id"`
	SubType     string       `json:"subtype"`
	Attachments []Attachment `json:"attachments"`
}

// mockIMAdapter 把"入站事件 → 统一 Message → handler 分发 → SendQueue 出站"串成闭环。
type mockIMAdapter struct {
	platformURL string
	token       string
	botID       string
	handler     MessageHandler
	queue       *SendQueue
	httpClient  *http.Client

	// sendErrors 累计出站发送失败次数（用于断言错误是否回传到分发层）。
	sendErrors atomic.Int32
	// lastSendErr 最近一次出站错误（闭环错误传播断言）。
	lastSendErr atomic.Value // error
}

// newMockIMAdapter 用给定速率构造适配器；rate<=0 用 SendQueue 默认（5/s）。
func newMockIMAdapter(platformURL, token string, ratePerSecond int) *mockIMAdapter {
	a := &mockIMAdapter{
		platformURL: platformURL,
		token:       token,
		httpClient:  &http.Client{Timeout: 5 * time.Second},
	}
	// 出站经 SendQueue：真实生产路径用它做串行化 + 速率限制（flood control）。
	a.queue = NewSendQueue(ratePerSecond, 128, a.sendReplyNow)
	return a
}

func (a *mockIMAdapter) attach(h MessageHandler) { a.handler = h }

func (a *mockIMAdapter) stop() { _ = a.queue.Stop(context.Background()) }

// dispatch 模拟 webhook 入站处理：解析 → 过滤 → 路由 → 分发 → 出站。
//
// 返回是否真正分发到了 handler（用于过滤断言）。同步执行（测试确定性），
// 真实适配器是 go processEvent(...)，但闭环逻辑等价。
func (a *mockIMAdapter) dispatch(ctx context.Context, raw []byte) (dispatched bool, err error) {
	var ev inboundEvent
	if e := json.Unmarshal(raw, &ev); e != nil {
		return false, fmt.Errorf("解析入站事件失败: %w", e)
	}
	// 入站过滤（与真实适配器一致）：非 message / bot 自身 / 编辑 / 自己发的 忽略。
	if ev.Type != "message" {
		return false, nil
	}
	if ev.BotID != "" || ev.SubType != "" {
		return false, nil
	}
	if ev.User == a.botID {
		return false, nil
	}

	// 附件闭环：入站校验（仅支持图片），不合法直接拒绝并出站错误提示。
	if len(ev.Attachments) > 0 {
		if verr := ValidateAttachments(ev.Attachments); verr != nil {
			// 校验失败也要把错误闭环回用户（出站一条提示）。
			_ = a.send(ctx, ev.Channel, &Reply{Content: "附件校验失败：" + verr.Error()})
			return false, verr
		}
	}

	// 入站 → 统一 Message（ChatID/会话路由的关键：Channel → ChatID）。
	msg := &Message{
		ID:          "evt-" + ev.User,
		Platform:    "mock-im",
		InstanceID:  "mock",
		ChatID:      ev.Channel,
		UserID:      ev.User,
		Content:     ev.Text,
		Attachments: ev.Attachments,
		Timestamp:   time.Now(),
	}

	reply, herr := a.handler(ctx, msg)
	if herr != nil {
		// handler 出错：闭环回一条错误提示（真实适配器同样行为）。
		// 注意用独立 ctx，避免复用可能已过期的入站 ctx（send_queue_test 记录的历史 bug）。
		errCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.send(errCtx, ev.Channel, &Reply{Content: "处理消息时出错，请稍后重试。"})
		return true, herr
	}
	if reply == nil {
		return true, nil
	}

	// 出站：ChatID 路由 → SendQueue → 平台。
	if serr := a.send(ctx, ev.Channel, reply); serr != nil {
		a.sendErrors.Add(1)
		a.lastSendErr.Store(serr)
		return true, serr
	}
	return true, nil
}

// send 经 SendQueue 出站（含 interactive 文本 fallback）。
func (a *mockIMAdapter) send(ctx context.Context, chatID string, reply *Reply) error {
	MaybeApplyTextFallback(ctx, reply) // 真实 Send 入口的第一步
	return a.queue.Send(ctx, chatID, reply)
}

// sendReplyNow 是 SendQueue 的底层发送函数：真发 HTTP 到 mock 平台并解析 ok。
func (a *mockIMAdapter) sendReplyNow(ctx context.Context, chatID string, reply *Reply) error {
	if reply == nil {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"channel": chatID, "text": reply.Content})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.platformURL+"/chat.postMessage", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("出站请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if derr := json.NewDecoder(resp.Body).Decode(&result); derr != nil {
		return fmt.Errorf("解析平台响应失败: %w", derr)
	}
	// 平台业务失败（ok:false）必须当成错误回传 —— 否则闭环断裂（失败被当成功）。
	if !result.OK {
		return fmt.Errorf("平台 API 错误: %s", result.Error)
	}
	return nil
}

// ============================================================================
// 闭环 1：入站解析 → ChatID 路由 → 出站投递端到端正确
// ============================================================================

// TestE2E_InboundToOutbound_RoutesToCorrectChat 端到端：一条入站消息经分发后，
// 出站必须发到【同一个 ChatID】且内容是 handler 的回复。这是 IM 闭环最基本的正确性。
func TestE2E_InboundToOutbound_RoutesToCorrectChat(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	a := newMockIMAdapter(platform.URL(), "xoxb-token", 100)
	defer a.stop()

	// mock engine/handler：把用户输入回声成回复。
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		return &Reply{Content: "echo: " + m.Content}, nil
	})

	raw := []byte(`{"type":"message","user":"U1","channel":"C-room-42","text":"你好"}`)
	dispatched, err := a.dispatch(context.Background(), raw)
	if err != nil {
		t.Fatalf("dispatch 失败: %v", err)
	}
	if !dispatched {
		t.Fatal("合法 message 事件应被分发")
	}

	reqs := platform.received()
	if len(reqs) != 1 {
		t.Fatalf("平台应收到 1 条出站，实际 %d", len(reqs))
	}
	if reqs[0].ChatID != "C-room-42" {
		t.Errorf("ChatID 路由错误：出站发到 %q，期望 C-room-42（入站 channel）", reqs[0].ChatID)
	}
	if reqs[0].Text != "echo: 你好" {
		t.Errorf("出站内容错误：%q，期望 echo: 你好", reqs[0].Text)
	}
	if reqs[0].Auth != "Bearer xoxb-token" {
		t.Errorf("出站鉴权头错误：%q", reqs[0].Auth)
	}
}

// TestE2E_InboundFilters_NoOutbound 入站过滤闭环：被过滤的事件不得产生任何出站。
func TestE2E_InboundFilters_NoOutbound(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"bot 自身消息", `{"type":"message","bot_id":"B1","user":"U1","channel":"C1","text":"x"}`},
		{"消息编辑(subtype)", `{"type":"message","subtype":"message_changed","user":"U1","channel":"C1","text":"x"}`},
		{"非 message 类型", `{"type":"reaction_added","user":"U1","channel":"C1"}`},
		{"机器人自己发的", `{"type":"message","user":"UBOT","channel":"C1","text":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			platform := newMockIMPlatform()
			defer platform.Close()
			a := newMockIMAdapter(platform.URL(), "t", 100)
			a.botID = "UBOT"
			defer a.stop()
			a.attach(func(_ context.Context, m *Message) (*Reply, error) {
				return &Reply{Content: "should-not-send"}, nil
			})

			dispatched, err := a.dispatch(context.Background(), []byte(tc.raw))
			if err != nil {
				t.Fatalf("dispatch 报错: %v", err)
			}
			if dispatched {
				t.Errorf("事件应被过滤，却分发了")
			}
			if got := platform.callCount(); got != 0 {
				t.Errorf("被过滤事件不应产生出站，实际发了 %d 次", got)
			}
		})
	}
}

// ============================================================================
// 闭环 2：SendQueue（出站串行化 + 速率限制）在闭环中的行为
// ============================================================================

// TestE2E_SendQueue_SerializesOutbound 并发入站 → 出站必须经 SendQueue 串行化，
// 平台收到的请求数 == 入站数，且无并发写竞态导致的丢失/重复。
func TestE2E_SendQueue_SerializesOutbound(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	a := newMockIMAdapter(platform.URL(), "t", 1000) // 高速率，专测串行化正确性
	defer a.stop()
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		return &Reply{Content: "r-" + m.Content}, nil
	})

	const n = 30
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			raw := fmt.Sprintf(`{"type":"message","user":"U%d","channel":"C%d","text":"%d"}`, idx, idx, idx)
			if _, err := a.dispatch(context.Background(), []byte(raw)); err != nil {
				t.Errorf("并发 dispatch[%d] 失败: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	reqs := platform.received()
	if len(reqs) != n {
		t.Fatalf("并发 %d 条入站应产生 %d 条出站，实际 %d（闭环丢消息或竞态）", n, n, len(reqs))
	}
	// 每条出站都应路由到对应的 ChatID（C{idx}），且内容为 r-{idx}。
	seen := map[string]string{}
	for _, r := range reqs {
		seen[r.ChatID] = r.Text
	}
	for i := 0; i < n; i++ {
		chat := fmt.Sprintf("C%d", i)
		wantText := fmt.Sprintf("r-%d", i)
		if seen[chat] != wantText {
			t.Errorf("ChatID %s 的出站内容=%q，期望 %q（路由串扰）", chat, seen[chat], wantText)
		}
	}
}

// TestE2E_SendQueue_RateLimits 速率限制闭环：rate=2/s 时连续出站应被节流，
// 后续发送相对首次有可观测的延迟（只断言"被节流"，不依赖绝对墙钟时刻）。
func TestE2E_SendQueue_RateLimits(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	// 2/s → minInterval=500ms。发 3 条，第 2、3 条应被 SendQueue 串行节流。
	a := newMockIMAdapter(platform.URL(), "t", 2)
	defer a.stop()
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		return &Reply{Content: m.Content}, nil
	})

	start := time.Now()
	for i := 0; i < 3; i++ {
		raw := fmt.Sprintf(`{"type":"message","user":"U%d","channel":"C%d","text":"m%d"}`, i, i, i)
		if _, err := a.dispatch(context.Background(), []byte(raw)); err != nil {
			t.Fatalf("dispatch[%d] 失败: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	if platform.callCount() != 3 {
		t.Fatalf("应出站 3 条，实际 %d", platform.callCount())
	}
	// 3 条 @ 2/s：至少 2 个 minInterval(500ms) 的等待 ≈ ≥ 1s（容差留足，仅证"确被节流"）。
	if elapsed < 800*time.Millisecond {
		t.Errorf("3 条 @2/s 应被节流到 ≥ ~1s，实际仅 %v（速率限制未生效=flood control 闭环断裂）", elapsed)
	}
}

// ============================================================================
// 闭环 3：错误跨层传播（出站平台业务失败 → 回传到分发层）
// ============================================================================

// TestE2E_OutboundBusinessFailure_PropagatesToDispatcher 关键闭环断裂点：
// 平台返回 HTTP 200 但 body ok:false（如 channel_not_found）。
// 这种"业务失败"必须穿透 SendQueue 回传到分发层，而不是被当成功吞掉。
func TestE2E_OutboundBusinessFailure_PropagatesToDispatcher(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()
	platform.failTimes = 1 // 第 1 次（也是唯一一次）出站返回 ok:false
	platform.errCode = "channel_not_found"

	a := newMockIMAdapter(platform.URL(), "t", 100)
	defer a.stop()
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		return &Reply{Content: "hi"}, nil
	})

	raw := []byte(`{"type":"message","user":"U1","channel":"C-bad","text":"x"}`)
	dispatched, err := a.dispatch(context.Background(), raw)
	if !dispatched {
		t.Fatal("消息应被分发")
	}
	if err == nil {
		t.Fatal("平台 ok:false 时 dispatch 应返回 error（错误跨层传播），却返回 nil —— 失败被当成功，闭环断裂")
	}
	if !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("错误应包含平台错误码 channel_not_found，得到: %v", err)
	}
	if a.sendErrors.Load() != 1 {
		t.Errorf("出站失败计数应为 1，实际 %d", a.sendErrors.Load())
	}
}

// TestE2E_HandlerError_ClosesLoopWithErrorReply handler 报错时，闭环应回一条
// 错误提示给用户（出站到同一 ChatID），而不是静默丢弃。
func TestE2E_HandlerError_ClosesLoopWithErrorReply(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	a := newMockIMAdapter(platform.URL(), "t", 100)
	defer a.stop()
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		return nil, fmt.Errorf("engine 内部错误")
	})

	raw := []byte(`{"type":"message","user":"U1","channel":"C-err","text":"x"}`)
	_, err := a.dispatch(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "engine 内部错误") {
		t.Fatalf("handler 错误应回传，得到: %v", err)
	}

	reqs := platform.received()
	if len(reqs) != 1 {
		t.Fatalf("handler 出错应出站 1 条错误提示，实际 %d", len(reqs))
	}
	if reqs[0].ChatID != "C-err" {
		t.Errorf("错误提示应发回原 ChatID C-err，实际 %q", reqs[0].ChatID)
	}
	if !strings.Contains(reqs[0].Text, "出错") {
		t.Errorf("错误提示文案异常：%q", reqs[0].Text)
	}
}

// TestE2E_ExpiredInboundCtx_DoesNotPoisonErrorReply 历史 bug 回归（send_queue_test 记录）：
// 入站 ctx 若已过期，handler 出错后用【同一过期 ctx】发错误提示会静默失败。
// 本闭环验证：用独立 ctx 发错误提示，即使入站 ctx 已取消，错误提示仍能送达平台。
func TestE2E_ExpiredInboundCtx_DoesNotPoisonErrorReply(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	a := newMockIMAdapter(platform.URL(), "t", 100)
	defer a.stop()
	a.attach(func(ctx context.Context, m *Message) (*Reply, error) {
		return nil, fmt.Errorf("boom")
	})

	// 入站 ctx 立即取消，模拟 webhook 响应返回后 ctx cancel。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := a.dispatch(ctx, []byte(`{"type":"message","user":"U1","channel":"C9","text":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("handler 错误应回传，得到: %v", err)
	}
	// 错误提示用独立 ctx 发，应到达平台。
	reqs := platform.received()
	if len(reqs) != 1 {
		t.Fatalf("入站 ctx 已取消，错误提示仍应用独立 ctx 送达，实际出站 %d 条", len(reqs))
	}
	if reqs[0].ChatID != "C9" {
		t.Errorf("错误提示 ChatID=%q，期望 C9", reqs[0].ChatID)
	}
}

// ============================================================================
// 闭环 4：attachments / interactive 闭环
// ============================================================================

// TestE2E_Attachments_ValidImagePassesToHandler 图片附件入站 → 校验通过 → 透传到 handler。
func TestE2E_Attachments_ValidImagePassesToHandler(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	a := newMockIMAdapter(platform.URL(), "t", 100)
	defer a.stop()

	var gotAttachments int
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		gotAttachments = len(m.Attachments)
		return &Reply{Content: "收到图"}, nil
	})

	raw := []byte(`{"type":"message","user":"U1","channel":"C1","text":"看图",
		"attachments":[{"type":"image","name":"a.png","mime":"image/png","url":"https://x/a.png"}]}`)
	dispatched, err := a.dispatch(context.Background(), raw)
	if err != nil {
		t.Fatalf("合法图片附件不应报错: %v", err)
	}
	if !dispatched {
		t.Fatal("带图片附件的消息应被分发")
	}
	if gotAttachments != 1 {
		t.Errorf("附件应透传到 handler，handler 看到 %d 个", gotAttachments)
	}
	if len(platform.received()) != 1 {
		t.Errorf("应正常出站 1 条")
	}
}

// TestE2E_Attachments_NonImageRejectedAndClosesLoop 非图片附件 → 入站校验拒绝 →
// 闭环回一条拒绝提示（出站），handler 不被调用。
func TestE2E_Attachments_NonImageRejectedAndClosesLoop(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	a := newMockIMAdapter(platform.URL(), "t", 100)
	defer a.stop()

	handlerCalled := false
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		handlerCalled = true
		return &Reply{Content: "x"}, nil
	})

	raw := []byte(`{"type":"message","user":"U1","channel":"C1","text":"看文档",
		"attachments":[{"type":"file","name":"a.pdf","mime":"application/pdf","url":"https://x/a.pdf"}]}`)
	dispatched, err := a.dispatch(context.Background(), raw)
	if err == nil {
		t.Fatal("非图片附件应被入站校验拒绝")
	}
	if dispatched {
		t.Error("校验失败的消息不应分发到 handler")
	}
	if handlerCalled {
		t.Error("非图片附件 handler 不应被调用")
	}
	// 闭环：拒绝提示应出站到原 ChatID。
	reqs := platform.received()
	if len(reqs) != 1 {
		t.Fatalf("附件校验失败应回一条提示，实际出站 %d 条", len(reqs))
	}
	if !strings.Contains(reqs[0].Text, "仅支持") && !strings.Contains(reqs[0].Text, "校验失败") {
		t.Errorf("拒绝提示文案异常: %q", reqs[0].Text)
	}
}

// TestE2E_Attachments_OverLimitRejected 超过 MaxAttachments 的附件应在闭环入口被拒。
func TestE2E_Attachments_OverLimitRejected(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()
	a := newMockIMAdapter(platform.URL(), "t", 100)
	defer a.stop()
	a.attach(func(_ context.Context, m *Message) (*Reply, error) { return &Reply{Content: "x"}, nil })

	var atts []Attachment
	for i := 0; i <= MaxAttachments; i++ { // MaxAttachments+1 个
		atts = append(atts, Attachment{Type: "image", Mime: "image/png", URL: "https://x/p.png"})
	}
	ev := inboundEvent{Type: "message", User: "U1", Channel: "C1", Text: "多图", Attachments: atts}
	raw, _ := json.Marshal(ev)

	_, err := a.dispatch(context.Background(), raw)
	if err == nil {
		t.Fatalf("超过 %d 个附件应被拒绝", MaxAttachments)
	}
}

// TestE2E_Interactive_TextFallbackReachesPlatform interactive 闭环：
// handler 返回带 InteractivePayload 的 Reply（buttons），flag OFF 时
// 出站到平台的【文本内容】必须已追加 fallback（"1) 是  2) 否"），
// 否则不支持原生交互的平台上用户看不到选项 —— 闭环断裂。
func TestE2E_Interactive_TextFallbackReachesPlatform(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	a := newMockIMAdapter(platform.URL(), "t", 100)
	defer a.stop()
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		return &Reply{
			Content: "请确认",
			Interactive: &InteractivePayload{
				Type: InteractiveTypeButtons,
				Buttons: []InteractiveButton{
					{Label: "是", Action: "yes"},
					{Label: "否", Action: "no"},
				},
			},
		}, nil
	})

	raw := []byte(`{"type":"message","user":"U1","channel":"C-int","text":"删吗"}`)
	if _, err := a.dispatch(context.Background(), raw); err != nil {
		t.Fatalf("dispatch 失败: %v", err)
	}

	reqs := platform.received()
	if len(reqs) != 1 {
		t.Fatalf("应出站 1 条，实际 %d", len(reqs))
	}
	text := reqs[0].Text
	if !strings.Contains(text, "请确认") {
		t.Errorf("出站文本应保留原 Content，得到: %q", text)
	}
	if !strings.Contains(text, "是") || !strings.Contains(text, "否") {
		t.Errorf("interactive 闭环断裂：出站平台文本未包含按钮 label fallback，用户看不到选项: %q", text)
	}
}

// TestE2E_Interactive_ApprovalFallbackReachesPlatform approval 类型交互的闭环。
func TestE2E_Interactive_ApprovalFallbackReachesPlatform(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	a := newMockIMAdapter(platform.URL(), "t", 100)
	defer a.stop()
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		return &Reply{
			Content: "",
			Interactive: &InteractivePayload{
				Type: InteractiveTypeApproval,
				Approval: &InteractiveApproval{
					Subject:      "删除 12 道错题",
					ApproveLabel: "批准",
					RejectLabel:  "拒绝",
				},
			},
		}, nil
	})

	raw := []byte(`{"type":"message","user":"U1","channel":"C-appr","text":"删除错题"}`)
	if _, err := a.dispatch(context.Background(), raw); err != nil {
		t.Fatalf("dispatch 失败: %v", err)
	}
	reqs := platform.received()
	if len(reqs) != 1 {
		t.Fatalf("应出站 1 条审批提示，实际 %d", len(reqs))
	}
	if !strings.Contains(reqs[0].Text, "删除 12 道错题") {
		t.Errorf("审批主题应出现在出站文本: %q", reqs[0].Text)
	}
	if !strings.Contains(reqs[0].Text, "批准") || !strings.Contains(reqs[0].Text, "拒绝") {
		t.Errorf("审批闭环断裂：出站未含批准/拒绝选项: %q", reqs[0].Text)
	}
}

// ============================================================================
// 闭环 5：SendQueue 生命周期与闭环健壮性
// ============================================================================

// TestE2E_SendAfterStop_Rejected 适配器停止后，入站再来消息出站应被拒绝（不 panic、不泄漏）。
func TestE2E_SendAfterStop_Rejected(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()

	a := newMockIMAdapter(platform.URL(), "t", 100)
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		return &Reply{Content: "late"}, nil
	})
	a.stop() // 先停队列

	_, err := a.dispatch(context.Background(), []byte(`{"type":"message","user":"U1","channel":"C1","text":"x"}`))
	if err == nil {
		t.Fatal("队列已停止后出站应失败")
	}
	if !strings.Contains(err.Error(), "已停止") {
		t.Errorf("应返回队列已停止错误，得到: %v", err)
	}
	if platform.callCount() != 0 {
		t.Errorf("队列停止后不应有任何真实出站，实际 %d", platform.callCount())
	}
}

// TestE2E_EmptyChatID_StillRoutes 边界：空 ChatID 仍按原值路由（暴露上游是否漏填）。
// 真实适配器对空 channel 不拦截（见 slack TestProcessEvent 用例），出站会带空 channel
// 让平台拒绝 —— 闭环应把该平台错误回传，而不是本地静默丢弃。
func TestE2E_EmptyChatID_StillRoutes(t *testing.T) {
	platform := newMockIMPlatform()
	defer platform.Close()
	platform.failTimes = 1
	platform.errCode = "channel_not_found"

	a := newMockIMAdapter(platform.URL(), "t", 100)
	defer a.stop()
	a.attach(func(_ context.Context, m *Message) (*Reply, error) {
		return &Reply{Content: "hi"}, nil
	})

	raw := []byte(`{"type":"message","user":"U1","channel":"","text":"x"}`)
	_, err := a.dispatch(context.Background(), raw)
	if err == nil {
		t.Fatal("空 ChatID 出站被平台拒绝，错误应回传（而非本地吞掉）")
	}
	reqs := platform.received()
	if len(reqs) != 1 || reqs[0].ChatID != "" {
		t.Errorf("空 ChatID 应原样路由出站（暴露上游漏填），实际: %+v", reqs)
	}
}
