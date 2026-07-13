package adapter

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errGateContext pauses Send exactly at its pre-admission ctx.Err check. It
// lets the test put Stop between the old stopped.Load and enqueue select
// without production-only timing hooks.
type errGateContext struct {
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (c *errGateContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *errGateContext) Done() <-chan struct{}       { return nil }
func (c *errGateContext) Value(any) any               { return nil }
func (c *errGateContext) Err() error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return nil
}

// doneGateContext pauses the worker while context.WithCancel wires the task
// parent. Stop can then cancel the queue before the worker reaches q.send.
type doneGateContext struct {
	entered chan struct{}
	release <-chan struct{}
	never   chan struct{}
	once    sync.Once
}

func (c *doneGateContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *doneGateContext) Value(any) any               { return nil }
func (c *doneGateContext) Err() error                  { return nil }
func (c *doneGateContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return c.never
}

// TestSendQueue_CancelledContextRejectsSend 证明:
// 当 context 已取消时，SendQueue.Send 立即返回 ctx.Err()，消息永远不会发出。
// 这正是修复前所有 IM 适配器的 bug 路径:
//
//	handler 耗尽 2min timeout → ctx deadline exceeded → Send(ctx, ...) 静默失败
func TestSendQueue_CancelledContextRejectsSend(t *testing.T) {
	var sendCalled atomic.Bool

	q := NewSendQueue(5, 128, func(ctx context.Context, chatID string, reply *Reply) error {
		sendCalled.Store(true)
		return nil
	})
	defer q.Stop(context.Background())

	// 模拟: handler 耗尽 timeout 后，ctx 已 deadline exceeded。
	// 用「已过期的 deadline」而非 WithTimeout(1ms)+Sleep：Go 在 dur<=0 时同步置
	// DeadlineExceeded（context.WithDeadline 内 `if dur <= 0 { cancel(DeadlineExceeded) }`），
	// 不依赖 timer goroutine 触发——避免全量并行套件下 timer 被调度饥饿、ctx.Err() 未翻转
	// 就被 Send 读到 nil 的 flaky（隔离能过、全量偶发 FAIL 的根因）。
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Millisecond))
	defer cancel()

	err := q.Send(ctx, "chat-1", &Reply{Content: "处理消息时出现错误"})

	if err == nil {
		t.Fatal("Send 用已取消的 ctx 应返回 error，实际返回 nil — 消息被静默吞掉")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("期望 context.DeadlineExceeded，实际: %v", err)
	}
	if sendCalled.Load() {
		t.Fatal("底层 send 函数不应被调用 — 消息在入队阶段就被拒绝")
	}
}

// TestSendQueue_FreshContextSucceeds 证明:
// 修复后使用独立的 context.Background() + timeout，Send 正常投递。
func TestSendQueue_FreshContextSucceeds(t *testing.T) {
	var capturedContent string

	q := NewSendQueue(5, 128, func(ctx context.Context, chatID string, reply *Reply) error {
		capturedContent = reply.Content
		return nil
	})
	defer q.Stop(context.Background())

	// 模拟修复后: 用新的 context 发送
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer sendCancel()

	err := q.Send(sendCtx, "chat-1", &Reply{Content: "处理消息时出现错误"})
	if err != nil {
		t.Fatalf("Send 用新 ctx 应成功，实际: %v", err)
	}
	if capturedContent != "处理消息时出现错误" {
		t.Fatalf("消息内容 = %q，期望 %q", capturedContent, "处理消息时出现错误")
	}
}

