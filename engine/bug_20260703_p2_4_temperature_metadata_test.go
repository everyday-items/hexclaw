package engine

// BUG-20260703 P2-4（engine 半边）：applyAgentConfigToMetadata 的温度下发语义。
// 旧实现 `if cfg.Temperature > 0` 把显式 0 当未设吞掉——温度 0（确定性采样）
// 永远无法从 Agent 配置传到 CompletionRequest（下游本就是 *float64 指针）。

import (
	"testing"

	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

func TestBug20260703P24_TemperatureMetadataTriState(t *testing.T) {
	cases := []struct {
		name    string
		temp    *float64
		want    string
		present bool
	}{
		{name: "nil 未设不下发", temp: nil, present: false},
		{name: "显式 0 如实下发确定性采样", temp: f64(0), want: "0.00", present: true},
		{name: "常规值照常下发", temp: f64(0.7), want: "0.70", present: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metadata := map[string]string{}
			cfg := &agentrouter.AgentConfig{Name: "a", Temperature: tc.temp}
			applyAgentConfigToMetadata(metadata, cfg, "")
			got, ok := metadata["agent_temperature"]
			if ok != tc.present {
				t.Fatalf("agent_temperature 存在性期望 %v，实际 %v (%q)", tc.present, ok, got)
			}
			if tc.present && got != tc.want {
				t.Fatalf("期望 %q，实际 %q", tc.want, got)
			}
		})
	}
}

func f64(v float64) *float64 { return &v }
