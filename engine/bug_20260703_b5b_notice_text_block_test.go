package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/template"
	"github.com/hexagon-codes/hexagon"
	hruntime "github.com/hexagon-codes/hexagon/runtime"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

// BUG-20260703 B5b（B5 蒸发同构，修复链路深测抓出）：finalize 追加的守卫提示只进
// content 不进 blocks。
//
// finalizeRuntimeStreamResult 对 maxTurns / cron 幻称守卫把 notice `+=` 进 content，
// 但 done chunk 与 SaveAssistantReply 的块流都直接取 result.Blocks——blocks 里已有
// text 块时（B5 根修后为常态），客户端 blocks 优先渲染，notice 在 finalize 一刻蒸发、
// 重载后也没有（meta.blocks 无它）；只有 live 流式期间靠 streamTail 短暂可见。
//
// 契约：凡追加进 content 的尾部提示，必须同步追加为 result.Blocks 尾部 text 块——
// blocks 是「完整渲染真相」，不允许 content 与 blocks 表意分叉。
func TestBug20260703_B5b_MaxTurnsNoticeEntersBlocks(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "不应被调用"}, nil
		})
	eng := newEngineWithProviderAndSkills(t, provider, skill.NewRegistry())
	msg := &adapter.Message{ID: "m-b5b-1", SessionID: "s-b5b-1", UserID: "u-1"}

	partial := "我已经查了前两步，结果是 A 和 B"
	result := &hruntime.Result{
		Content:    partial,
		StopReason: hruntime.StopReasonMaxTurns,
		Blocks:     blocksWithTextAndTool(partial),
	}
	content, _, _, _, _ := eng.finalizeRuntimeStreamResult(
		context.Background(), "s-b5b-1", msg, provider,
		hexagon.CompletionRequest{Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: "继续"}}},
		result, "test", "mock-model", "cache-key",
		true, // maxTurnsHit
		0,
	)

	if !strings.Contains(content, maxTurnsReachedNotice) {
		t.Fatalf("前置：content 应含轮次上限提示, got %q", content)
	}
	assertBlocksTailContains(t, result, maxTurnsReachedNotice, "轮次上限提示")
}

func TestBug20260703_B5b_CronClaimNoticeEntersBlocks(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "不应被调用"}, nil
		})
	eng := newEngineWithProviderAndSkills(t, provider, skill.NewRegistry())
	msg := &adapter.Message{
		ID: "m-b5b-2", SessionID: "s-b5b-2", UserID: "u-1",
		Metadata: map[string]string{cronGuidanceActiveKey: "true"},
	}

	claim := "好的，已为您创建定时任务，每天 9 点采集科技新闻。"
	result := &hruntime.Result{
		Content: claim,
		Blocks:  blocksWithTextAndTool(claim), // 本轮无 cron_task 调用，claim 是幻称
	}
	content, _, _, _, _ := eng.finalizeRuntimeStreamResult(
		context.Background(), "s-b5b-2", msg, provider,
		hexagon.CompletionRequest{Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: "每天9点采集科技新闻"}}},
		result, "test", "mock-model", "cache-key",
		false,
		0,
	)

	if !strings.Contains(content, cronClaimGuardNotice) {
		t.Fatalf("前置：应命中幻称守卫、content 含更正提示, got %q", content)
	}
	assertBlocksTailContains(t, result, cronClaimGuardNotice, "cron 幻称更正提示")
}

// 同步路径（IM/HTTP 非流式）：finalizeReply 内的幻称守卫同样必须把提示镜像进
// 它携带的 blocks（落库与 reply 同源），调用方不再二次覆盖。
func TestBug20260703_B5b_CronClaimNoticeEntersBlocks_SyncPath(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "不应被调用"}, nil
		})
	eng := newEngineWithProviderAndSkills(t, provider, skill.NewRegistry())
	msg := &adapter.Message{
		ID: "m-b5b-3", SessionID: "s-b5b-3", UserID: "u-1",
		Metadata: map[string]string{cronGuidanceActiveKey: "true"},
	}

	claim := "已为您创建定时任务，每天 8 点执行。"
	inBlocks := []adapter.Block{{Type: "text", Text: claim}}
	reply, err := eng.finalizeReply(
		context.Background(), "s-b5b-3", msg, provider,
		hexagon.CompletionRequest{Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: "每天8点提醒我喝水"}}},
		&llm.CompletionResponse{Content: claim},
		"test", "mock-model", "cache-key",
		nil, inBlocks,
	)
	if err != nil {
		t.Fatalf("finalizeReply: %v", err)
	}
	if !strings.Contains(reply.Content, cronClaimGuardNotice) {
		t.Fatalf("前置：sync 路径应命中幻称守卫, got %q", reply.Content)
	}
	if len(reply.Blocks) == 0 {
		t.Fatalf("B5b: sync 路径 reply 应携带块流")
	}
	last := reply.Blocks[len(reply.Blocks)-1]
	if last.Type != "text" || !strings.Contains(last.Text, cronClaimGuardNotice) {
		t.Fatalf("B5b: sync 路径更正提示未进块流尾部, 末块 = %+v", last)
	}
}

// blocksWithTextAndTool 构造 B5 根修后的常态块流形状：[text, tool_use, tool_result]
// ——text 块已存在，桌面「无 text 块补 content」兜底不会触发，notice 只能靠块流自带。
func blocksWithTextAndTool(text string) template.Blocks {
	return template.NewBlockBuilder().
		Text(text).
		ToolUse("t1", "web_search", `{"q":"x"}`).
		ToolResult("t1", "结果", false, "success").
		Build()
}

func assertBlocksTailContains(t *testing.T, result *hruntime.Result, want, label string) {
	t.Helper()
	joined := ""
	for _, b := range result.Blocks {
		if b.Type == template.BlockText {
			joined += b.Text
		}
	}
	if !strings.Contains(joined, want) {
		t.Fatalf("B5b: %s 未进块流（blocks 优先渲染时该提示蒸发，重载后也没有）。text 块合并 = %q", label, joined)
	}
	last := result.Blocks[len(result.Blocks)-1]
	if last.Type != template.BlockText || !strings.Contains(last.Text, want) {
		t.Fatalf("B5b: %s 应为块流尾部 text 块, 末块 = %+v", label, last)
	}
}
