package router

import (
	"testing"
)

// TestRouter_RegisterAndList 测试注册和列出 Agent
func TestRouter_RegisterAndList(t *testing.T) {
	r := New()

	err := r.Register(AgentConfig{
		Name:        "assistant",
		DisplayName: "通用助手",
		Model:       "deepseek-chat",
	})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	err = r.Register(AgentConfig{
		Name:        "coder",
		DisplayName: "代码助手",
		Model:       "gpt-4o",
	})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	agents := r.ListAgents()
	if len(agents) != 2 {
		t.Fatalf("应有 2 个 Agent，实际 %d", len(agents))
	}

	// 第一个注册的应为默认
	if r.DefaultAgent() != "assistant" {
		t.Errorf("默认 Agent 应为 assistant，实际 %q", r.DefaultAgent())
	}
}

// TestRouter_RegisterDuplicate 测试重复注册
func TestRouter_RegisterDuplicate(t *testing.T) {
	r := New()
	r.Register(AgentConfig{Name: "test"})

	err := r.Register(AgentConfig{Name: "test"})
	if err == nil {
		t.Error("重复注册应返回错误")
	}
}

// TestRouter_RegisterEmptyName 测试空名称注册
func TestRouter_RegisterEmptyName(t *testing.T) {
	r := New()
	err := r.Register(AgentConfig{})
	if err == nil {
		t.Error("空名称应返回错误")
	}
}

// TestRouter_Unregister 测试注销 Agent
func TestRouter_Unregister(t *testing.T) {
	r := New()
	r.Register(AgentConfig{Name: "a"})
	r.Register(AgentConfig{Name: "b"})
	r.AddRule(Rule{Platform: "api", AgentName: "a"})

	err := r.Unregister("a")
	if err != nil {
		t.Fatalf("注销失败: %v", err)
	}

	agents := r.ListAgents()
	if len(agents) != 1 {
		t.Errorf("注销后应剩 1 个 Agent，实际 %d", len(agents))
	}

	// 关联规则应被清除
	rules := r.ListRules()
	if len(rules) != 0 {
		t.Errorf("注销后关联规则应被清除，实际 %d", len(rules))
	}

	// 默认 Agent 应更新
	if r.DefaultAgent() != "b" {
		t.Errorf("默认 Agent 应更新为 b，实际 %q", r.DefaultAgent())
	}
}

// TestRouter_RoutePlatform 测试平台路由
func TestRouter_RoutePlatform(t *testing.T) {
	r := New()
	r.Register(AgentConfig{Name: "telegram-bot"})
	r.Register(AgentConfig{Name: "feishu-bot"})
	r.AddRule(Rule{Platform: "telegram", AgentName: "telegram-bot"})
	r.AddRule(Rule{Platform: "feishu", AgentName: "feishu-bot"})

	result := r.Route(RouteRequest{Platform: "telegram"})
	if result == nil || result.AgentName != "telegram-bot" {
		t.Errorf("telegram 应路由到 telegram-bot，实际 %v", result)
	}

	result = r.Route(RouteRequest{Platform: "feishu"})
	if result == nil || result.AgentName != "feishu-bot" {
		t.Errorf("feishu 应路由到 feishu-bot，实际 %v", result)
	}
}

// TestRouter_RouteUserID 测试用户 ID 路由
func TestRouter_RouteUserID(t *testing.T) {
	r := New()
	r.Register(AgentConfig{Name: "default"})
	r.Register(AgentConfig{Name: "vip"})
	r.AddRule(Rule{UserID: "admin", AgentName: "vip"})
	r.AddRule(Rule{Platform: "api", AgentName: "default"})

	// admin 用户应匹配 vip（UserID 优先级更高）
	result := r.Route(RouteRequest{Platform: "api", UserID: "admin"})
	if result == nil || result.AgentName != "vip" {
		t.Errorf("admin 用户应路由到 vip，实际 %v", result)
	}

	// 普通用户走平台规则
	result = r.Route(RouteRequest{Platform: "api", UserID: "user-1"})
	if result == nil || result.AgentName != "default" {
		t.Errorf("普通用户应路由到 default，实际 %v", result)
	}
}

// TestRouter_RouteDefault 测试默认路由
func TestRouter_RouteDefault(t *testing.T) {
	r := New()
	r.Register(AgentConfig{Name: "fallback"})

	// 无规则匹配时应返回默认 Agent
	result := r.Route(RouteRequest{Platform: "unknown", UserID: "nobody"})
	if result == nil || result.AgentName != "fallback" {
		t.Errorf("应返回默认 Agent，实际 %v", result)
	}
}

// TestRouter_RouteEmpty 测试空路由器
func TestRouter_RouteEmpty(t *testing.T) {
	r := New()

	result := r.Route(RouteRequest{Platform: "api"})
	if result != nil {
		t.Error("空路由器应返回 nil")
	}
}

// TestRouter_RoutePriority 测试规则优先级
func TestRouter_RoutePriority(t *testing.T) {
	r := New()
	r.Register(AgentConfig{Name: "normal"})
	r.Register(AgentConfig{Name: "priority"})

	// 两条平台规则，但 priority 更高
	r.AddRule(Rule{Platform: "api", AgentName: "normal", Priority: 1})
	r.AddRule(Rule{Platform: "api", AgentName: "priority", Priority: 10})

	result := r.Route(RouteRequest{Platform: "api"})
	if result == nil || result.AgentName != "priority" {
		t.Errorf("高优先级规则应胜出，实际 %v", result)
	}
}

// TestRouter_RouteExact 测试精确匹配（UserID + Platform）
func TestRouter_RouteExact(t *testing.T) {
	r := New()
	r.Register(AgentConfig{Name: "general"})
	r.Register(AgentConfig{Name: "specific"})

	r.AddRule(Rule{Platform: "telegram", AgentName: "general"})
	r.AddRule(Rule{Platform: "telegram", UserID: "boss", AgentName: "specific"})

	// boss 在 telegram 上应匹配精确规则
	result := r.Route(RouteRequest{Platform: "telegram", UserID: "boss"})
	if result == nil || result.AgentName != "specific" {
		t.Errorf("精确匹配应胜出，实际 %v", result)
	}

	// 其他用户在 telegram 上走平台规则
	result = r.Route(RouteRequest{Platform: "telegram", UserID: "other"})
	if result == nil || result.AgentName != "general" {
		t.Errorf("普通用户应走平台规则，实际 %v", result)
	}
}

// TestRouter_SetDefault 测试设置默认 Agent
func TestRouter_SetDefault(t *testing.T) {
	r := New()
	r.Register(AgentConfig{Name: "a"})
	r.Register(AgentConfig{Name: "b"})

	r.SetDefault("b")
	if r.DefaultAgent() != "b" {
		t.Errorf("默认 Agent 应为 b，实际 %q", r.DefaultAgent())
	}

	err := r.SetDefault("nonexistent")
	if err == nil {
		t.Error("设置不存在的 Agent 为默认应返回错误")
	}
}

// TestRouter_GetAgent 测试获取 Agent 配置
func TestRouter_GetAgent(t *testing.T) {
	r := New()
	r.Register(AgentConfig{Name: "test", Model: "gpt-4o"})

	cfg, ok := r.GetAgent("test")
	if !ok {
		t.Fatal("应能获取已注册 Agent")
	}
	if cfg.Model != "gpt-4o" {
		t.Errorf("模型不匹配: %q", cfg.Model)
	}

	_, ok = r.GetAgent("nonexistent")
	if ok {
		t.Error("获取不存在的 Agent 应返回 false")
	}
}
