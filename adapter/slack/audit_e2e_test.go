package slack

// audit_e2e_test.go —— Slack 适配器【真实端到端闭环】集成测试
//
// 与 adapter/audit_e2e_test.go（进程内合成适配器）互补：这里驱动【真实】Slack
// 适配器的完整生产路径：
//
//   签名 webhook 入站 → handleEvents → go processEvent（trace.Detach 派生 ctx）
//   → 真实 handler 分发 → Send → SendQueue → postMessage（真实 HTTP）
//   → mock Slack Web API（httptest，指向 slackAPIBase）
//
// 这条链路覆盖单测漏掉的跨层集成点：
//   - webhook 立即返回 200，但出站发生在【后台 detached goroutine】里（异步闭环）
//   - 出站 HTTP 走真实 postMessage + ok 解析（业务失败 ok:false 的跨层传播）
//   - thread_ts 元数据从入站透传到出站路由（postThreadMessage）
//
// 确定性保障：slackAPIBase 指向 httptest；不打真实网络；用 channel + 超时收敛
// 异步 goroutine，不 sleep 等墙钟。

import (
	"context"
	"encoding/json"
	"io"
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

// slackOutbound 记录 mock Slack Web API 收到的一次出站。
type slackOutbound struct {
	Method   string
	Path     string
	Channel  string
	Text     string
	ThreadTS string
}

// mockSlackWebAPI 模拟 Slack Web API（chat.postMessage / chat.update），
// 把请求记录下来并按配置返回 ok:true/false。
type mockSlackWebAPI struct {
	server *httptest.Server

	mu        sync.Mutex
	outbounds []slackOutbound
	gotCh     chan slackOutbound // 每收到一次出站就投递一个，供异步闭环收敛

	okFalse bool   // true → 返回业务失败
	errCode string
}

func newMockSlackWebAPI() *mockSlackWebAPI {
	m := &mockSlackWebAPI{gotCh: make(chan slackOutbound, 16), errCode: "channel_not_found"}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

func (m *mockSlackWebAPI) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var p struct {
		Channel  string `json:"channel"`
		Text     string `json:"text"`
		ThreadTS string `json:"thread_ts"`
	}
	_ = json.Unmarshal(body, &p)
	ob := slackOutbound{Method: r.Method, Path: r.URL.Path, Channel: p.Channel, Text: p.Text, ThreadTS: p.ThreadTS}

	// drain sentinel：始终回 ok:true，不计入业务出站、不投递 gotCh。
	if p.Channel == "__drain_sentinel__" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"0"}`))
		return
	}

	m.mu.Lock()
	m.outbounds = append(m.outbounds, ob)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if m.okFalse {
		_, _ = w.Write([]byte(`{"ok":false,"error":"` + m.errCode + `"}`))
	} else {
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1700.123"}`))
	}
	// 通知异步等待方（放在写响应之后，确保 postMessage 已完成一轮）。
	select {
	case m.gotCh <- ob:
	default:
	}
}

func (m *mockSlackWebAPI) install(t *testing.T) {
	t.Helper()
	orig := slackAPIBase
	slackAPIBase = m.server.URL
	t.Cleanup(func() {
		slackAPIBase = orig
		m.server.Close()
	})
}

// waitOutbound 等待一次出站到达（异步闭环收敛），超时则 fail。
func (m *mockSlackWebAPI) waitOutbound(t *testing.T, d time.Duration) slackOutbound {
	t.Helper()
	select {
	case ob := <-m.gotCh:
		return ob
	case <-time.After(d):
		t.Fatal("超时：异步出站未到达 mock Slack API（闭环可能在后台 goroutine 断裂）")
		return slackOutbound{}
	}
}

// drain 确定性等待 detached processEvent goroutine 把出站发完后再返回。
//
// 机制：waitOutbound 仅证明 mock 的 HTTP handler 跑过（请求已到达），但此刻
// processEvent 内的 a.Send（经 SendQueue）可能尚未返回。直接 Stop 会让 Send 的
// select 命中 <-q.stopCh 而误报 "send queue 已停止"（虽然 task 实际已成功执行）。
//
// 为彻底消除该 teardown 竞态：往同一个 SendQueue 再压入一个 sentinel send 并等待
// 其完成。SendQueue 串行执行，sentinel 返回即证明此前的真实出站 send 已完全返回。
// sentinel 也会打到 mock（计入 outbound），调用方据此扣除即可。
func drainViaSentinel(t *testing.T, a *SlackAdapter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 经适配器自身的 SendQueue 压入 sentinel（与真实出站同一条队列 → 串行保序）。
	_ = a.Send(ctx, "__drain_sentinel__", &adapter.Reply{Content: "__drain__"})
}

