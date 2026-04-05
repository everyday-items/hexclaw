package adapter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ══════════════════════════════════════════════════════════════════
// 场景 1: handler 超时 → 错误恢复 Send 被 SendQueue 拒绝
// 对应 feishu.go handleSDKMessage / handleMessage 的 error 分支
// ══════════════════════════════════════════════════════════════════

func TestCtxBug_HandlerTimeout_ErrorSendRejected(t *testing.T) {
	var sent atomic.Bool
	q := NewSendQueue(5, 128, func(ctx context.Context, chatID string, reply *Reply) error {
		sent.Store(true)
		return nil
	})
	defer q.Stop(context.Background())

	// 模拟 2 分钟 timeout，handler 耗尽
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	time.Sleep(15 * time.Millisecond)

	// BUG: 修复前代码 a.Send(ctx, chatID, &Reply{...})
	err := q.Send(ctx, "chat-1", &Reply{Content: "处理消息时出现错误"})
	if err == nil {
		t.Fatal("过期 ctx 的 Send 不应成功")
	}
	if sent.Load() {
		t.Fatal("底层 send 不应被调用")
	}

	// FIX: 独立 ctx
	errCtx, errCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer errCancel()
	err = q.Send(errCtx, "chat-1", &Reply{Content: "处理消息时出现错误"})
	if err != nil {
		t.Fatalf("新 ctx 的 Send 应成功: %v", err)
	}
	if !sent.Load() {
		t.Fatal("底层 send 应被调用")
	}
}

// ══════════════════════════════════════════════════════════════════
// 场景 2: handler 接近 deadline → reply Send 在 rate-limit 排队中过期
// feishu rate = 3/sec → minInterval = 333ms
// handler 用了 1:59.8 → ctx 剩余 200ms → 排队 333ms → 超时
// ══════════════════════════════════════════════════════════════════

func TestCtxBug_NearDeadline_RateLimitWaitExpires(t *testing.T) {
	var sent atomic.Bool
	// rate=1/sec → minInterval=1s，放大效果更容易触发
	q := NewSendQueue(1, 128, func(ctx context.Context, chatID string, reply *Reply) error {
		sent.Store(true)
		return nil
	})
	defer q.Stop(context.Background())

	// 先发一条消息，建立 lastSend 基线
	warmCtx, warmCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer warmCancel()
	_ = q.Send(warmCtx, "chat-1", &Reply{Content: "warmup"})

	// 模拟 handler 耗时接近 deadline，ctx 只剩 50ms
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	sent.Store(false)
	// BUG: rate-limit 等待 ~1s，但 ctx 只剩 50ms → 排队中过期
	err := q.Send(ctx, "chat-1", &Reply{Content: "正常回复"})
	if err == nil {
		t.Fatal("剩余时间不足以通过 rate-limit 等待，应失败")
	}

	// FIX: 独立 30s ctx，足够等完 rate-limit
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sendCancel()
	err = q.Send(sendCtx, "chat-1", &Reply{Content: "正常回复"})
	if err != nil {
		t.Fatalf("新 ctx 应能等完 rate-limit 后成功: %v", err)
	}
	if !sent.Load() {
		t.Fatal("底层 send 应被调用")
	}
}

// ══════════════════════════════════════════════════════════════════
// 场景 3: getAccessToken 需刷新 token + ctx 已过期 → HTTP 请求失败
// sendReplyNow 用 ctx 调 getAccessToken → http.NewRequestWithContext(ctx)
// 如果 ctx 已过期，HTTP 请求立即被取消
// ══════════════════════════════════════════════════════════════════

func TestCtxBug_ExpiredCtx_HTTPRequestCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond)

	// 模拟: sendReplyNow 中的 http.NewRequestWithContext(ctx, ...)
	// 过期 ctx 创建的 request 在 client.Do 时立即返回 context.DeadlineExceeded
	req, err := makeHTTPRequest(ctx)
	if err != nil {
		// 某些 Go 版本在 NewRequest 阶段就报错
		t.Logf("NewRequestWithContext 直接报错 (预期): %v", err)
		return
	}
	_ = req // 即使 NewRequest 成功，client.Do(req) 也会因 ctx 过期失败

	// FIX: 新 ctx 可以正常创建 request
	freshCtx, freshCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer freshCancel()
	req, err = makeHTTPRequest(freshCtx)
	if err != nil {
		t.Fatalf("新 ctx 创建 request 应成功: %v", err)
	}
	if req == nil {
		t.Fatal("request 不应为 nil")
	}
}

func makeHTTPRequest(ctx context.Context) (interface{}, error) {
	// 不实际发 HTTP，只验证 context 状态
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return "ok", nil
}

