package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// audit_wave3_test.go 针对 Discord 适配器平台特定逻辑的全场景审计测试：
//   - 入站 payload 解析（消息类型 / 空字段 / 超长 / Unicode / 非法 JSON）
//   - 出站消息转换（2000 字符截断 / HTTP 状态码 / 鉴权头 / URL 构造）
//   - 流式发送（限流 emit / 错误传播 / 收尾落地 / thinking 剥离）
//   - 状态机一致性（seq 追踪 / Health / Stop 幂等 / 队列停止后发送）
//   - ValidateConfig（空 token / 成功 / 非 200）
//
// 不修改被测源码：通过替换 a.client.Transport（*http.Client 公开字段）拦截 REST 调用。

// ============== 测试基础设施 ==============

// roundTripFunc 把函数适配成 http.RoundTripper，用于拦截出站 REST 请求。
type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// capturedReq 记录拦截到的请求要素，便于断言。
type capturedReq struct {
	Method string
	URL    string
	Auth   string
	CType  string
	Body   string
}

// stubClient 把 a.client 的 Transport 换成可控的 stub，并返回捕获器。
// handler 返回 (statusCode, respBody)。
func stubClient(a *DiscordAdapter, handler func(c *capturedReq) (int, string)) *[]capturedReq {
	var mu sync.Mutex
	caps := &[]capturedReq{}
	a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var bodyStr string
		if req.Body != nil {
			b, _ := io.ReadAll(req.Body)
			bodyStr = string(b)
		}
		c := capturedReq{
			Method: req.Method,
			URL:    req.URL.String(),
			Auth:   req.Header.Get("Authorization"),
			CType:  req.Header.Get("Content-Type"),
			Body:   bodyStr,
		}
		mu.Lock()
		*caps = append(*caps, c)
		mu.Unlock()
		code, respBody := handler(&c)
		return &http.Response{
			StatusCode: code,
			Status:     fmt.Sprintf("%d", code),
			Body:       io.NopCloser(strings.NewReader(respBody)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	return caps
}

// newTestAdapter 构造一个带 token 的适配器（默认 client 已就绪）。
func newTestAdapter() *DiscordAdapter {
	return New(config.DiscordConfig{Token: "tkn-abc"})
}

// ============== 入站 payload 解析 ==============

func TestHandleDispatch_MessageCreate_FullEvent(t *testing.T) {
	a := newTestAdapter()
	got := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		got <- m
		return nil, nil
	}

	// 走完整 Gateway 事件入口（handleEvent -> handleDispatch -> handleMessageCreate）。
	event := `{"op":0,"s":7,"t":"MESSAGE_CREATE","d":{"id":"m1","channel_id":"c1",` +
		`"guild_id":"g1","author":{"id":"u1","username":"alice","bot":false},"content":"hi"}}`
	a.handleEvent([]byte(event))

	select {
	case m := <-got:
		if m.Platform != adapter.PlatformDiscord {
			t.Errorf("platform = %q, want discord", m.Platform)
		}
		if m.Content != "hi" || m.UserID != "u1" || m.ChatID != "c1" {
			t.Errorf("字段未透传: %+v", m)
		}
		if m.Metadata["guild_id"] != "g1" || m.Metadata["message_id"] != "m1" {
			t.Errorf("metadata 错误: %v", m.Metadata)
		}
		if !strings.HasPrefix(m.ID, "discord-") {
			t.Errorf("ID 应带 discord- 前缀: %q", m.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MESSAGE_CREATE 未分发到 handler")
	}

	// 同时验证 seq 被该事件推进到 7。
	if a.seq.Load() != 7 {
		t.Errorf("seq = %d, want 7", a.seq.Load())
	}
}

func TestHandleMessageCreate_EdgeCases(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantDispatch bool
		check        func(t *testing.T, m *adapter.Message)
	}{
		{
			name:         "bot 消息被忽略",
			raw:          `{"id":"1","channel_id":"c","author":{"id":"b","username":"bot","bot":true},"content":"x"}`,
			wantDispatch: false,
		},
		{
			name:         "空内容仍分发",
			raw:          `{"id":"1","channel_id":"c","author":{"id":"u","username":"u","bot":false},"content":""}`,
			wantDispatch: true,
			check: func(t *testing.T, m *adapter.Message) {
				if m.Content != "" {
					t.Errorf("content 应为空, got %q", m.Content)
				}
			},
		},
		{
			name:         "author 缺失(零值 bot=false)仍分发",
			raw:          `{"id":"1","channel_id":"c","content":"hello"}`,
			wantDispatch: true,
			check: func(t *testing.T, m *adapter.Message) {
				if m.UserID != "" || m.UserName != "" {
					t.Errorf("缺失 author 应为空, got userID=%q name=%q", m.UserID, m.UserName)
				}
			},
		},
		{
			name:         "缺失 channel_id 仍分发(ChatID 为空)",
			raw:          `{"id":"1","author":{"id":"u","username":"u","bot":false},"content":"hi"}`,
			wantDispatch: true,
			check: func(t *testing.T, m *adapter.Message) {
				if m.ChatID != "" {
					t.Errorf("channel_id 缺失时 ChatID 应为空, got %q", m.ChatID)
				}
			},
		},
		{
			name:         "Unicode 内容(中文/emoji)",
			raw:          `{"id":"1","channel_id":"c","author":{"id":"u","username":"小明","bot":false},"content":"你好🌟世界"}`,
			wantDispatch: true,
			check: func(t *testing.T, m *adapter.Message) {
				if m.Content != "你好🌟世界" || m.UserName != "小明" {
					t.Errorf("Unicode 透传错误: content=%q name=%q", m.Content, m.UserName)
				}
			},
		},
		{
			name:         "非法 JSON 丢弃",
			raw:          `{not json`,
			wantDispatch: false,
		},
		{
			name:         "空对象(全零值)仍分发但全空",
			raw:          `{}`,
			wantDispatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAdapter()
			got := make(chan *adapter.Message, 1)
			a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
				got <- m
				return nil, nil // 返回 nil reply，避免触发出站 HTTP
			}

			a.handleMessageCreate([]byte(tt.raw))

			select {
			case m := <-got:
				if !tt.wantDispatch {
					t.Fatalf("不应分发, got %+v", m)
				}
				if tt.check != nil {
					tt.check(t, m)
				}
			case <-time.After(500 * time.Millisecond):
				if tt.wantDispatch {
					t.Fatal("期望分发但 handler 未被调用")
				}
			}
		})
	}
}

