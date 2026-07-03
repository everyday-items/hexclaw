package adapter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// bug_20260702（G）：interactive.render.v1 GA 后 MaybeApplyTextFallback 在 flag ON 下变
// no-op，注释宣称「让适配器自行调原生 renderer」。查证结论：
//
//  1. 当前生态没有任何 IM 适配器实现 PayloadRenderer 原生渲染（6 个适配器只有本文本 fallback）；
//  2. 没有任何生产路径填充 reply.Interactive —— 唯一填充点是 K12 识题确认
//     （engine.buildInteractivePayload 命中 metadata.expect_question_confirm），该触发器无
//     生产生产者接线（仅测试用，TODO E6/v0.4.0）。
//
// 故 flag ON 的 no-op 此刻不丢任何东西，**不是运行时 bug**（详见 interactive_render.go 注释）。
// 本测试锁住「注释与行为一致性」：证明一个合法的文本表示确实存在（RenderTextFallback 非空），
// 只是在 GA no-op 下被跳过——一旦有人接上 reply.Interactive 的生产生产者而适配器仍无原生
// renderer，就会静默丢弃，届时必须落地原生 renderer 或让 renderer-less 适配器继续走 fallback。
func TestBug20260702_InteractiveRenderGA_NoOpSafeOnlyBecauseNoProducer(t *testing.T) {
	payload := &InteractivePayload{
		Type:    "buttons",
		Prompt:  "继续吗？",
		Buttons: []InteractiveButton{{Label: "是", Action: "y"}, {Label: "否", Action: "n"}},
	}

	// 一个合法的纯文本表示确实存在（fallback 能渲染这个 payload）。
	if RenderTextFallback(payload) == "" {
		t.Fatal("RenderTextFallback 应能把 payload 渲染成非空文本")
	}

	// flag ON（GA 默认）：MaybeApplyTextFallback 是 no-op，Content 保持不变、payload 未被降级到文本。
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{FlagInteractiveRenderV1: true})
	ctx := featureflag.WithContext(context.Background(), flags)
	reply := &Reply{Content: "继续吗？", Interactive: payload}
	if MaybeApplyTextFallback(ctx, reply) {
		t.Fatal("flag ON 下应为 no-op（当前 GA 契约）")
	}
	if reply.Content != "继续吗？" {
		t.Fatalf("flag ON 不应改动 Content；got %q", reply.Content)
	}
	// 契约提示：payload 未被渲染成文本，也无原生 renderer 兜底——安全仅因生产从不填充 Interactive。
}
