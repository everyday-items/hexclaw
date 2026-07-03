package engine

// GO-9 修复核验（hex-test 对抗审查暴露）：原 GO-9 只改了 SystemDispatchPolicy 矩阵，
// 但矩阵在**生产策略 DefaultBaselinePolicy 下根本不被 cron_task 触及**——cron_task 是
// builtin(Source="skill" 非 mcp)、DefaultBaselinePolicy 无其规则 → policy.Evaluate 返
// ActionAllow → gateUnattendedConnectorTool 对非 mcp 工具早退 return nil → 自动执行，
// 矩阵形同虚设。旧 GO-9 测试全用 `*→ActionRequireApproval` 测试策略强制走 requestApproval
// 才碰到矩阵，与生产不符（测试策略≠生产策略陷阱）。
//
// 本测试用**生产同款 DefaultBaselinePolicy**，钉死真实执行路径：cron 源自建 cron 必须
// 转审批/拒绝，workflow 编排源仍放行，交互(无派发源)不受影响。

import (
	"context"
	"testing"
)

func TestBug20260703_CronTaskGatedUnderProductionPolicy(t *testing.T) {
	// 生产装配：DefaultBaselinePolicy（main.go 注入的同一个）。
	hook := NewPermissionHook(nil, WithPolicy(DefaultBaselinePolicy()))

	// cron 派发源自建 cron = 自我复制回路，必须被拦（矩阵不放行 automation）。
	if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), "cron"),
		&ToolCallInfo{Name: "cron_task", Source: "skill"}); err == nil {
		t.Error("[GO-9 生产路径] cron 源自动执行了 cron_task（自排程回路未闭合，矩阵在生产策略下被绕过）")
	}

	// webhook / heartbeat / spawn 同样不应自动放行 cron_task。
	for _, src := range []string{"webhook", "heartbeat", "spawn"} {
		if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), src),
			&ToolCallInfo{Name: "cron_task", Source: "skill"}); err == nil {
			t.Errorf("[GO-9 生产路径] %s 源自动执行了 cron_task", src)
		}
	}

	// workflow 编排源（用户显式编排，非自我复制）仍放行。
	if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), "workflow"),
		&ToolCallInfo{Name: "cron_task", Source: "skill"}); err != nil {
		t.Errorf("[GO-9 生产路径] workflow 编排源应放行 cron_task，实际被拦：%v", err)
	}
}

// 回归守护：非 automation 的 builtin 工具在生产策略下的无人值守行为不被本次收口误伤。
// read 类（knowledge_ingest 等）在 cron 源仍自动放行（矩阵允许 read）。
func TestBug20260703_CronTaskGateDoesNotOverBlockReadBuiltins(t *testing.T) {
	hook := NewPermissionHook(nil, WithPolicy(DefaultBaselinePolicy()))
	for _, tool := range []string{"knowledge_ingest", "search", "web_search"} {
		if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), "cron"),
			&ToolCallInfo{Name: tool, Source: "skill"}); err != nil {
			t.Errorf("read 类 builtin %q 在 cron 源应自动放行（矩阵允许 read），实际被拦：%v", tool, err)
		}
	}
}

// 交互会话（无系统派发源）：cron_task 保持自动放行手感，不因本次收口新增审批摩擦。
func TestBug20260703_CronTaskInteractiveUnaffected(t *testing.T) {
	hook := NewPermissionHook(nil, WithPolicy(DefaultBaselinePolicy()))
	// 无 withSystemDispatch → 无派发源 → 交互路径。
	if err := hook.BeforeToolCall(context.Background(),
		&ToolCallInfo{Name: "cron_task", Source: "skill"}); err != nil {
		t.Errorf("交互会话 cron_task 应自动放行（无派发源），实际被拦：%v", err)
	}
}