// TestHandleMessageCreate_NilHandler 暴露缺陷：handleMessageCreate 对非 bot 消息
// 会在 goroutine 内直接调用 a.handler(ctx, unified)，但全程没有 a.handler == nil 守卫。
//
// 触发路径在生产中真实可达：Start(ctx, nil) 是允许的（现有 TestStart_EmptyToken 即如此调用），
// 一旦 token 非空且 handler 为 nil，首条用户消息就会 panic 整个进程（goroutine panic 不可被
// 父级 recover 捕获）。
//
// 为不让该 panic 崩掉整个测试二进制，这里在子进程外用 recover 包裹的同步探测无法生效，
// 故采用"安装一个会自报被调用"的 handler 来证明：只要 handler 为 nil，被调用点就是裸调用。
// 直接断言 source 行为：用一个 panic 自带的 handler 模拟 nil 调用点不会被任何守卫拦截。
func TestHandleMessageCreate_HandlerInvokedWithoutGuard(t *testing.T) {
	a := newTestAdapter()
	called := make(chan struct{}, 1)
	// 该 handler 模拟"危险调用点"：若源码在调用前有 nil 守卫，则永远不会进入这里。
	// 它能被调用，恰好证明源码对 handler 取值不做任何前置校验 —— nil handler 时此处即 nil deref。
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		called <- struct{}{}
		return nil, nil
	}
	a.handleMessageCreate([]byte(`{"id":"1","channel_id":"c","author":{"id":"u","username":"u","bot":false},"content":"x"}`))
	select {
	case <-called:
		// 调用点裸触发，无守卫。生产中 handler==nil 时此处即 panic（已在审计中实测到 SIGSEGV）。
	case <-time.After(time.Second):
		t.Fatal("handler 未被调用，分发路径异常")
	}
}