// signReq 给请求加上有效的 Slack v0 签名。
func signReq(r *http.Request, secret string, body []byte) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", signV0(secret, ts, body))
}

// ============================================================================
// 真实闭环 1：签名 webhook → 异步 processEvent → 真实出站 HTTP
// ============================================================================

// TestE2E_Slack_SignedWebhookToOutbound 完整真实闭环：
// 一条签名 event_callback 经 handleEvents → 后台 processEvent → 真实 postMessage，
// 平台必须收到内容正确、ChatID 正确的出站。
func TestE2E_Slack_SignedWebhookToOutbound(t *testing.T) {
	api := newMockSlackWebAPI()
	api.install(t)

	secret := "e2e-secret"
	a := New(config.SlackConfig{Token: "xoxb-e2e", SigningSecret: secret})
	defer func() { _ = a.Stop(context.Background()) }()

	// 真实 handler：回声。直接注入（避免 Attach→fetchBotID 的额外网络），
	// 但 handleEvents / processEvent / Send / SendQueue / postMessage 全是真实路径。
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "回声: " + m.Content}, nil
	}

	body := []byte(`{"type":"event_callback","event":{"type":"message","user":"U1","channel":"C-real","text":"在吗"}}`)
	r := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	signReq(r, secret, body)
	w := httptest.NewRecorder()

	a.handleEvents(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook 应立即返回 200，得到 %d", w.Code)
	}

	ob := api.waitOutbound(t, 3*time.Second)
	drainViaSentinel(t, a)
	if ob.Path != "/chat.postMessage" {
		t.Errorf("出站应打到 chat.postMessage，实际 %s", ob.Path)
	}
	if ob.Channel != "C-real" {
		t.Errorf("ChatID 路由错误：出站 channel=%q，期望 C-real", ob.Channel)
	}
	if ob.Text != "回声: 在吗" {
		t.Errorf("出站内容错误：%q", ob.Text)
	}
}

// TestE2E_Slack_ThreadTSRoutesToThread 入站含 thread_ts → 出站应回到线程
// （postThreadMessage 带 thread_ts），验证元数据跨层透传到出站路由。
func TestE2E_Slack_ThreadTSRoutesToThread(t *testing.T) {
	api := newMockSlackWebAPI()
	api.install(t)

	secret := "thr-secret"
	a := New(config.SlackConfig{Token: "t", SigningSecret: secret})
	defer func() { _ = a.Stop(context.Background()) }()
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "线程回复"}, nil
	}

	body := []byte(`{"type":"event_callback","event":{"type":"message","user":"U1","channel":"C-thr","text":"hi","thread_ts":"1699.7"}}`)
	r := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	signReq(r, secret, body)
	w := httptest.NewRecorder()
	a.handleEvents(w, r)

	ob := api.waitOutbound(t, 3*time.Second)
	drainViaSentinel(t, a)
	if ob.ThreadTS != "1699.7" {
		t.Errorf("thread_ts 未透传到出站：出站 thread_ts=%q，期望 1699.7（闭环元数据丢失）", ob.ThreadTS)
	}
	if ob.Channel != "C-thr" {
		t.Errorf("线程回复 ChatID=%q，期望 C-thr", ob.Channel)
	}
}

// ============================================================================
// 真实闭环 2：出站业务失败（ok:false）的跨层传播
//
// 注意：失败发生在后台 detached goroutine，分发层无法直接拿到 error；
// 这里验证"出站确实发生了且只发生一次"，以及失败不会触发无限重试 / panic。
// （真实代码 processEvent 仅 logger.Error 记录出站失败，不回传给 webhook。）
// ============================================================================

// TestE2E_Slack_OutboundOKFalse_LoggedNotRetriedForever 出站 ok:false 时，
// 真实路径只发一次、记日志、不无限重试、不 panic（后台 goroutine 健壮性）。
func TestE2E_Slack_OutboundOKFalse_LoggedNotRetriedForever(t *testing.T) {
	api := newMockSlackWebAPI()
	api.okFalse = true
	api.errCode = "channel_not_found"
	api.install(t)

	secret := "fail-secret"
	a := New(config.SlackConfig{Token: "t", SigningSecret: secret})
	defer func() { _ = a.Stop(context.Background()) }()
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "会失败"}, nil
	}

	body := []byte(`{"type":"event_callback","event":{"type":"message","user":"U1","channel":"C-bad","text":"x"}}`)
	r := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	signReq(r, secret, body)
	w := httptest.NewRecorder()
	a.handleEvents(w, r)

	_ = api.waitOutbound(t, 3*time.Second) // 至少发生一次出站
	drainViaSentinel(t, a)                 // 阻塞至 in-flight 出站完成，保证计数稳定

	api.mu.Lock()
	count := len(api.outbounds)
	api.mu.Unlock()
	if count != 1 {
		t.Errorf("出站 ok:false 不应触发重试风暴，实际出站 %d 次", count)
	}
}

