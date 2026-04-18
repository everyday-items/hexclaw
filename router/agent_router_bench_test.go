package router

import (
	"fmt"
	"testing"

	"github.com/hexagon-codes/toolkit/util/logger"
)

// init 将 toolkit logger 级别设为 error，避免 benchmark 被 INFO 日志淹没。
func init() {
	logger.SetLevel("error")
}

// setupDispatcher 预注册 N 个 Agent 和 M 条规则，供 benchmark 使用
func setupDispatcher(b *testing.B, agents, rules int) *Dispatcher {
	b.Helper()
	r := New()
	for i := 0; i < agents; i++ {
		name := fmt.Sprintf("agent-%d", i)
		if err := r.Register(AgentConfig{Name: name, Model: "deepseek-chat"}); err != nil {
			b.Fatalf("register: %v", err)
		}
	}
	// 分摊到 platform/user/chat 三类规则
	for i := 0; i < rules; i++ {
		agentName := fmt.Sprintf("agent-%d", i%agents)
		var rule Rule
		switch i % 3 {
		case 0:
			rule = Rule{Platform: fmt.Sprintf("platform-%d", i), AgentName: agentName}
		case 1:
			rule = Rule{UserID: fmt.Sprintf("user-%d", i), AgentName: agentName}
		case 2:
			rule = Rule{ChatID: fmt.Sprintf("chat-%d", i), AgentName: agentName}
		}
		if err := r.AddRule(rule); err != nil {
			b.Fatalf("add rule: %v", err)
		}
	}
	return r
}

// BenchmarkDispatcher_RouteDefault 命中默认 Agent 的路径（无规则匹配）
func BenchmarkDispatcher_RouteDefault(b *testing.B) {
	r := setupDispatcher(b, 10, 50)
	req := RouteRequest{Platform: "unknown-platform", UserID: "unknown-user"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Route(req)
	}
}

// BenchmarkDispatcher_RouteUserMatch 命中用户规则（最高分规则）
func BenchmarkDispatcher_RouteUserMatch(b *testing.B) {
	r := setupDispatcher(b, 10, 60)
	// 从 setupDispatcher 已知: i=1 的规则是 UserID=user-1
	req := RouteRequest{UserID: "user-1"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Route(req)
	}
}

// BenchmarkDispatcher_RoutePlatformMatch 命中平台规则（较低分）
func BenchmarkDispatcher_RoutePlatformMatch(b *testing.B) {
	r := setupDispatcher(b, 10, 60)
	// i=0 的规则是 Platform=platform-0
	req := RouteRequest{Platform: "platform-0"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Route(req)
	}
}

// BenchmarkDispatcher_Register 衡量 Agent 注册成本
func BenchmarkDispatcher_Register(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		r := New()
		b.StartTimer()
		_ = r.Register(AgentConfig{Name: fmt.Sprintf("agent-%d", i), Model: "deepseek-chat"})
	}
}

// BenchmarkDispatcher_RouteHighRuleCount 路由在 500 条规则下的 O(N) 扫描成本
func BenchmarkDispatcher_RouteHighRuleCount(b *testing.B) {
	r := setupDispatcher(b, 20, 500)
	req := RouteRequest{UserID: "user-499"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Route(req)
	}
}