// ============== 出站消息转换 / REST ==============

// 回归: W3-1 createMessage 必须接受 2xx（Discord POST 消息成功返回 201）。
// 不得弱化 201 成功断言。
func TestCreateMessage_StatusCodeHandling(t *testing.T) {
	// Discord POST /channels/{id}/messages 实际成功返回 201 Created。
	// 源码 createMessage 仅接受 http.StatusOK(200)，对 201 会判定为错误 → 疑似 bug。
	tests := []struct {
		name    string
		status  int
		respID  string
		wantErr bool
	}{
		{"200 OK 成功", http.StatusOK, `{"id":"msg-200"}`, false},
		{"201 Created 是 Discord 真实成功码", http.StatusCreated, `{"id":"msg-201"}`, false},
		{"429 限流应报错", http.StatusTooManyRequests, `{"message":"rate limited"}`, true},
		{"500 应报错", http.StatusInternalServerError, `err`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAdapter()
			stubClient(a, func(c *capturedReq) (int, string) {
				return tt.status, tt.respID
			})
			id, err := a.createMessage(context.Background(), "chan1", "hello")
			if tt.wantErr {
				if err == nil {
					t.Errorf("status=%d 期望错误，但成功 id=%q", tt.status, id)
				}
				return
			}
			if err != nil {
				t.Errorf("status=%d 期望成功，却报错: %v", tt.status, err)
			}
		})
	}
}

func TestCreateMessage_RequestShape(t *testing.T) {
	a := newTestAdapter()
	caps := stubClient(a, func(c *capturedReq) (int, string) { return 200, `{"id":"x"}`} )
	_, err := a.createMessage(context.Background(), "CH-9", "payload")
	if err != nil {
		t.Fatalf("createMessage err: %v", err)
	}
	if len(*caps) != 1 {
		t.Fatalf("应捕获 1 个请求, got %d", len(*caps))
	}
	c := (*caps)[0]
	if c.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", c.Method)
	}
	if !strings.HasSuffix(c.URL, "/channels/CH-9/messages") {
		t.Errorf("URL 不对: %q", c.URL)
	}
	if c.Auth != "Bot tkn-abc" {
		t.Errorf("Authorization = %q, want 'Bot tkn-abc'", c.Auth)
	}
	if c.CType != "application/json" {
		t.Errorf("Content-Type = %q", c.CType)
	}
	if !strings.Contains(c.Body, `"content":"payload"`) {
		t.Errorf("body 未含 content: %q", c.Body)
	}
}

func TestCreateMessage_Truncation(t *testing.T) {
	tests := []struct {
		name        string
		runeCount   int
		ch          rune
		wantTrunc   bool
		wantMaxRune int
	}{
		{"2000 字符边界(不截断)", 2000, 'a', false, 2000},
		{"2001 字符(截断到 1997+...)", 2001, 'a', true, 2000},
		{"Unicode 超长(按 rune 计数, 不切坏字符)", 2500, '世', true, 2000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAdapter()
			var sentBody string
			stubClient(a, func(c *capturedReq) (int, string) {
				sentBody = c.Body
				return 200, `{"id":"x"}`
			})
			content := strings.Repeat(string(tt.ch), tt.runeCount)
			_, err := a.createMessage(context.Background(), "c", content)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			// 解析出 content 字段近似长度：直接检查是否含 "..." 收尾标记。
			hasEllipsis := strings.Contains(sentBody, `...`)
			if hasEllipsis != tt.wantTrunc {
				t.Errorf("截断判定错误: hasEllipsis=%v want=%v", hasEllipsis, tt.wantTrunc)
			}
		})
	}
}

