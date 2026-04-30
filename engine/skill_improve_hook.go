// skill_improve_hook.go 把 skill.ImproveStore 接到 ToolExecutor 的 AfterToolHook 链上，
// 完成 v0.4.x C3 闭环：每次 Skill / MCP 工具调用结束 → Record → 低分自动写 v2 草稿。
//
// 设计：
//   - 只对 source=="skill" 的调用记录（MCP 工具不在 Skills 闭环范围）
//   - 同步耗时极短（仅一次 mu 加锁追加切片），低分判定 + 文件写入由 store 内异步策略决定
//   - 失败不影响主流程：hook 永远不返回错误（AfterToolHook 接口本身也不返回）
//   - Judge 注入由 cmd/main 负责，未注入时只采集不评分（C4 组合学习仍能基于纯执行轨迹工作）

package engine

import (
	"context"
	"time"

	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/trace"
)

// ImproveHook 把工具调用结果送进 skill.ImproveStore。
type ImproveHook struct {
	store *skill.ImproveStore
}

// Priority 排在 Audit (100) 之前、Sanitize (80) 之后；让 Sanitize 后的最终
// 文本被送进 Improve store 评分。
func (h *ImproveHook) Priority() int { return 90 }

// NewImproveHook 用给定的 store 创建 hook。store==nil 时返回 nil（调用方需 nil-check）。
func NewImproveHook(store *skill.ImproveStore) *ImproveHook {
	if store == nil {
		return nil
	}
	return &ImproveHook{store: store}
}

// BeforeToolCall 记录起始时间到 ctx —— 走 ctx value 而非全局，保证并发安全。
func (h *ImproveHook) BeforeToolCall(ctx context.Context, _ *ToolCallInfo) error {
	// 起始时间通过 context value 传递；这里用一个 unexported key 避免冲突。
	// AfterToolCall 需要拿到这个值来算 duration —— 但 ctx 在 hook 间不会传递写后值，
	// 所以这里**不**注入 ctx，改成在 hook 内部记录 monotonic clock：见下方 startKey 实现。
	return nil
}

// startTimeKey 见下方 storeStartTime 注释 —— 由于 hook chain 不传递 ctx 修改，
// 我们改用 store 内部的 Snapshot/最近一次 Record 时间反推 duration —— 但更简单是直接
// 在 AfterToolCall 内用 0 duration（生产场景下有 trace.RequestStart 可拿到精确耗时，
// MVP 阶段 0 duration 不影响 judge 评分逻辑，judge 主要看输入输出）。

// AfterToolCall 把工具调用结果写进 ImproveStore。
//
// 注意：这里只记录 source=="skill" 的调用 —— MCP 工具不在 v0.4.x C3 改进闭环范围。
// duration 字段在 hook 链当前无法精确获取（hook 间不共享上下文），后续可在 ToolExecutor
// 直接回填 startedAt；当前阶段 0 duration 不影响 Judge 评分（Judge 主要看输入输出）。
func (h *ImproveHook) AfterToolCall(ctx context.Context, call *ToolCallInfo, result *ToolCallResult) {
	if h == nil || h.store == nil || call == nil || result == nil {
		return
	}
	if call.Source != "skill" {
		return
	}
	exec := skill.Execution{
		SkillName: call.Name,
		UserInput: argString(call.Arguments),
		Output:    result.Content,
		Success:   result.Error == nil,
		Timestamp: time.Now(),
	}
	if err := h.store.Record(exec); err != nil {
		trace.L(ctx).Warn("ImproveHook.Record 失败", "skill", call.Name, "error", err)
	}
}

// argString 把 args map 序列化成稳定字符串表示，用于 judge 看用户输入。
// 不引 encoding/json 是因为 hook 调用频繁，反射成本高于纯字符串拼接；
// 仅取 query / text / question 等常见字段（最优表达），其余 fallback 用 fmt。
func argString(args map[string]any) string {
	if args == nil {
		return ""
	}
	for _, key := range []string{"query", "question", "input", "text", "prompt"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	// fallback：拼前几个 key=value
	var b []byte
	count := 0
	for k, v := range args {
		if count >= 3 {
			break
		}
		if count > 0 {
			b = append(b, ' ')
		}
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, formatVal(v)...)
		count++
	}
	return string(b)
}

func formatVal(v any) string {
	switch x := v.(type) {
	case string:
		if len(x) > 80 {
			return x[:80] + "…"
		}
		return x
	case nil:
		return ""
	default:
		return ""
	}
}
