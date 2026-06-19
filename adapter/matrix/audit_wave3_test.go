package matrix

// audit_wave3_test.go 是对 Matrix 适配器的第三轮挑刺式全场景审计测试。
//
// 覆盖范围：
//   - New / 默认值 / Name 回退
//   - 出站 sendReplyNow：HTTP 请求构造（PUT / txnID / 鉴权头 / Content-Type）、
//     StripThinking 出口剥离、非 200 错误透传、nil reply 短路、body 编码
//   - Send 经由 SendQueue 串行化的端到端发送
//   - SendStream：降级聚合发送、chunk 携带 Error 时中断、空流
//   - 入站 handleEvent：消息类型过滤、自身消息过滤、空 body 过滤、
//     字段类型不匹配的健壮性、超长/Unicode/空白 body、时间戳转换、并发竞态
//   - doSync：URL since 拼接、非 200 透传、非法 JSON、空响应
//   - ValidateConfig：缺字段、whoami 成功 / 失败
//   - Health：未附加 handler / 缺配置 / 停止后
//   - Stop 幂等 / Stop 后 Send 行为
//
// 注意：Matrix 通过 Client-Server API 的长轮询 /sync 接收消息，不存在
// webhook / HMAC 签名 / 时间戳重放窗口逻辑，故此处不测签名校验（本包无此代码路径）。

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

	"github.com/hexagon-codes/hexclaw/adapter"
)

// newTestAdapter 创建一个把 HomeserverURL 指向 httptest.Server 的适配器，
// 复用其内部 *http.Client（同包可访问私有字段，但这里只改 config）。
func newTestAdapter(t *testing.T, srvURL, userID string) *MatrixAdapter {
	t.Helper()
	return New(Config{
		HomeserverURL: srvURL,
		AccessToken:   "tok-secret",
		UserID:        userID,
	})
}

// ---------- New / 默认值 ----------

func TestNew_DefaultsAndNameFallback(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		wantName string
		wantTO   int
	}{
		{"零值默认超时与名称", Config{}, "matrix", 30},
		{"显式 Name", Config{Name: "matrix-prod"}, "matrix-prod", 30},
		{"显式超时", Config{SyncTimeout: 7}, "matrix", 7},
		{"负数超时不被覆盖", Config{SyncTimeout: -1}, "matrix", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.cfg)
			if a.Name() != tt.wantName {
				t.Errorf("Name() = %q, 期望 %q", a.Name(), tt.wantName)
			}
			if a.config.SyncTimeout != tt.wantTO {
				t.Errorf("SyncTimeout = %d, 期望 %d", a.config.SyncTimeout, tt.wantTO)
			}
			if a.Platform() != PlatformMatrix {
				t.Errorf("Platform() = %q, 期望 %q", a.Platform(), PlatformMatrix)
			}
			if a.queue == nil {
				t.Error("queue 不应为 nil")
			}
		})
	}
}

// ---------- sendReplyNow：HTTP 请求构造与出口剥离 ----------