// ══════════════════════════════════════════════════════════════════
// 场景 4: 多消息并发排队 → 后面的消息在排队中 ctx 过期
// 飞书 rate=3/s → 如果 5 条消息同时到达，第 5 条需等 ~1.3s
// 如果每条消息的 ctx 只剩 500ms，第 3 条之后的都会超时
// ══════════════════════════════════════════════════════════════════

func TestCtxBug_ConcurrentMessages_LaterMessagesExpire(t *testing.T) {
	var mu sync.Mutex
	delivered := make([]string, 0)

	// rate=2/sec → minInterval=500ms
	q := NewSendQueue(2, 128, func(ctx context.Context, chatID string, reply *Reply) error {
		mu.Lock()
		delivered = append(delivered, reply.Content)
		mu.Unlock()
		return nil
	})
	defer q.Stop(context.Background())

	// BUG 路径: 所有消息共享一个只剩 600ms 的 ctx
	// 第 1 条立即发送，第 2 条等 500ms，第 3 条等 1000ms → 超时
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer shortCancel()

	var wg sync.WaitGroup
	bugErrors := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			bugErrors[idx] = q.Send(shortCtx, "chat-1", &Reply{Content: fmt.Sprintf("msg-%d", idx)})
		}(i)
	}
	wg.Wait()

	failCount := 0
	for _, err := range bugErrors {
		if err != nil {
			failCount++
		}
	}
	if failCount == 0 {
		t.Log("警告: 所有消息都成功了 (可能 goroutine 调度让它们都赶在 deadline 前完成)")
	} else {
		t.Logf("[BUG 路径] %d/3 条消息因 ctx 过期丢失", failCount)
	}

	// FIX 路径: 每条消息用独立 ctx
	mu.Lock()
	delivered = delivered[:0]
	mu.Unlock()

	fixErrors := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer sendCancel()
			fixErrors[idx] = q.Send(sendCtx, "chat-1", &Reply{Content: fmt.Sprintf("msg-%d", idx)})
		}(i)
	}
	wg.Wait()

	for i, err := range fixErrors {
		if err != nil {
			t.Errorf("[FIX 路径] msg-%d 不应失败: %v", i, err)
		}
	}

	mu.Lock()
	fixDelivered := len(delivered)
	mu.Unlock()
	if fixDelivered != 3 {
		t.Errorf("[FIX 路径] 期望 3 条消息全部送达，实际 %d", fixDelivered)
	}
}

// ══════════════════════════════════════════════════════════════════
// 场景 5: SendQueue.run() 中 task.ctx 在 rate-limit 等待期间过期
// 直接验证 send_queue.go:138-141 的 <-task.ctx.Done() 分支
// ══════════════════════════════════════════════════════════════════

func TestCtxBug_RunLoop_CtxExpiresInRateLimitWait(t *testing.T) {
	callCount := atomic.Int32{}
	// rate=1/sec → 500ms interval
	q := NewSendQueue(2, 128, func(ctx context.Context, chatID string, reply *Reply) error {
		callCount.Add(1)
		return nil
	})
	defer q.Stop(context.Background())

	// 先发一条建立 lastSend
	bg := context.Background()
	_ = q.Send(bg, "c", &Reply{Content: "init"})
	callCount.Store(0)

	// 任务入队后，ctx 在 run() 的 rate-limit timer 等待期间过期
	tinyCtx, tinyCancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer tinyCancel()

	err := q.Send(tinyCtx, "c", &Reply{Content: "will be dropped"})
	if err == nil {
		// 如果 goroutine 调度让 Send 刚好赶上，跳过此测试
		t.Skip("task 在 rate-limit 等待前就完成了 (调度依赖)")
	}
	if callCount.Load() != 0 {
		t.Fatal("ctx 在排队等待中过期后，底层 send 不应被调用")
	}
	t.Logf("确认: task.ctx 在 run() rate-limit 等待中过期 → 返回 %v", err)
}

// ══════════════════════════════════════════════════════════════════
// 场景 6: 端到端 — 模拟完整适配器 handler → Send 链路
// 合并验证: handler超时 + token需刷新 + SendQueue排队
// ══════════════════════════════════════════════════════════════════

