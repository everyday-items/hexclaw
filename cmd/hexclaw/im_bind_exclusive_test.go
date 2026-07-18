package main

import (
	"context"
	"strings"
	"testing"

	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

// 限绑契约（架构设计-v0.5.0 §3.12，2026-07-18 外部评审裁决）：同一私聊目标同一时间
// 只能绑定一个 TutorAgent——妈妈的同一个钉钉号不能同时是两个孩子助手的接收人，
// 否则入站照片归属无解（卷面号仅 Learner 内唯一救不了绑定歧义）。
//   - 已绑其他实例 → 拒绝并给可读错误（明示原因，引导先解绑）；
//   - 同实例重复绑定 → 幂等成功；
//   - 不同私聊目标绑不同实例 → 各绑各的，互不影响。
func TestIMBindExclusive_OneTutorPerChatTarget(t *testing.T) {
	ctx := context.Background()
	binder, dispatcher, store, _ := newIMBinderFixture(t)

	if err := binder.Bind(ctx, "dingtalk", "bot-1", "mom-chat", "child-a"); err != nil {
		t.Fatal(err)
	}
	// 同一私聊目标绑第二个孩子：拒绝 + 可读错误（含已绑实例名与解绑引导）。
	err := binder.Bind(ctx, "dingtalk", "bot-1", "mom-chat", "child-b")
	if err == nil {
		t.Fatal("同一私聊目标绑第二个 TutorAgent 必须被拒绝（§3.12 限绑）")
	}
	if !strings.Contains(err.Error(), "child-a") || !strings.Contains(err.Error(), "解绑") {
		t.Errorf("拒绝理由应明示已绑实例并引导先解绑, got %v", err)
	}
	// 拒绝后路由与持久化都保持原绑定。
	got := dispatcher.Route(agentrouter.RouteRequest{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "mom-chat"})
	if got == nil || got.Rule == nil || got.AgentName != "child-a" {
		t.Fatalf("拒绝后路由必须保持 child-a, got=%+v", got)
	}
	rules, err2 := store.LoadRules(ctx)
	if err2 != nil {
		t.Fatal(err2)
	}
	if len(rules) != 1 || rules[0].AgentName != "child-a" {
		t.Fatalf("拒绝后持久化必须保持单条 child-a 规则, got=%+v", rules)
	}

	// 同实例重复绑定：幂等成功，不产生重复规则。
	if err := binder.Bind(ctx, "dingtalk", "bot-1", "mom-chat", "child-a"); err != nil {
		t.Fatalf("同实例重复绑定应幂等成功, got %v", err)
	}
	if rules, _ := store.LoadRules(ctx); len(rules) != 1 {
		t.Fatalf("幂等重绑不得新增规则, got=%+v", rules)
	}

	// 不同私聊目标（爸爸的号）绑另一个孩子：合法。
	if err := binder.Bind(ctx, "dingtalk", "bot-1", "dad-chat", "child-b"); err != nil {
		t.Fatalf("不同私聊目标绑不同实例应成功, got %v", err)
	}
}