func TestSendReplyNow_RequestConstruction(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotCT     string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	// 出口必须剥离 <think>，验证 K12 防泄漏
	reply := &adapter.Reply{Content: "答案是 42 <think>这是推理过程，绝不能泄漏</think>"}
	if err := a.sendReplyNow(context.Background(), "!room:example.com", reply); err != nil {
		t.Fatalf("sendReplyNow 不应报错: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("HTTP 方法 = %q, 期望 PUT", gotMethod)
	}
	// 路径应包含 send/m.room.message/ 且带 txnID 前缀 hc_
	if !strings.Contains(gotPath, "/_matrix/client/v3/rooms/!room:example.com/send/m.room.message/hc_") {
		t.Errorf("路径不符合 Matrix send 规范: %q", gotPath)
	}
	if gotAuth != "Bearer tok-secret" {
		t.Errorf("Authorization = %q, 期望 Bearer tok-secret", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, 期望 application/json", gotCT)
	}

	var payload map[string]string
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v; raw=%s", err, gotBody)
	}
	if payload["msgtype"] != "m.text" {
		t.Errorf("msgtype = %q, 期望 m.text", payload["msgtype"])
	}
	if strings.Contains(payload["body"], "推理过程") || strings.Contains(payload["body"], "<think>") {
		t.Errorf("出口未剥离 thinking，发生泄漏: %q", payload["body"])
	}
	if !strings.Contains(payload["body"], "42") {
		t.Errorf("正文丢失: %q", payload["body"])
	}
}

func TestSendReplyNow_NilReplyShortCircuit(t *testing.T) {
	// nil reply 必须不发起任何 HTTP 请求，直接返回 nil
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	if err := a.sendReplyNow(context.Background(), "!room", nil); err != nil {
		t.Fatalf("nil reply 应返回 nil, 得到: %v", err)
	}
	if called.Load() {
		t.Error("nil reply 不应发起 HTTP 请求")
	}
}

func TestSendReplyNow_Non200Error(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
	}{
		{"403 禁止", http.StatusForbidden, `{"errcode":"M_FORBIDDEN"}`},
		{"429 限流", http.StatusTooManyRequests, `{"errcode":"M_LIMIT_EXCEEDED"}`},
		{"500 服务端", http.StatusInternalServerError, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			a := newTestAdapter(t, srv.URL, "@bot:example.com")
			err := a.sendReplyNow(context.Background(), "!room", &adapter.Reply{Content: "hi"})
			if err == nil {
				t.Fatalf("非 200 状态码 %d 应返回错误", tt.code)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", tt.code)) {
				t.Errorf("错误信息应含状态码 %d, 得到: %v", tt.code, err)
			}
		})
	}
}

func TestSendReplyNow_ConnectionError(t *testing.T) {
	// 指向一个立即关闭的服务器地址，触发传输层错误
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立即关闭

	a := newTestAdapter(t, url, "@bot:example.com")
	err := a.sendReplyNow(context.Background(), "!room", &adapter.Reply{Content: "hi"})
	if err == nil {
		t.Fatal("连接失败应返回错误")
	}
	if !strings.Contains(err.Error(), "发送消息失败") {
		t.Errorf("错误应被包装为 '发送消息失败', 得到: %v", err)
	}
}

// ---------- Send 经由 SendQueue ----------

func TestSend_ThroughQueue(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	defer func() { _ = a.Stop(context.Background()) }()

	if err := a.Send(context.Background(), "!room", &adapter.Reply{Content: "via queue"}); err != nil {
		t.Fatalf("Send 不应报错: %v", err)
	}
	if count.Load() != 1 {
		t.Errorf("期望 1 次 HTTP 调用, 得到 %d", count.Load())
	}
}

func TestSend_NilReplyViaQueue(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:0", "@bot:example.com")
	defer func() { _ = a.Stop(context.Background()) }()
	// 队列里 nil reply 直接返回 nil，不发送
	if err := a.Send(context.Background(), "!room", nil); err != nil {
		t.Errorf("nil reply 应返回 nil, 得到: %v", err)
	}
}

func TestSend_AfterStopRejected(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:0", "@bot:example.com")
	_ = a.Stop(context.Background())
	// 队列停止后 Send 应被拒绝
	err := a.Send(context.Background(), "!room", &adapter.Reply{Content: "after stop"})
	if err == nil {
		t.Fatal("Stop 之后 Send 应被拒绝")
	}
}

// ---------- SendStream：降级与中断 ----------