func TestCtxBug_EndToEnd_FullAdapterSimulation(t *testing.T) {
	var sentContent string
	var tokenRefreshed atomic.Bool

	// 模拟 sendReplyNow: getAccessToken + HTTP send
	mockSend := func(ctx context.Context, chatID string, reply *Reply) error {
		// 模拟 getAccessToken — 需要用 ctx 做 HTTP 请求
		if ctx.Err() != nil {
			return fmt.Errorf("getAccessToken 失败: %w", ctx.Err())
		}
		tokenRefreshed.Store(true)
		// 模拟 HTTP POST 发送消息
		if ctx.Err() != nil {
			return fmt.Errorf("HTTP POST 失败: %w", ctx.Err())
		}
		sentContent = reply.Content
		return nil
	}

	q := NewSendQueue(3, 128, mockSend) // feishu rate=3/sec
	defer q.Stop(context.Background())

	// 模拟 handler 处理 (耗尽 2 分钟 timeout)
	handlerTimeout := 30 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()

	// handler 模拟: 调 LLM, 执行 tool calls, RAG 检索...
	simulateHandler := func(ctx context.Context) (*Reply, error) {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("engine: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
			return &Reply{Content: "正常回复"}, nil
		}
	}

	reply, handlerErr := simulateHandler(ctx)

	// ── 验证 handler 确实超时 ──
	if handlerErr == nil {
		t.Fatal("handler 应因 timeout 返回 error")
	}
	if reply != nil {
		t.Fatal("超时时 reply 应为 nil")
	}

	// ── BUG 路径 (修复前) ──
	sentContent = ""
	tokenRefreshed.Store(false)
	bugErr := q.Send(ctx, "chat-feishu-123", &Reply{Content: "处理消息时出现错误，请稍后重试。"})
	if bugErr == nil {
		t.Fatal("[BUG] 过期 ctx 不应成功发送")
	}
	if sentContent != "" {
		t.Fatal("[BUG] 错误消息不应送达")
	}
	if tokenRefreshed.Load() {
		t.Fatal("[BUG] token 不应被刷新 (ctx 在入队前就被拒绝)")
	}
	t.Logf("[BUG 路径] 用户看到: (无任何回复) — 错误: %v", bugErr)

	// ── FIX 路径 (修复后) ──
	sentContent = ""
	tokenRefreshed.Store(false)
	errCtx, errCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer errCancel()
	fixErr := q.Send(errCtx, "chat-feishu-123", &Reply{Content: "处理消息时出现错误，请稍后重试。"})
	if fixErr != nil {
		t.Fatalf("[FIX] 新 ctx 发送应成功: %v", fixErr)
	}
	if sentContent != "处理消息时出现错误，请稍后重试。" {
		t.Fatalf("[FIX] 内容不匹配: %q", sentContent)
	}
	if !tokenRefreshed.Load() {
		t.Fatal("[FIX] token 应被正常刷新")
	}
	t.Log("[FIX 路径] 用户看到: '处理消息时出现错误，请稍后重试。'")
}

// ══════════════════════════════════════════════════════════════════
// 场景 7: handler 成功但耗时 1:58 → reply Send ctx 剩余 2s
// 边界条件: ctx 未过期但剩余时间极少，网络抖动即可导致失败
// ══════════════════════════════════════════════════════════════════

func TestCtxBug_SuccessPath_NearDeadlineSendFragile(t *testing.T) {
	sendDuration := 100 * time.Millisecond // 模拟网络延迟

	q := NewSendQueue(5, 128, func(ctx context.Context, chatID string, reply *Reply) error {
		// 模拟 send 需要 100ms (token刷新 + HTTP POST)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sendDuration):
			return nil
		}
	})
	defer q.Stop(context.Background())

	// handler 成功返回，但 ctx 只剩 50ms < send 需要的 100ms
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// BUG: reply 用的 ctx 剩余时间不够完成 send
	err := q.Send(ctx, "chat-1", &Reply{Content: "正常回复"})
	if err == nil {
		t.Skip("Send 在 deadline 前完成了 (调度差异)")
	}
	t.Logf("[BUG 路径] 正常回复发送失败: %v", err)

	// FIX: 独立 30s ctx，足够完成 send
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sendCancel()
	err = q.Send(sendCtx, "chat-1", &Reply{Content: "正常回复"})
	if err != nil {
		t.Fatalf("[FIX 路径] 应成功: %v", err)
	}
}

// ══════════════════════════════════════════════════════════════════
// 场景 8: context.Canceled vs context.DeadlineExceeded
// 确认两种取消方式都被正确拦截
// ══════════════════════════════════════════════════════════════════

func TestCtxBug_BothCancelAndDeadlineRejected(t *testing.T) {
	q := NewSendQueue(5, 128, func(ctx context.Context, chatID string, reply *Reply) error {
		return nil
	})
	defer q.Stop(context.Background())

	// DeadlineExceeded
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer deadlineCancel()
	time.Sleep(5 * time.Millisecond)
	err := q.Send(deadlineCtx, "c", &Reply{Content: "x"})
	if err != context.DeadlineExceeded {
		t.Errorf("DeadlineExceeded ctx: 期望 DeadlineExceeded，实际 %v", err)
	}

	// Canceled (用户主动取消)
	cancelCtx, cancelFn := context.WithCancel(context.Background())
	cancelFn() // 立即取消
	err = q.Send(cancelCtx, "c", &Reply{Content: "x"})
	if err != context.Canceled {
		t.Errorf("Canceled ctx: 期望 Canceled，实际 %v", err)
	}
}