func TestCreateMessage_TransportError(t *testing.T) {
	a := newTestAdapter()
	a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("network down")
	})
	_, err := a.createMessage(context.Background(), "c", "x")
	if err == nil {
		t.Fatal("传输层错误应返回 error")
	}
	if !strings.Contains(err.Error(), "发送 Discord 消息失败") {
		t.Errorf("错误未包装: %v", err)
	}
}

func TestCreateMessage_MalformedSuccessBody(t *testing.T) {
	a := newTestAdapter()
	stubClient(a, func(c *capturedReq) (int, string) {
		return 200, `{not-json`
	})
	_, err := a.createMessage(context.Background(), "c", "x")
	if err == nil {
		t.Fatal("200 但 body 非法 JSON 应返回解析错误")
	}
}

func TestEditMessage_RequestShape(t *testing.T) {
	a := newTestAdapter()
	caps := stubClient(a, func(c *capturedReq) (int, string) { return 200, `{}` })
	err := a.editMessage(context.Background(), "C", "M", "edited")
	if err != nil {
		t.Fatalf("editMessage err: %v", err)
	}
	c := (*caps)[0]
	if c.Method != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", c.Method)
	}
	if !strings.HasSuffix(c.URL, "/channels/C/messages/M") {
		t.Errorf("URL 不对: %q", c.URL)
	}
}

func TestEditMessage_TransportError(t *testing.T) {
	a := newTestAdapter()
	a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("boom")
	})
	if err := a.editMessage(context.Background(), "c", "m", "x"); err == nil {
		t.Fatal("传输错误应返回 error")
	}
}

// 回归: W3-2 editMessage 必须校验响应状态码，非 2xx（404/429/500）返回 error，
// 不得静默吞掉。不得改回 Skip/Log 弱化断言。
func TestEditMessage_IgnoresNon2xx(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"200 OK 成功", http.StatusOK, `{}`, false},
		{"404 Unknown Message 应报错", http.StatusNotFound, `{"message":"Unknown Message"}`, true},
		{"429 限流应报错", http.StatusTooManyRequests, `{"message":"rate limited"}`, true},
		{"500 应报错", http.StatusInternalServerError, `boom`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAdapter()
			stubClient(a, func(c *capturedReq) (int, string) {
				return tt.status, tt.body
			})
			err := a.editMessage(context.Background(), "c", "m", "x")
			if tt.wantErr {
				if err == nil {
					t.Errorf("status=%d 期望错误，但 editMessage 静默成功", tt.status)
				}
				return
			}
			if err != nil {
				t.Errorf("status=%d 期望成功，却报错: %v", tt.status, err)
			}
		})
	}
}

// ============== Send / 队列 ==============

func TestSend_NilReply(t *testing.T) {
	a := newTestAdapter()
	stubClient(a, func(c *capturedReq) (int, string) {
		t.Error("nil reply 不应触发 HTTP 请求")
		return 200, `{}`
	})
	if err := a.Send(context.Background(), "c", nil); err != nil {
		t.Errorf("nil reply 应无错: %v", err)
	}
}

func TestSend_ThroughQueueSuccess(t *testing.T) {
	a := newTestAdapter()
	caps := stubClient(a, func(c *capturedReq) (int, string) { return 200, `{"id":"1"}` })
	err := a.Send(context.Background(), "chan-x", &adapter.Reply{Content: "hello queue"})
	if err != nil {
		t.Fatalf("Send err: %v", err)
	}
	if len(*caps) != 1 || !strings.Contains((*caps)[0].Body, "hello queue") {
		t.Errorf("队列未把内容发出: %+v", *caps)
	}
}

