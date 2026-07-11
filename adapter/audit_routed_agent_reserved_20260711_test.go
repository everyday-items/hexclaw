package adapter

// hex-test 审计 · R1(High 安全)：routed_agent/route_source 可被 API 客户端伪造。
// ReservedDispatchMetadataKeys 不含这两键 → StripReservedDispatchMetadata 不剥除 →
// 客户端带 provider hint 令引擎路由块跳过时，伪造的 routed_agent 原样流入
// skill.WithRoutedAgent，被 k12 grade/review 当作孩子实例 scope，绕过 agent 存在性
// 校验跨孩子写批改/错题记录。这两键只应由引擎内部路由(react.go 路由块/applyPinnedAgent)
// 写入，客户端提供即伪造。
// RED：Strip 后伪造值残留 → FAIL；GREEN：加入 reserved 列表后被剥除。

import "testing"

func TestStripReservedDispatchMetadata_StripsRoutedAgentSpoof_R1(t *testing.T) {
	m := map[string]string{
		"routed_agent": "victim-child-agent", // 客户端伪造：冒充路由到他人孩子实例
		"route_source": "pinned",             // 客户端伪造：冒充确定性锁定来源
		"provider":     "openai",             // 合法客户端 hint（不该被剥除）
	}
	StripReservedDispatchMetadata(m)

	if v, ok := m["routed_agent"]; ok {
		t.Fatalf("R1: 客户端伪造的 routed_agent 必须被剥除,实际残留 %q（可绕过 agent 存在性校验跨孩写记录）", v)
	}
	if v, ok := m["route_source"]; ok {
		t.Fatalf("R1: 客户端伪造的 route_source 必须被剥除,实际残留 %q", v)
	}
	if m["provider"] != "openai" {
		t.Fatal("provider 是合法客户端 hint,不应被误剥除")
	}
}
