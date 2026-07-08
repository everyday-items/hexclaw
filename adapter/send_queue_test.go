package adapter

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

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

	// 模拟: handler 耗尽 timeout 后，ctx 已 deadline exceeded
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // 确保 ctx 已过期

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