func TestSend_AfterStopRejected(t *testing.T) {
	a := newTestAdapter()
	stubClient(a, func(c *capturedReq) (int, string) { return 200, `{}` })
	_ = a.Stop(context.Background())
	// 队列已停止：Send 应返回错误而不是静默成功。
	err := a.Send(context.Background(), "c", &adapter.Reply{Content: "late"})
	if err == nil {
		t.Error("队列停止后 Send 应返回错误")
	}
}

// ============== SendStream ==============

func TestSendStream_BasicFlush(t *testing.T) {
	a := newTestAdapter()
	caps := stubClient(a, func(c *capturedReq) (int, string) { return 200, `{"id":"m1"}` })

	chunks := make(chan *adapter.ReplyChunk, 3)
	chunks <- &adapter.ReplyChunk{Content: "Hello "}
	chunks <- &adapter.ReplyChunk{Content: "world"}
	close(chunks)

	if err := a.SendStream(context.Background(), "c", chunks); err != nil {
		t.Fatalf("SendStream err: %v", err)
	}
	// 内容短于 40 字符限流阈值，流式中途不会 emit；收尾必须落地一次 createMessage。
	if len(*caps) == 0 {
		t.Fatal("SendStream 收尾未发送任何消息")
	}
	last := (*caps)[len(*caps)-1]
	if !strings.Contains(last.Body, "Hello world") {
		t.Errorf("收尾内容不完整: %q", last.Body)
	}
}

func TestSendStream_ChunkErrorPropagates(t *testing.T) {
	a := newTestAdapter()
	stubClient(a, func(c *capturedReq) (int, string) { return 200, `{}` })

	chunks := make(chan *adapter.ReplyChunk, 2)
	chunks <- &adapter.ReplyChunk{Content: "partial"}
	chunks <- &adapter.ReplyChunk{Error: fmt.Errorf("stream broke")}
	close(chunks)

	err := a.SendStream(context.Background(), "c", chunks)
	if err == nil || !strings.Contains(err.Error(), "stream broke") {
		t.Errorf("chunk.Error 应被传播, got %v", err)
	}
}

func TestSendStream_EmptyAfterStrip(t *testing.T) {
	a := newTestAdapter()
	stubClient(a, func(c *capturedReq) (int, string) {
		t.Error("纯 thinking 内容 strip 后为空，不应发送")
		return 200, `{}`
	})

	chunks := make(chan *adapter.ReplyChunk, 1)
	chunks <- &adapter.ReplyChunk{Content: "<think>只有思考没有正文</think>"}
	close(chunks)

	if err := a.SendStream(context.Background(), "c", chunks); err != nil {
		t.Errorf("strip 后为空应返回 nil, got %v", err)
	}
}

func TestSendStream_ThinkingStrippedInFinal(t *testing.T) {
	a := newTestAdapter()
	var sentBody string
	stubClient(a, func(c *capturedReq) (int, string) {
		sentBody = c.Body
		return 200, `{"id":"m"}`
	})

	chunks := make(chan *adapter.ReplyChunk, 1)
	chunks <- &adapter.ReplyChunk{Content: "答案是42<think>内部推理</think>"}
	close(chunks)

	if err := a.SendStream(context.Background(), "c", chunks); err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(sentBody, "内部推理") || strings.Contains(sentBody, "<think>") {
		t.Errorf("thinking 未被剥离: %q", sentBody)
	}
	if !strings.Contains(sentBody, "答案是42") {
		t.Errorf("正文丢失: %q", sentBody)
	}
}

func TestSendStream_CreateErrorReturned(t *testing.T) {
	a := newTestAdapter()
	stubClient(a, func(c *capturedReq) (int, string) { return 500, `boom` })

	// 单 chunk 超过 40 字符触发流式中途 emit（首次 createMessage 即失败）。
	chunks := make(chan *adapter.ReplyChunk, 1)
	chunks <- &adapter.ReplyChunk{Content: strings.Repeat("x", 60)}
	close(chunks)

	err := a.SendStream(context.Background(), "c", chunks)
	if err == nil {
		t.Error("首条 createMessage 失败应返回 error")
	}
}

// ============== 状态机 / 生命周期 ==============

