package router

import "testing"

// IM 频道默认模型绑定的路由/解析契约（hex-test 必跑，跑在 router 包 `go test`）。
//
// 锁 item1/item3 的命脉链：桌面端「频道默认模型」用匿名 Agent `@im/<platform>` 承载，
// 经平台级路由规则解析 → 该 Agent 的 model 被采纳 → 更精确规则优先 → 名字含 `/` 仍可路由
// （真机已验证 @im/ 名后端 path 路由 OK，此处锁内核路由解析层防回退）。
func TestChannelDefaultModelRouting(t *testing.T) {
	const M = "Qwen/Qwen3.6-35B-A3B"
	d := New()

	// item1：注册匿名频道默认 Agent（名含 `/`）+ 平台级规则
	if err := d.Register(AgentConfig{Name: "@im/feishu", Model: M, Provider: "siliconflow"}); err != nil {
		t.Fatalf("Register @im/feishu: %v", err)
	}
	if err := d.AddRule(Rule{Platform: "feishu", AgentName: "@im/feishu", Priority: 0}); err != nil {
		t.Fatalf("AddRule feishu: %v", err)
	}

	// item1 路由 + item3 模型解析：platform=feishu → @im/feishu，且其绑定 model 被解析出
	r := d.Route(RouteRequest{Platform: "feishu"})
	if r == nil || r.AgentName != "@im/feishu" {
		t.Fatalf("feishu 应路由到 @im/feishu，得 %+v", r)
	}
	if r.AgentConfig == nil || r.AgentConfig.Model != M {
		t.Fatalf("绑定模型应被解析为 %q，得 %+v", M, r.AgentConfig)
	}

	// 优先级链：更精确的 chat 级规则胜过平台级
	if err := d.Register(AgentConfig{Name: "vip", Model: M, Provider: "siliconflow"}); err != nil {
		t.Fatalf("Register vip: %v", err)
	}
	if err := d.AddRule(Rule{Platform: "feishu", ChatID: "oc_vip", AgentName: "vip", Priority: 10}); err != nil {
		t.Fatalf("AddRule vip: %v", err)
	}
	if r := d.Route(RouteRequest{Platform: "feishu", ChatID: "oc_vip"}); r == nil || r.AgentName != "vip" {
		t.Fatalf("feishu+oc_vip 应路由到 vip（更精确规则胜），得 %+v", r)
	}
	if r := d.Route(RouteRequest{Platform: "feishu", ChatID: "other"}); r == nil || r.AgentName != "@im/feishu" {
		t.Fatalf("feishu+其它chat 应回落平台级 @im/feishu，得 %+v", r)
	}
}
