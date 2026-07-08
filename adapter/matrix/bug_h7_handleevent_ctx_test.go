package matrix

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type bugH7CtxKey struct{}

// TestBugH7_MatrixHandleEvent_PreservesCtxValues 是 BUG-H7 的 regression test。
//
// BUG：在未修版本中，handleEvent 未接收 ctx，goroutine 里直接用 context.Background()
//
//	调用 handler —— parent 请求的 logger/session/user_id 全部丢失，
//	出 bug 时异步路径的日志无法与源头请求关联。
//
// 契约：handleEvent 必须接收 parent ctx；goroutine 里调用 handler 时
//
//	传入的 ctx 必须保留 parent ctx 的 Values。
//
// RED（未修版本）：handleEvent 签名不带 ctx → 本测试编译失败 [build failed]
// GREEN（修复版本）：handleEvent(ctx, roomID, event) + trace.Detach(ctx)
//
//	→ handler 收到的 ctx 保留 bugH7CtxKey 的 Value
func TestBugH7_MatrixHandleEvent_PreservesCtxValues(t *testing.T) {
	var capturedCtx context.Context
	done := make(chan struct{})
	var once sync.Once

	a := New(Config{UserID: "@bot:example.com"})
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		capturedCtx = ctx
		once.Do(func() { close(done) })
		return nil, nil
	}

	parentCtx := context.WithValue(context.Background(), bugH7CtxKey{}, "sess-h7-abc")

	event := matrixEvent{
		Type:    "m.room.message",
		Sender:  "@user:example.com",
		Content: map[string]any{"msgtype": "m.text", "body": "hello"},
	}

	// 未修版本签名 handleEvent(roomID, event) — 下一行编译失败
	// 修复版本签名 handleEvent(ctx, roomID, event) — 通过且透传
	a.handleEvent(parentCtx, "!room:example.com", event)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("BUG-H7: handler 未在 2s 内被调用")
	}

	if v := capturedCtx.Value(bugH7CtxKey{}); v != "sess-h7-abc" {
		t.Fatalf("BUG-H7: handler 收到的 ctx 未保留 parent Value；got=%v want=sess-h7-abc", v)
	}
}