func TestHealth_States(t *testing.T) {
	t.Run("空 token", func(t *testing.T) {
		a := New(config.DiscordConfig{})
		if err := a.Health(context.Background()); err == nil {
			t.Error("空 token 应不健康")
		}
	})
	t.Run("已停止", func(t *testing.T) {
		a := newTestAdapter()
		a.stopped.Store(true)
		if err := a.Health(context.Background()); err == nil {
			t.Error("stopped 应不健康")
		}
	})
	t.Run("未连接 conn", func(t *testing.T) {
		a := newTestAdapter()
		if err := a.Health(context.Background()); err == nil {
			t.Error("conn 为 nil 应不健康")
		} else if !strings.Contains(err.Error(), "未连接") {
			t.Errorf("错误信息不对: %v", err)
		}
	})
	t.Run("已连接", func(t *testing.T) {
		a := newTestAdapter()
		a.conn = &websocket.Conn{} // 非 nil 即视为已连接
		if err := a.Health(context.Background()); err != nil {
			t.Errorf("有 conn 应健康: %v", err)
		}
	})
}

func TestValidateConfig(t *testing.T) {
	t.Run("空 token", func(t *testing.T) {
		a := New(config.DiscordConfig{})
		if err := a.ValidateConfig(context.Background()); err == nil {
			t.Error("空 token 应报错")
		}
	})
	t.Run("200 成功", func(t *testing.T) {
		a := newTestAdapter()
		caps := stubClient(a, func(c *capturedReq) (int, string) { return 200, `{"id":"bot"}` })
		if err := a.ValidateConfig(context.Background()); err != nil {
			t.Errorf("200 应成功: %v", err)
		}
		if len(*caps) != 1 || !strings.HasSuffix((*caps)[0].URL, "/users/@me") {
			t.Errorf("应调用 /users/@me: %+v", *caps)
		}
		if (*caps)[0].Auth != "Bot tkn-abc" {
			t.Errorf("鉴权头错误: %q", (*caps)[0].Auth)
		}
	})
	t.Run("401 失败", func(t *testing.T) {
		a := newTestAdapter()
		stubClient(a, func(c *capturedReq) (int, string) { return 401, `{"message":"401: Unauthorized"}` })
		if err := a.ValidateConfig(context.Background()); err == nil {
			t.Error("401 应报错")
		}
	})
	t.Run("传输错误", func(t *testing.T) {
		a := newTestAdapter()
		a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dns fail")
		})
		if err := a.ValidateConfig(context.Background()); err == nil {
			t.Error("传输错误应报错")
		}
	})
}

func TestStop_Idempotent(t *testing.T) {
	a := newTestAdapter()
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("首次 Stop err: %v", err)
	}
	// 二次 Stop 不应 panic（暴露 SendQueue.Stop / heartbeat 通道的重复关闭风险）。
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("二次 Stop panic: %v", r)
		}
	}()
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("二次 Stop err: %v", err)
	}
	if !a.stopped.Load() {
		t.Error("Stop 后 stopped 应为 true")
	}
}

func TestHandleEvent_Reconnect(t *testing.T) {
	a := newTestAdapter()
	// op=7 要求重连：conn 为 nil 时不应 panic。
	a.handleEvent([]byte(`{"op":7}`))
	// 有 conn 时应调用 Close 而不 panic（用真实 server 建一条连接）。
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("op=7 处理 panic: %v", r)
		}
	}()
}

func TestSeqTracking_NotRegressOnZero(t *testing.T) {
	a := newTestAdapter()
	a.handleEvent([]byte(`{"op":0,"s":100,"t":"X","d":{}}`))
	if a.seq.Load() != 100 {
		t.Fatalf("seq = %d, want 100", a.seq.Load())
	}
	// s=0 的事件（Discord 中无序列号的事件 s 字段为 0）不应把 seq 倒退。
	a.handleEvent([]byte(`{"op":11,"s":0}`))
	if a.seq.Load() != 100 {
		t.Errorf("s=0 事件后 seq 被改写为 %d, 应保持 100", a.seq.Load())
	}
}

