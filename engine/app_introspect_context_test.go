package engine

import (
	"context"
	"strings"
	"testing"
)

// fakeIntrospector 是 buildCapabilityContext 注入测试用的可控自感知实现。
type fakeIntrospector struct {
	inv AppInventory
}

func (f *fakeIntrospector) Inventory(_ context.Context, _ string) AppInventory { return f.inv }
func (f *fakeIntrospector) Query(_ context.Context, _, _, _, _ string) (string, error) {
	return "", nil
}

// #9：[本应用现状] 注入块此前零测试。钉死：版本、基数行、app_query 指引都出现。
// 同时覆盖 #4：有"工具型资源"（连接/cron…）时才注入"切 tool-capable 模型"提示。
func TestBuildCapabilityContext_AppCard_Injected(t *testing.T) {
	e := &ReActEngine{}
	e.SetAppIntrospector(&fakeIntrospector{inv: AppInventory{
		Version: "v0.4.6",
		Counts:  map[string]int{"agents": 2, "connections": 3, "cron": 1},
	}})

	out := e.buildCapabilityContext(context.Background(), map[string]string{"user_id": "u1"})

	if !strings.Contains(out, "[本应用现状]") {
		t.Fatalf("应注入 [本应用现状] 块: %q", out)
	}
	if !strings.Contains(out, "v0.4.6") {
		t.Fatalf("应含版本: %q", out)
	}
	for _, want := range []string{"3 个连接", "1 个定时任务", "2 个Agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("基数行应含 %q: %q", want, out)
		}
	}
	if !strings.Contains(out, "app_query") {
		t.Fatalf("应引导用 app_query 按需读取: %q", out)
	}
	// #4：有连接/cron（工具型资源 > 0）→ 提示出现
	if !strings.Contains(out, "tool-capable") {
		t.Fatalf("有工具型资源时应提示 tool-capable 模型: %q", out)
	}
}

// #4：纯聊天用户（只有 Agent，无 MCP/连接/cron/webhook/工作流）→ 不应注入"切 tool-capable 模型"噪声。
func TestBuildCapabilityContext_AdvisoryConditional_NoToolResources(t *testing.T) {
	e := &ReActEngine{}
	e.SetAppIntrospector(&fakeIntrospector{inv: AppInventory{
		Version: "v0.4.6",
		Counts:  map[string]int{"agents": 1}, // 仅 Agent，无任何工具型资源
	}})

	out := e.buildCapabilityContext(context.Background(), nil)

	if !strings.Contains(out, "2 个Agent") && !strings.Contains(out, "1 个Agent") {
		t.Fatalf("基数行仍应出现: %q", out)
	}
	if strings.Contains(out, "tool-capable") {
		t.Fatalf("无工具型资源时不应注入 tool-capable 提示（噪声）: %q", out)
	}
}

// 未注入 introspector 时不应出现 [本应用现状]（优雅降级）。
func TestBuildCapabilityContext_NoIntrospector_NoCard(t *testing.T) {
	e := &ReActEngine{}
	out := e.buildCapabilityContext(context.Background(), nil)
	if strings.Contains(out, "[本应用现状]") {
		t.Fatalf("未注入 introspector 不应出现系统名片: %q", out)
	}
}
