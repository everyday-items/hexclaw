package adapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func TestRenderTextFallback_Buttons(t *testing.T) {
	out := RenderTextFallback(&InteractivePayload{
		Type:    "buttons",
		Prompt:  "是这道题吗？",
		Buttons: []InteractiveButton{{Label: "是", Action: "y"}, {Label: "不是", Action: "n"}},
	})
	if !strings.Contains(out, "是这道题吗") || !strings.Contains(out, "1) 是") || !strings.Contains(out, "2) 不是") {
		t.Errorf("buttons fallback 不对；got %q", out)
	}
}

func TestRenderTextFallback_Select(t *testing.T) {
	out := RenderTextFallback(&InteractivePayload{
		Type:    "select",
		Options: []InteractiveOption{{Label: "A", Value: "a", Description: "选项 A"}, {Label: "B", Value: "b"}},
	})
	if !strings.Contains(out, "1) A") || !strings.Contains(out, "选项 A") {
		t.Errorf("select fallback 不对；got %q", out)
	}
}

func TestRenderTextFallback_Approval(t *testing.T) {
	out := RenderTextFallback(&InteractivePayload{
		Type:     "approval",
		Approval: &InteractiveApproval{Subject: "删除 12 道错题", Summary: "确认删除"},
	})
	if !strings.Contains(out, "删除 12 道错题") || !strings.Contains(out, "1) 同意") {
		t.Errorf("approval fallback 不对；got %q", out)
	}
}

func TestRenderTextFallback_Card(t *testing.T) {
	out := RenderTextFallback(&InteractivePayload{
		Type: "card",
		Card: &InteractiveCard{
			Title:   "今日概况",
			Fields:  []CardField{{Label: "做题", Value: "12"}},
			Buttons: []InteractiveButton{{Label: "查看", Action: "view"}},
		},
	})
	if !strings.Contains(out, "今日概况") || !strings.Contains(out, "做题: 12") || !strings.Contains(out, "1) 查看") {
		t.Errorf("card fallback 不对；got %q", out)
	}
}

func TestRenderTextFallback_Nil(t *testing.T) {
	if RenderTextFallback(nil) != "" {
		t.Error("nil 应返回空")
	}
}

func TestShouldUseNativeRenderer_FlagOff(t *testing.T) {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{FlagInteractiveRenderV1: false})
	if ShouldUseNativeRenderer(flags) {
		t.Error("flag OFF 应返回 false")
	}
}

func TestShouldUseNativeRenderer_FlagOn(t *testing.T) {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{FlagInteractiveRenderV1: true})
	if !ShouldUseNativeRenderer(flags) {
		t.Error("flag ON 应返回 true")
	}
}

func TestShouldUseNativeRenderer_NilFlags(t *testing.T) {
	if ShouldUseNativeRenderer(nil) {
		t.Error("nil flags 应返回 false")
	}
}

// v0.4.0 F6: MaybeApplyTextFallback 主链路接入
func TestMaybeApplyTextFallback_FlagOff_AppendsFallback(t *testing.T) {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{FlagInteractiveRenderV1: false})
	ctx := featureflag.WithContext(context.Background(), flags)

	reply := &Reply{
		Content: "继续吗？",
		Interactive: &InteractivePayload{
			Type:    "buttons",
			Buttons: []InteractiveButton{{Label: "是", Action: "y"}, {Label: "否", Action: "n"}},
		},
	}
	applied := MaybeApplyTextFallback(ctx, reply)
	if !applied {
		t.Fatal("flag OFF 应触发 fallback 追加")
	}
	if !strings.Contains(reply.Content, "1) 是") || !strings.Contains(reply.Content, "2) 否") {
		t.Errorf("Content 应含按钮文本；got %q", reply.Content)
	}
	if !strings.HasPrefix(reply.Content, "继续吗？") {
		t.Errorf("原 Content 应保留在前；got %q", reply.Content)
	}
	// Interactive 字段不应被清除
	if reply.Interactive == nil {
		t.Error("Interactive 字段应保留以供上层观察 / persistence")
	}
}

func TestMaybeApplyTextFallback_FlagOn_NoOp(t *testing.T) {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{FlagInteractiveRenderV1: true})
	ctx := featureflag.WithContext(context.Background(), flags)

	original := "继续吗？"
	reply := &Reply{
		Content: original,
		Interactive: &InteractivePayload{
			Type:    "buttons",
			Buttons: []InteractiveButton{{Label: "是", Action: "y"}},
		},
	}
	applied := MaybeApplyTextFallback(ctx, reply)
	if applied {
		t.Error("flag ON 应跳过 fallback，让适配器走原生 renderer")
	}
	if reply.Content != original {
		t.Errorf("flag ON 不应修改 Content；got %q", reply.Content)
	}
}

func TestMaybeApplyTextFallback_NoInteractive_NoOp(t *testing.T) {
	reply := &Reply{Content: "纯文本回复"}
	if MaybeApplyTextFallback(context.Background(), reply) {
		t.Error("无 Interactive 应直接返回 false")
	}
	if reply.Content != "纯文本回复" {
		t.Errorf("Content 不应被修改；got %q", reply.Content)
	}
}

func TestMaybeApplyTextFallback_NilReply_NoOp(t *testing.T) {
	if MaybeApplyTextFallback(context.Background(), nil) {
		t.Error("nil reply 应返回 false")
	}
}

func TestMaybeApplyTextFallback_EmptyContent_NoExtraNewlines(t *testing.T) {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{FlagInteractiveRenderV1: false})
	ctx := featureflag.WithContext(context.Background(), flags)

	reply := &Reply{
		Interactive: &InteractivePayload{
			Type:    "buttons",
			Buttons: []InteractiveButton{{Label: "OK", Action: "ok"}},
		},
	}
	MaybeApplyTextFallback(ctx, reply)
	if strings.HasPrefix(reply.Content, "\n") {
		t.Errorf("空 Content 应直接放 fallback，不留前导 \\n；got %q", reply.Content)
	}
}