func TestSendStream_AggregatesAndSends(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p map[string]string
		_ = json.Unmarshal(b, &p)
		gotBody = p["body"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	defer func() { _ = a.Stop(context.Background()) }()

	chunks := make(chan *adapter.ReplyChunk, 3)
	chunks <- &adapter.ReplyChunk{Content: "Hello "}
	chunks <- &adapter.ReplyChunk{Content: "World"}
	chunks <- &adapter.ReplyChunk{Content: "!"}
	close(chunks)

	if err := a.SendStream(context.Background(), "!room", chunks); err != nil {
		t.Fatalf("SendStream 不应报错: %v", err)
	}
	if gotBody != "Hello World!" {
		t.Errorf("聚合内容 = %q, 期望 'Hello World!'", gotBody)
	}
}

func TestSendStream_ChunkErrorInterrupts(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	defer func() { _ = a.Stop(context.Background()) }()

	wantErr := fmt.Errorf("上游断流")
	chunks := make(chan *adapter.ReplyChunk, 2)
	chunks <- &adapter.ReplyChunk{Content: "部分内容"}
	chunks <- &adapter.ReplyChunk{Error: wantErr}
	close(chunks)

	err := a.SendStream(context.Background(), "!room", chunks)
	if err == nil {
		t.Fatal("chunk 携带 Error 时 SendStream 应返回该错误")
	}
	if err != wantErr {
		t.Errorf("应原样返回 chunk.Error, 得到: %v", err)
	}
	// 实际行为记录：chunk 出错时不应再发送已积累的部分内容
	if called.Load() {
		t.Error("chunk 出错后不应仍发起发送")
	}
}

func TestSendStream_EmptyStream(t *testing.T) {
	var gotBody string
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		b, _ := io.ReadAll(r.Body)
		var p map[string]string
		_ = json.Unmarshal(b, &p)
		gotBody = p["body"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	defer func() { _ = a.Stop(context.Background()) }()

	chunks := make(chan *adapter.ReplyChunk)
	close(chunks) // 空流

	if err := a.SendStream(context.Background(), "!room", chunks); err != nil {
		t.Fatalf("空流 SendStream 不应报错: %v", err)
	}
	// 记录实际行为：空流仍发起一次空 body 发送
	if called.Load() && gotBody != "" {
		t.Errorf("空流应发送空 body, 得到: %q", gotBody)
	}
}

// ---------- handleEvent：入站过滤与转换 ----------

// captureHandler 创建一个捕获消息的 handler，并返回信号 channel。
func captureHandler() (adapter.MessageHandler, <-chan *adapter.Message) {
	ch := make(chan *adapter.Message, 8)
	h := func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		ch <- msg
		return nil, nil
	}
	return h, ch
}

func TestHandleEvent_Filtering(t *testing.T) {
	tests := []struct {
		name      string
		event     matrixEvent
		wantPass  bool // 是否应进入 handler
		checkMsg  func(t *testing.T, m *adapter.Message)
	}{
		{
			name: "正常文本消息透传",
			event: matrixEvent{
				Type: "m.room.message", EventID: "$evt1", Sender: "@alice:example.com",
				OriginServerTS: 1700000000000,
				Content:        map[string]any{"msgtype": "m.text", "body": "你好世界"},
			},
			wantPass: true,
			checkMsg: func(t *testing.T, m *adapter.Message) {
				if m.ID != "$evt1" || m.UserID != "@alice:example.com" || m.Content != "你好世界" {
					t.Errorf("消息字段映射错误: %+v", m)
				}
				if m.Platform != PlatformMatrix {
					t.Errorf("Platform 错误: %v", m.Platform)
				}
				if m.ChatID != "!room:example.com" {
					t.Errorf("ChatID 错误: %v", m.ChatID)
				}
				if m.Timestamp.UnixMilli() != 1700000000000 {
					t.Errorf("时间戳转换错误: %v", m.Timestamp.UnixMilli())
				}
			},
		},
		{
			name: "忽略自己发的消息",
			event: matrixEvent{
				Type: "m.room.message", Sender: "@bot:example.com",
				Content: map[string]any{"msgtype": "m.text", "body": "self"},
			},
			wantPass: false,
		},
		{
			name: "忽略非 m.room.message 类型",
			event: matrixEvent{
				Type: "m.room.member", Sender: "@alice:example.com",
				Content: map[string]any{"msgtype": "m.text", "body": "join"},
			},
			wantPass: false,
		},
		{
			name: "忽略非 m.text 子类型（图片）",
			event: matrixEvent{
				Type: "m.room.message", Sender: "@alice:example.com",
				Content: map[string]any{"msgtype": "m.image", "url": "mxc://x"},
			},
			wantPass: false,
		},
		{
			name: "忽略空 body",
			event: matrixEvent{
				Type: "m.room.message", Sender: "@alice:example.com",
				Content: map[string]any{"msgtype": "m.text", "body": ""},
			},
			wantPass: false,
		},
		{
			// 回归: W3-17 —— 纯空白/换行/制表符的 body 等价于空消息，
			// 不应触发 handler（避免空白消息浪费一次 LLM 调用）。
			name: "忽略纯空白/换行 body",
			event: matrixEvent{
				Type: "m.room.message", Sender: "@alice:example.com",
				Content: map[string]any{"msgtype": "m.text", "body": "  \n\t \r\n  "},
			},
			wantPass: false,
		},
		{
			name: "msgtype 字段类型不匹配（非字符串）",
			event: matrixEvent{
				Type: "m.room.message", Sender: "@alice:example.com",
				Content: map[string]any{"msgtype": 123, "body": "x"},
			},
			wantPass: false,
		},
		{
			name: "body 字段类型不匹配（非字符串）",
			event: matrixEvent{
				Type: "m.room.message", Sender: "@alice:example.com",
				Content: map[string]any{"msgtype": "m.text", "body": 999},
			},
			wantPass: false,
		},
		{
			name: "content 为 nil map",
			event: matrixEvent{
				Type: "m.room.message", Sender: "@alice:example.com",
				Content: nil,
			},
			wantPass: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(Config{UserID: "@bot:example.com"})
			h, ch := captureHandler()
			a.handler = h

			a.handleEvent(context.Background(), "!room:example.com", tt.event)

			select {
			case m := <-ch:
				if !tt.wantPass {
					t.Fatalf("不应进入 handler 但收到了消息: %+v", m)
				}
				if tt.checkMsg != nil {
					tt.checkMsg(t, m)
				}
			case <-time.After(300 * time.Millisecond):
				if tt.wantPass {
					t.Fatal("期望进入 handler 但超时未收到消息")
				}
			}
		})
	}
}