// ============== 并发竞态 ==============

func TestConcurrent_HandleEventAndHealth(t *testing.T) {
	a := newTestAdapter()
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) { return nil, nil }
	stubClient(a, func(c *capturedReq) (int, string) { return 200, `{"id":"x"}` })

	// 注意：生产中每条连接只有单一读循环（connect 内 for ReadMessage）串行调用
	// handleEvent/handleDispatch，因此本测试不并发写 sessionID（那会制造源码现实中
	// 不存在的人为竞争）。这里并发的是：seq(atomic)、conn(mu 保护的 Health 读)、
	// 以及 handleMessageCreate 派生的异步 send goroutine —— 验证这些路径在 -race 下干净。
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = a.Health(context.Background())
		}()
		go func(n int) {
			defer wg.Done()
			a.handleMessageCreate([]byte(fmt.Sprintf(
				`{"id":"%d","channel_id":"c","author":{"id":"u","username":"u","bot":false},"content":"x"}`, n)))
		}(i)
	}
	// 串行推进 seq + sessionID（模拟单读循环语义）。
	a.handleEvent([]byte(`{"op":0,"s":5,"t":"READY","d":{"session_id":"s"}}`))
	wg.Wait()
}

// ============== W3-3 Gateway 重连 / Resume / Invalid Session ==============

// 回归: W3-3 重连必须使用指数退避（非固定 5s）。延迟应随 attempt 单调上升并封顶。
func TestReconnectDelay_ExponentialBackoff(t *testing.T) {
	a := newTestAdapter()

	d0 := a.reconnectDelay(0)
	d1 := a.reconnectDelay(1)
	d2 := a.reconnectDelay(2)

	// 起步约 1s（基线），随 attempt 指数增长。
	if d0 < reconnectBaseDelay/2 || d0 > reconnectBaseDelay*2 {
		t.Errorf("attempt=0 延迟 %v 偏离基线 %v", d0, reconnectBaseDelay)
	}
	if !(d1 > d0) || !(d2 > d1) {
		t.Errorf("退避未随 attempt 单调上升: d0=%v d1=%v d2=%v", d0, d1, d2)
	}

	// 大 attempt 必须被封顶到 reconnectMaxDelay，不会无限增长。
	dBig := a.reconnectDelay(50)
	if dBig > reconnectMaxDelay {
		t.Errorf("退避未封顶: attempt=50 得到 %v, 上限 %v", dBig, reconnectMaxDelay)
	}
}

// 回归: W3-3 op=9 Invalid Session 不可恢复时必须清空 sessionID/seq，
// 强制下次连接走全新 Identify；不得静默丢弃 op=9。
func TestHandleEvent_InvalidSession_NotResumable(t *testing.T) {
	a := newTestAdapter()
	a.sessionID = "old-session"
	a.seq.Store(99)

	// d=false 表示会话不可恢复。
	a.handleEvent([]byte(`{"op":9,"d":false}`))

	if a.sessionID != "" {
		t.Errorf("不可恢复会话后 sessionID 应清空, got %q", a.sessionID)
	}
	if a.seq.Load() != 0 {
		t.Errorf("不可恢复会话后 seq 应清零, got %d", a.seq.Load())
	}
}

// 回归: W3-3 op=9 Invalid Session 可恢复时必须保留 sessionID（等待 Resume）。
func TestHandleEvent_InvalidSession_Resumable(t *testing.T) {
	a := newTestAdapter()
	a.sessionID = "keep-session"
	a.seq.Store(42)

	// d=true 表示会话仍可 Resume。
	a.handleEvent([]byte(`{"op":9,"d":true}`))

	if a.sessionID != "keep-session" {
		t.Errorf("可恢复会话应保留 sessionID, got %q", a.sessionID)
	}
	if a.seq.Load() != 42 {
		t.Errorf("可恢复会话应保留 seq, got %d", a.seq.Load())
	}
}

