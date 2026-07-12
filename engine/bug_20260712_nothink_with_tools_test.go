package engine

import "testing"

// BUG-20260712 本地 thinking 模型 /no_think 注入被工具存在误挡回归锁。
//
// 现象：本地 qwen3.5:9b 在桌面会话里极慢（"用一句话说你好"直连 120s+）。根因：/no_think 注入
// 两处条件都以 `len(req.Tools) == 0` 为前置，而桌面会话恒挂 22 个工具 → 条件永远为假 →
// 本地 qwen3/deepseek-r1 从不注入 /no_think → 思考模式在 CPU 上生成海量推理 → 卡死。
// 修：注入不再以「无工具」为前提（/no_think 只抑制 <think>，不阻止工具调用）。
func TestShouldInjectNoThink_NotBlockedByTools(t *testing.T) {
	cases := []struct {
		name     string
		isLocal  bool
		hasTools bool
		thinking string
		model    string
		want     bool
	}{
		// ★核心：本地 qwen3 + 有工具 + 深度思考关 → 应注入（RED 旧逻辑：hasTools=true 被挡 → false）
		{"本地qwen3有工具深度思考关", true, true, "", "qwen3.5:9b", true},
		{"本地qwen3无工具", true, false, "", "qwen3.5:9b", true},
		{"本地deepseek-r1有工具", true, true, "", "deepseek-r1:8b", true},
		// 用户显式开「深度思考」→ 尊重，不注入（即便本地）
		{"显式开深度思考不注入", true, true, "on", "qwen3.5:9b", false},
		// 云端 thinking 模型 → 不注入（云端思考不慢）
		{"云端不注入", false, true, "", "qwen3.5:9b", false},
		// 非 thinking 模型 → 不注入
		{"非thinking模型不注入", true, true, "", "glm-4v-flash", false},
	}
	for _, c := range cases {
		if got := shouldInjectNoThink(c.isLocal, c.hasTools, c.thinking, c.model); got != c.want {
			t.Errorf("%s: shouldInjectNoThink(isLocal=%v,hasTools=%v,thinking=%q,model=%q)=%v want %v",
				c.name, c.isLocal, c.hasTools, c.thinking, c.model, got, c.want)
		}
	}
}