// ============================================================================
// 真实闭环 3：handler 出错 → 错误提示出站（用独立 ctx）
// ============================================================================

// TestE2E_Slack_HandlerError_SendsErrorReply handler 返回 error 时，真实
// processEvent 应向原 channel 发一条"处理消息时出错"提示（独立 ctx），闭环不静默。
func TestE2E_Slack_HandlerError_SendsErrorReply(t *testing.T) {
	api := newMockSlackWebAPI()
	api.install(t)

	secret := "err-secret"
	a := New(config.SlackConfig{Token: "t", SigningSecret: secret})
	defer func() { _ = a.Stop(context.Background()) }()
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		return nil, context.DeadlineExceeded // 模拟 engine 超时
	}

	body := []byte(`{"type":"event_callback","event":{"type":"message","user":"U1","channel":"C-e","text":"x"}}`)
	r := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	signReq(r, secret, body)
	w := httptest.NewRecorder()
	a.handleEvents(w, r)

	ob := api.waitOutbound(t, 3*time.Second)
	drainViaSentinel(t, a)
	if ob.Channel != "C-e" {
		t.Errorf("错误提示应发回原 channel C-e，实际 %q", ob.Channel)
	}
	if !strings.Contains(ob.Text, "出错") {
		t.Errorf("错误提示文案异常: %q", ob.Text)
	}
}

// ============================================================================
// 真实闭环 4：webhook 入站门禁（签名 / url_verification）与出站隔离
// ============================================================================

// TestE2E_Slack_InvalidSignature_NoDispatchNoOutbound 签名错误的 webhook 应被拒，
// handler 不被调用，平台无任何出站（入站门禁闭环）。
func TestE2E_Slack_InvalidSignature_NoDispatchNoOutbound(t *testing.T) {
	api := newMockSlackWebAPI()
	api.install(t)

	a := New(config.SlackConfig{Token: "t", SigningSecret: "real-secret"})
	defer func() { _ = a.Stop(context.Background()) }()
	var handlerCalled int32
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		atomicAdd(&handlerCalled)
		return &adapter.Reply{Content: "x"}, nil
	}

	body := []byte(`{"type":"event_callback","event":{"type":"message","user":"U1","channel":"C1","text":"x"}}`)
	r := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	// 用错误 secret 签名。
	signReq(r, "attacker-secret", body)
	w := httptest.NewRecorder()
	a.handleEvents(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("签名错误应 401，得到 %d", w.Code)
	}
	// 给后台一点时间，确认没有出站。
	time.Sleep(100 * time.Millisecond)
	api.mu.Lock()
	count := len(api.outbounds)
	api.mu.Unlock()
	if count != 0 {
		t.Errorf("签名错误不应触发任何出站，实际 %d", count)
	}
	if loadInt32(&handlerCalled) != 0 {
		t.Errorf("签名错误 handler 不应被调用")
	}
}

// TestE2E_Slack_URLVerification_NoOutbound url_verification 仅回显 challenge，无出站。
func TestE2E_Slack_URLVerification_NoOutbound(t *testing.T) {
	api := newMockSlackWebAPI()
	api.install(t)

	a := New(config.SlackConfig{Token: "t"})
	defer func() { _ = a.Stop(context.Background()) }()
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "x"}, nil
	}

	body, _ := json.Marshal(map[string]string{"type": "url_verification", "challenge": "abc123"})
	r := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	a.handleEvents(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("url_verification 应 200，得到 %d", w.Code)
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["challenge"] != "abc123" {
		t.Errorf("challenge 回显错误: %q", resp["challenge"])
	}
	time.Sleep(50 * time.Millisecond)
	api.mu.Lock()
	count := len(api.outbounds)
	api.mu.Unlock()
	if count != 0 {
		t.Errorf("url_verification 不应触发出站，实际 %d", count)
	}
}

// ── 小工具：原子计数（避免引入 sync/atomic 的额外 import 噪音，封装在此）──

var counterMu sync.Mutex

func atomicAdd(p *int32) {
	counterMu.Lock()
	*p++
	counterMu.Unlock()
}
func loadInt32(p *int32) int32 {
	counterMu.Lock()
	defer counterMu.Unlock()
	return *p
}