// TestSendQueue_SimulateAdapterBugAndFix 端到端模拟:
// 1. handler 消耗全部 timeout → ctx 过期
// 2. 用同一个 ctx 调 Send → 失败 (BUG)
// 3. 用新 ctx 调 Send → 成功 (FIX)
func TestSendQueue_SimulateAdapterBugAndFix(t *testing.T) {
	var sendCount atomic.Int32

	q := NewSendQueue(5, 128, func(ctx context.Context, chatID string, reply *Reply) error {
		sendCount.Add(1)
		return nil
	})
	defer q.Stop(context.Background())

	// 模拟 handler 处理: 2 分钟 timeout，handler 耗时超过 timeout
	handlerTimeout := 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()

	// 模拟 handler 执行（耗尽 timeout）
	handler := func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
			return nil
		}
	}
	handlerErr := handler(ctx)
	if handlerErr == nil {
		t.Fatal("handler 应因 timeout 返回 error")
	}

	// ── BUG 路径: 修复前的代码 ──
	// a.Send(ctx, chatID, &Reply{...})  ← 复用已过期的 ctx
	bugErr := q.Send(ctx, "chat-1", &Reply{Content: "error msg"})
	if bugErr == nil {
		t.Fatal("[BUG 路径] 用过期 ctx 调 Send 应失败")
	}
	if sendCount.Load() != 0 {
		t.Fatal("[BUG 路径] 底层 send 不应被执行")
	}

	// ── FIX 路径: 修复后的代码 ──
	// errCtx, errCancel := context.WithTimeout(context.Background(), 10*time.Second)
	// a.Send(errCtx, chatID, &Reply{...})  ← 独立 ctx
	errCtx, errCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer errCancel()
	fixErr := q.Send(errCtx, "chat-1", &Reply{Content: "error msg"})
	if fixErr != nil {
		t.Fatalf("[FIX 路径] 用新 ctx 调 Send 应成功，实际: %v", fixErr)
	}
	if sendCount.Load() != 1 {
		t.Fatalf("[FIX 路径] 底层 send 应被执行 1 次，实际: %d", sendCount.Load())
	}
}

func TestSendQueue_StopCancelsActiveSend(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	q := NewSendQueue(5, 8, func(ctx context.Context, _ string, _ *Reply) error {
		close(started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return errors.New("test send released")
		}
	})
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- q.Send(context.Background(), "chat-1", &Reply{Content: "hello"})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("send did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- q.Stop(ctx) }()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error after canceling active send: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		<-stopDone
		t.Fatal("Stop did not cancel the active send")
	}
	if err := <-sendDone; err == nil {
		t.Fatal("active Send returned nil while the queue was stopping")
	}
}

func TestSendQueue_StopHonorsDeadlineWhenSenderIgnoresContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	q := NewSendQueue(5, 8, func(context.Context, string, *Reply) error {
		close(started)
		<-release
		return nil
	})
	go func() { _ = q.Send(context.Background(), "chat-1", &Reply{Content: "hello"}) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("send did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- q.Stop(ctx) }()
	select {
	case err := <-stopDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Stop error = %v, want context deadline exceeded", err)
		}
	case <-time.After(100 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		<-stopDone
		t.Fatal("Stop ignored its context deadline")
	}
	releaseOnce.Do(func() { close(release) })
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop did not converge after sender exit: %v", err)
	}
}

func TestSendQueue_StopLinearizesWithBlockedAdmissions(t *testing.T) {
	const senders = 128
	release := make(chan struct{})
	q := NewSendQueue(1000, senders, func(context.Context, string, *Reply) error {
		return errors.New("send must not run after stop")
	})

	var wg sync.WaitGroup
	results := make(chan error, senders)
	entered := make([]chan struct{}, senders)
	for i := range senders {
		entered[i] = make(chan struct{})
		ctx := &errGateContext{entered: entered[i], release: release}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- q.Send(ctx, "chat", &Reply{Content: "queued"})
		}()
	}
	for _, ch := range entered {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("Send did not reach its admission boundary")
		}
	}

	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err == nil {
			t.Fatal("a Send admitted after Stop returned nil")
		}
	}
	if got := len(q.tasks); got != 0 {
		t.Fatalf("%d tasks were enqueued after Stop had already drained the queue", got)
	}
}

func TestSendQueue_WorkerNeverCallsSenderAfterStop(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var called atomic.Bool
	q := NewSendQueue(1000, 1, func(context.Context, string, *Reply) error {
		called.Store(true)
		return nil
	})
	gate := &doneGateContext{entered: entered, release: release, never: make(chan struct{})}
	q.tasks <- &sendTask{
		ctx: gate, chatID: "chat", reply: &Reply{Content: "must be rejected"}, done: make(chan error, 1),
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not reach the controlled pre-send boundary")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := q.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop while worker is paused = %v, want deadline exceeded", err)
	}
	close(release)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop did not converge after releasing worker: %v", err)
	}
	if called.Load() {
		t.Fatal("worker called the sender after queue Stop")
	}
}
