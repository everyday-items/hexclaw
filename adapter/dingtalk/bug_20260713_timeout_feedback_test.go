package dingtalk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// BUG-20260713：钉钉拍照解题在 handler 超时（多图视觉请求 > 2 分钟)后，「⌨️ 已收到，正在思考…」
// 占位永久残留、且用户收不到任何错误提示。
//
// 根因(dingtalk.go handleMessageContext)：
//
//	① handler 失败且 ctx.Err()!=nil（超时正是此情形)时**提前 return**，跳过错误回复与占位撤回；
//	② 即便发送，errCtx 也派生自已过期的 handler ctx → 在发送队列 admission（send_queue.go 的
//	   ctx.Err() 检查)被静默拒绝。二者叠加，占位永远停在「正在思考」。
//
// 契约：占位是必须兑现的承诺——handler 超时失败时仍必须 (a) 发出错误提示、(b) 撤回占位，
// 且二者用独立于 handler ctx 的兜底 ctx 执行（对齐 send_queue_test.go 的 FIX 模式)。
func TestDingtalkTimeoutStillNotifiesUser_BUG20260713(t *testing.T) {
	a := newTestAdapter()
	// 缩短处理预算，复现「占位已发 → handler 耗尽预算 → 返回 error(ctx 已 DeadlineExceeded)」，
	// 无需真等 2 分钟。
	a.handlerTimeout = 40 * time.Millisecond
	a.handler = func(ctx context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		<-ctx.Done()          // 耗尽 handler 预算，模拟视觉请求超时
		return nil, ctx.Err() // 返回 error，且此刻 ctx 已过期（DeadlineExceeded）
	}
	fakeAPI := newFakeDingtalkOpenAPI("test-token")
	a.openAPI = fakeAPI

	event := dtEvent{SenderStaffId: "user-hw", SenderNick: "娃"}
	event.Text.Content = "这道题怎么做"

	a.handleMessageContext(context.Background(), event)

	// ① 占位之外必须再发一条错误提示。旧代码：超时分支提前 return（0 条错误提示）/ 过期 ctx 被
	// 发送队列拒绝（错误提示发不出）。
	calls := fakeAPI.SendCalls()
	if len(calls) < 2 {
		t.Fatalf("handler 超时失败后，除占位外应再发一条错误提示，实际发送 %d 条: %+v", len(calls), calls)
	}
	last := calls[len(calls)-1].Text
	if !strings.Contains(last, "出现错误") && !strings.Contains(last, "重试") {
		t.Fatalf("最后一条应为错误提示，实际 %q", last)
	}

	// ② 占位必须被撤回（否则钉钉永久停在「正在思考」）。
	if len(fakeAPI.RecallCalls()) == 0 {
		t.Fatal("handler 超时失败后占位未被撤回 → 钉钉永久残留「⌨️ 已收到，正在思考…」")
	}
}

// 回归守卫：成功路径不受重构影响——仍是「占位 + 答案」两条发送 + 撤回占位。
func TestDingtalkSuccessStillSendsAndRecalls_BUG20260713(t *testing.T) {
	a := newTestAdapter()
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "96 cm³"}, nil
	}
	fakeAPI := newFakeDingtalkOpenAPI("test-token")
	a.openAPI = fakeAPI

	event := dtEvent{SenderStaffId: "user-hw", SenderNick: "娃"}
	event.Text.Content = "这道题怎么做"

	a.handleMessageContext(context.Background(), event)

	calls := fakeAPI.SendCalls()
	if len(calls) != 2 {
		t.Fatalf("成功路径应为占位 + 答案共 2 条，实际 %d: %+v", len(calls), calls)
	}
	if !strings.Contains(calls[1].Text, "96 cm³") {
		t.Fatalf("第二条应为答案，实际 %q", calls[1].Text)
	}
	if len(fakeAPI.RecallCalls()) == 0 {
		t.Fatal("答案送达后应撤回占位")
	}
}