func TestHandleEvent_NilHandlerNoPanic(t *testing.T) {
	// handler 未附加时，goroutine 内应安全早退不 panic
	a := New(Config{UserID: "@bot:example.com"})
	event := matrixEvent{
		Type: "m.room.message", Sender: "@alice:example.com",
		Content: map[string]any{"msgtype": "m.text", "body": "no handler"},
	}
	a.handleEvent(context.Background(), "!room", event)
	// 给 goroutine 一点时间执行
	time.Sleep(100 * time.Millisecond)
}

func TestHandleEvent_LongAndUnicodeBody(t *testing.T) {
	a := New(Config{UserID: "@bot:example.com"})
	h, ch := captureHandler()
	a.handler = h

	longBody := strings.Repeat("超长消息内容🚀", 5000) // 含 emoji 的超长 Unicode
	event := matrixEvent{
		Type: "m.room.message", Sender: "@alice:example.com",
		Content: map[string]any{"msgtype": "m.text", "body": longBody},
	}
	a.handleEvent(context.Background(), "!room", event)

	select {
	case m := <-ch:
		if m.Content != longBody {
			t.Errorf("超长 Unicode body 应原样透传，长度 got=%d want=%d", len(m.Content), len(longBody))
		}
	case <-time.After(time.Second):
		t.Fatal("超时未收到消息")
	}
}

// TestHandleEvent_DetachIgnoresParentCancel 验证 trace.Detach 的关键语义：
// 即使 parent ctx 已被取消，handleEvent 内的异步处理仍应执行（脱离父请求生命周期）。
// 这是 H7 设计意图，但 H7 测试只校验了 Value 保留，未校验 cancel 脱离，这里补齐。
func TestHandleEvent_DetachIgnoresParentCancel(t *testing.T) {
	a := New(Config{UserID: "@bot:example.com"})
	got := make(chan struct{}, 1)
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		// handler 收到的 ctx 不应是已取消状态
		if ctx.Err() != nil {
			t.Errorf("handler ctx 不应继承父 cancel, err=%v", ctx.Err())
		}
		got <- struct{}{}
		return nil, nil
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消父 ctx

	event := matrixEvent{
		Type: "m.room.message", Sender: "@alice:example.com",
		Content: map[string]any{"msgtype": "m.text", "body": "even if parent cancelled"},
	}
	a.handleEvent(parent, "!room", event)

	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("父 ctx 取消后 handler 应仍被调用（Detach 语义），但超时")
	}
}

// TestHandleEvent_ConcurrentNoRace 并发投递多条事件，配合 -race 检测竞态。
func TestHandleEvent_ConcurrentNoRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	defer func() { _ = a.Stop(context.Background()) }()

	var processed atomic.Int32
	var wg sync.WaitGroup
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		processed.Add(1)
		wg.Done()
		return &adapter.Reply{Content: "echo: " + msg.Content}, nil
	}

	const n = 50
	wg.Add(n)
	for i := 0; i < n; i++ {
		event := matrixEvent{
			Type: "m.room.message", EventID: fmt.Sprintf("$e%d", i),
			Sender:  "@alice:example.com",
			Content: map[string]any{"msgtype": "m.text", "body": fmt.Sprintf("msg-%d", i)},
		}
		go a.handleEvent(context.Background(), "!room", event)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("并发处理超时, 已处理 %d/%d", processed.Load(), n)
	}
	if processed.Load() != n {
		t.Errorf("处理数 = %d, 期望 %d", processed.Load(), n)
	}
}