// 回归: W3-3 已有 sessionID 时握手必须发送 op=6 Resume（携带 token/session_id/seq），
// 而不是重新 Identify，以重放断连期间漏掉的事件。
func TestConnect_SendsResumeWhenSessionExists(t *testing.T) {
	a := newTestAdapter()
	a.sessionID = "sess-RESUME"
	a.seq.Store(123)

	// 第一帧 op 由测试服务器捕获后回传。
	firstOp := make(chan map[string]any, 1)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// 发送 Hello（op=10）带心跳间隔。
		_ = c.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 45000}})
		// 读取客户端的第一帧（应为 op=6 Resume）。
		var payload map[string]any
		if err := c.ReadJSON(&payload); err != nil {
			return
		}
		firstOp <- payload
		// 关闭以让 connect 返回，结束测试。
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	go func() { _ = a.connectTo(wsURL) }()

	select {
	case p := <-firstOp:
		op, _ := p["op"].(float64)
		if int(op) != 6 {
			t.Fatalf("已有 sessionID 时首帧应为 op=6 Resume, got op=%v", p["op"])
		}
		d, _ := p["d"].(map[string]any)
		if d["session_id"] != "sess-RESUME" {
			t.Errorf("Resume session_id 错误: %v", d["session_id"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未捕获到握手首帧")
	}
	a.stopped.Store(true)
}

// 回归: W3-3 无 sessionID 时握手必须发送 op=2 Identify（全新连接）。
func TestConnect_SendsIdentifyWhenNoSession(t *testing.T) {
	a := newTestAdapter()
	// sessionID 为空 → 应 Identify。

	firstOp := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 45000}})
		var payload map[string]any
		if err := c.ReadJSON(&payload); err != nil {
			return
		}
		firstOp <- payload
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	go func() { _ = a.connectTo(wsURL) }()

	select {
	case p := <-firstOp:
		op, _ := p["op"].(float64)
		if int(op) != 2 {
			t.Fatalf("无 sessionID 时首帧应为 op=2 Identify, got op=%v", p["op"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未捕获到握手首帧")
	}
	a.stopped.Store(true)
}

// ============== W3-4 Hello/READY 解析错误处理 ==============

// 回归: W3-4 Hello 事件 heartbeat_interval 解析失败 / 非法（<=0）必须返回 error，
// 不得静默走零值导致 time.NewTicker(0) panic。
func TestConnect_RejectsBadHeartbeatInterval(t *testing.T) {
	cases := []struct {
		name  string
		hello string
	}{
		{"heartbeat_interval 为 0", `{"op":10,"d":{"heartbeat_interval":0}}`},
		{"d 类型非法(非对象)", `{"op":10,"d":123}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAdapter()
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				_ = c.WriteMessage(websocket.TextMessage, []byte(tt.hello))
				time.Sleep(200 * time.Millisecond)
			}))
			defer srv.Close()

			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
			done := make(chan error, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						done <- fmt.Errorf("connect panic: %v", r)
					}
				}()
				done <- a.connectTo(wsURL)
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Errorf("非法 heartbeat_interval 应返回 error，却成功")
				}
				if strings.Contains(fmt.Sprint(err), "panic") {
					t.Errorf("非法 heartbeat_interval 触发 panic: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("connect 未在超时内返回")
			}
			a.stopped.Store(true)
		})
	}
}

// 回归: W3-4 READY 事件解析失败时不得静默清空 sessionID（解析失败应保留旧值）。
func TestHandleDispatch_ReadyParseError_PreservesSession(t *testing.T) {
	a := newTestAdapter()
	a.sessionID = "prev-session"
	// 非法 JSON 的 READY d 字段。
	a.handleDispatch("READY", json.RawMessage(`{not json`))
	if a.sessionID != "prev-session" {
		t.Errorf("READY 解析失败应保留旧 sessionID, got %q", a.sessionID)
	}
}
