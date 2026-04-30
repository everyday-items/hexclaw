// tool_lifecycle.go 实现 v0.4.0 H1 Tool Lifecycle + Hook Runtime（feature-flag gated）。
//
// 当 feature flag `tool.lifecycle.v2` 关闭（默认）时，ToolExecutor 行为与 v0.3 完全一致：
//   - Hook 按注册顺序串联
//   - After hook panic 直接传播到调用方
//   - ToolCallResult.Duration / StartedAt 不被填充
//
// 当 flag 开启时，新增能力：
//   - PrioritizedHook：声明优先级，按 priority 升序执行（同优先级保持稳定注册顺序）
//   - LifecycleTool：Tool 可选实现 Init/Shutdown，由 ToolExecutor.InitLifecycle / ShutdownLifecycle 调度
//   - After hook panic 隔离：单 hook panic 不影响其余 after hook 与最终结果返回
//   - ToolCallResult.StartedAt / Duration 自动填充供 Audit hook 观测
//
// flag 设计为 alpha：StageAlpha + Default=true（自动 fallback 到 false）—— 内部测试通过后
// 改 StageBeta + 显式 Default=true，再 GA。
package engine

import (
	"context"
	"sort"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagToolLifecycleV2 是控制 H1 新行为的 feature flag。
const FlagToolLifecycleV2 = "tool.lifecycle.v2"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagToolLifecycleV2,
		Default:      true, // alpha 阶段会被强制 OFF
		Description:  "Enable Tool Lifecycle + Hook Runtime v2 (priority sort, panic isolation, duration metrics, Init/Shutdown).",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// PrioritizedHook 是可选接口：实现该接口的 Hook 在 lifecycle.v2 启用时按 Priority
// 升序执行；同优先级按注册顺序稳定排序。Priority 数字小的先执行（before）/ 后执行（after）。
//
// 不实现该接口的 Hook 视作 priority=100。
type PrioritizedHook interface {
	Priority() int
}

const defaultHookPriority = 100

// hookPriority 取出 Hook 的优先级，未实现 PrioritizedHook 时返回默认值。
func hookPriority(h any) int {
	if ph, ok := h.(PrioritizedHook); ok {
		return ph.Priority()
	}
	return defaultHookPriority
}

// sortBeforeHooks 按 priority 升序稳定排序 before hooks。
func sortBeforeHooks(hooks []BeforeToolHook) []BeforeToolHook {
	out := make([]BeforeToolHook, len(hooks))
	copy(out, hooks)
	sort.SliceStable(out, func(i, j int) bool {
		return hookPriority(out[i]) < hookPriority(out[j])
	})
	return out
}

// sortAfterHooks 按 priority 升序稳定排序 after hooks（使用与 before 相同的语义：
// priority 小先跑——例如 Truncate(50) → Sanitize(80) → Audit(100)）。
func sortAfterHooks(hooks []AfterToolHook) []AfterToolHook {
	out := make([]AfterToolHook, len(hooks))
	copy(out, hooks)
	sort.SliceStable(out, func(i, j int) bool {
		return hookPriority(out[i]) < hookPriority(out[j])
	})
	return out
}

// LifecycleTool 是可选接口：实现该接口的 Skill 在 ToolExecutor.InitLifecycle 时被
// 调用一次 Init（用于建立连接 / 加载缓存 / 校验凭证），ShutdownLifecycle 时调用 Shutdown。
//
// 仅在 lifecycle.v2 flag 开启时生效，flag 关闭时调用是 no-op，老 Skill 完全不需修改。
type LifecycleTool interface {
	Init(ctx context.Context) error
	Shutdown(ctx context.Context) error
}