// ---------- doSync ----------

func TestDoSync_URLAndSinceToken(t *testing.T) {
	var gotURLs []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotURLs = append(gotURLs, r.URL.String())
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok-secret" {
			t.Errorf("sync 缺少鉴权头: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"next_batch":"s2_99","rooms":{"join":{}}}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	a.config.SyncTimeout = 30

	// 第一次同步：无 since
	if err := a.doSync(context.Background()); err != nil {
		t.Fatalf("首次 doSync 报错: %v", err)
	}
	if a.nextBatch != "s2_99" {
		t.Errorf("nextBatch 未更新, got=%q", a.nextBatch)
	}
	// 第二次同步：应带上 since=s2_99
	if err := a.doSync(context.Background()); err != nil {
		t.Fatalf("二次 doSync 报错: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotURLs) != 2 {
		t.Fatalf("期望 2 次请求, 得到 %d", len(gotURLs))
	}
	if !strings.Contains(gotURLs[0], "timeout=30000") {
		t.Errorf("首次 URL 应含 timeout=30000(ms), got=%q", gotURLs[0])
	}
	if strings.Contains(gotURLs[0], "since=") {
		t.Errorf("首次同步不应带 since, got=%q", gotURLs[0])
	}
	if !strings.Contains(gotURLs[1], "since=s2_99") {
		t.Errorf("二次同步应带 since=s2_99, got=%q", gotURLs[1])
	}
}

func TestDoSync_Non200Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN"}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	err := a.doSync(context.Background())
	if err == nil {
		t.Fatal("401 同步应返回错误")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误应含 401, 得到: %v", err)
	}
}

func TestDoSync_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{ not valid json `))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	err := a.doSync(context.Background())
	if err == nil {
		t.Fatal("非法 JSON 应返回解析错误")
	}
	if !strings.Contains(err.Error(), "解析同步响应失败") {
		t.Errorf("错误应被包装为解析失败, 得到: %v", err)
	}
}

func TestDoSync_DispatchesRoomMessages(t *testing.T) {
	// 验证 doSync 把 join room timeline 事件分发到 handler
	resp := matrixSyncResponse{
		NextBatch: "s_next",
		Rooms: matrixRooms{
			Join: map[string]matrixJoinedRoom{
				"!room:example.com": {
					Timeline: matrixTimeline{
						Events: []matrixEvent{
							{Type: "m.room.message", EventID: "$a", Sender: "@u:example.com",
								Content: map[string]any{"msgtype": "m.text", "body": "from sync"}},
						},
					},
				},
			},
		},
	}
	payload, _ := json.Marshal(resp)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	h, ch := captureHandler()
	a.handler = h

	if err := a.doSync(context.Background()); err != nil {
		t.Fatalf("doSync 报错: %v", err)
	}
	select {
	case m := <-ch:
		if m.Content != "from sync" || m.ChatID != "!room:example.com" {
			t.Errorf("分发的消息字段错误: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("doSync 未把 room 消息分发到 handler")
	}
}

func TestDoSync_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	a.nextBatch = "prev"
	if err := a.doSync(context.Background()); err != nil {
		t.Fatalf("空响应不应报错: %v", err)
	}
	// 回归: W3-15 —— 空响应（无 next_batch）不得把已有增量游标重置为空串，
	// 否则下次 sync 将丢失游标、退回全量重新拉取整个会话历史。
	// 正确行为：仅在响应携带有效 next_batch 时才更新游标，否则保留旧游标。
	if a.nextBatch != "prev" {
		t.Errorf("W3-15: 空 next_batch 不应覆盖已有游标; got=%q want=%q", a.nextBatch, "prev")
	}
}

// ---------- ValidateConfig ----------

func TestValidateConfig_MissingFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantSub string
	}{
		{"缺 homeserver", Config{AccessToken: "t", UserID: "@u:x"}, "homeserver_url 未配置"},
		{"缺 access_token", Config{HomeserverURL: "http://x", UserID: "@u:x"}, "access_token 未配置"},
		{"缺 user_id", Config{HomeserverURL: "http://x", AccessToken: "t"}, "user_id 未配置"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.cfg)
			err := a.ValidateConfig(context.Background())
			if err == nil {
				t.Fatal("缺字段应返回错误")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("错误应含 %q, 得到: %v", tt.wantSub, err)
			}
		})
	}
}

func TestValidateConfig_WhoamiSuccess(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"@bot:example.com"}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	if err := a.ValidateConfig(context.Background()); err != nil {
		t.Fatalf("whoami 成功不应报错: %v", err)
	}
	if gotPath != "/_matrix/client/v3/account/whoami" {
		t.Errorf("whoami 路径错误: %q", gotPath)
	}
	if gotAuth != "Bearer tok-secret" {
		t.Errorf("whoami 缺鉴权头: %q", gotAuth)
	}
}

func TestValidateConfig_WhoamiFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN"}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	err := a.ValidateConfig(context.Background())
	if err == nil {
		t.Fatal("whoami 401 应返回错误")
	}
	if !strings.Contains(err.Error(), "凭证验证失败") {
		t.Errorf("错误应含 '凭证验证失败', 得到: %v", err)
	}
}

// ---------- Health ----------

func TestHealth_StateMatrix(t *testing.T) {
	full := Config{HomeserverURL: "http://x", AccessToken: "t", UserID: "@u:x"}

	t.Run("未附加 handler", func(t *testing.T) {
		a := New(full)
		if err := a.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "handler 未附加") {
			t.Errorf("应报 handler 未附加, 得到: %v", err)
		}
	})

	t.Run("缺配置", func(t *testing.T) {
		a := New(Config{HomeserverURL: "http://x"})
		a.handler = func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		if err := a.Health(context.Background()); err == nil {
			t.Error("缺 access_token/user_id 应报错")
		}
	})

	t.Run("健康", func(t *testing.T) {
		a := New(full)
		a.handler = func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		if err := a.Health(context.Background()); err != nil {
			t.Errorf("配置齐全且未停止应健康, 得到: %v", err)
		}
	})

	t.Run("停止后", func(t *testing.T) {
		a := New(full)
		a.handler = func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) { return nil, nil }
		_ = a.Stop(context.Background())
		if err := a.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Errorf("停止后应报 stopped, 得到: %v", err)
		}
	})
}

// ---------- Start / Stop 生命周期 ----------

func TestStartStop_Lifecycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// sync 总返回空，避免无限快速循环空转过快可接受
		_, _ = w.Write([]byte(`{"next_batch":"s1","rooms":{"join":{}}}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "@bot:example.com")
	a.config.SyncTimeout = 1
	h := func(ctx context.Context, m *adapter.Message) (*adapter.Reply, error) { return nil, nil }

	if err := a.Start(context.Background(), h); err != nil {
		t.Fatalf("Start 报错: %v", err)
	}
	// 让 sync loop 跑几圈
	time.Sleep(150 * time.Millisecond)
	if a.handler == nil {
		t.Error("Start 后 handler 应被设置")
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 报错: %v", err)
	}
	// Stop 后 stopped 标志应为 true
	if !a.stopped.Load() {
		t.Error("Stop 后 stopped 应为 true")
	}
}

// TestStop_TripleIdempotent 多次 Stop 幂等（在已有 double-stop 测试基础上加码）。
func TestStop_TripleIdempotent(t *testing.T) {
	a := New(Config{HomeserverURL: "http://x", AccessToken: "t", UserID: "@u:x"})
	for i := 0; i < 3; i++ {
		if err := a.Stop(context.Background()); err != nil {
			t.Fatalf("第 %d 次 Stop 不应报错: %v", i+1, err)
		}
	}
}

// TestStop_ConcurrentIdempotent 并发 Stop 不应 panic（配合 -race）。
func TestStop_ConcurrentIdempotent(t *testing.T) {
	a := New(Config{HomeserverURL: "http://x", AccessToken: "t", UserID: "@u:x"})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Stop(context.Background())
		}()
	}
	wg.Wait()
}
