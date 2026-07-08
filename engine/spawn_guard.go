package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
)

// errFanoutBudgetExceeded 标记一个子任务因触及跨树总量闸而被拒派（非真错，供完成度统计区分）。
var errFanoutBudgetExceeded = errors.New("fan-out 总量已达上限，未派发该子任务")

// 多 Agent 拆分协作的递归 / 扇出安全护栏。
//
// spawn_agent / orchestrate 让一个 Agent 派生子 Agent。没有护栏时：
//   - 子 Agent 仍持有 spawn/orchestrate 工具 → 父→子→孙→… 可无限递归；
//   - orchestrate 裸 goroutine fan-out → 子任务数 = goroutine 数，可瞬时打爆。
// 本文件提供两道护栏，对齐 OpenClaw（main/orchestrator/leaf 角色 + maxSpawnDepth）
// 与 Hermes（delegation.max_spawn_depth + max_concurrent_children）的工业实践：
//  1. 深度上限：到顶后 spawn/orchestrate 优雅拒绝（返回提示而非派生）。
//  2. leaf 剥工具：到顶的子 Agent 根本看不到 spawn/orchestrate/transfer 工具。

// maxSpawnDepth 限制 Agent 派生的递归深度。depth 0 = 顶层（用户）请求。默认 2 启用三级角色：
// main(0) → orchestrator(1，仍可派生) → leaf(2，到顶被剥夺派生)——对齐 OpenClaw 的角色层级。
// 用 var 而非 const，便于测试收紧或放宽。
var maxSpawnDepth = 2

// 子 Agent 角色层级常量（对齐 OpenClaw main/orchestrator/leaf）。
const (
	subAgentRoleMain         = "main"
	subAgentRoleOrchestrator = "orchestrator"
	subAgentRoleLeaf         = "leaf"
)

// roleForDepth 按派生深度判定角色层级：
//
//	depth <= 0              → main          顶层用户请求
//	1 .. maxSpawnDepth-1    → orchestrator  仍可继续派生子 Agent
//	>= maxSpawnDepth        → leaf          到顶，被剥夺派生能力（工具被剥）
func roleForDepth(depth int) string {
	switch {
	case depth <= 0:
		return subAgentRoleMain
	case depth >= maxSpawnDepth:
		return subAgentRoleLeaf
	default:
		return subAgentRoleOrchestrator
	}
}

// maxOrchestrateConcurrency 限制单次 orchestrate 并行执行的子 Agent 数量上限。
// 超出的子任务排队，空出一个槽再跑——把「子任务数 = goroutine 数」收敛为有界并发。
//
// ★自适应轴 = LLM 后端，非 CPU 核心：HexClaw 的子 Agent 干的是 LLM 调用（瓶颈在 provider
// 的 rate-limit/成本 或本地模型吞吐），不是本地计算。所以不像 Claude Code 那样按 CPU 核心扩容，
// 而是由 cmd/hexclaw 启动时按默认 provider 的本地/云端属性调 SetMaxOrchestrateConcurrency：
// 本地模型同机扛不住多并发推理 → 调低(2)；云端 rate-limit 较宽 → 放到 8。
var maxOrchestrateConcurrency = 8

// SetMaxOrchestrateConcurrency 设定 orchestrate 并发上限（clamp 到 [1,16]）。
func SetMaxOrchestrateConcurrency(n int) {
	switch {
	case n < 1:
		n = 1
	case n > 16:
		n = 16
	}
	maxOrchestrateConcurrency = n
}

// maxChildrenPerAgent 限制单次 orchestrate 拆分的子任务总数（对齐 OpenClaw maxChildrenPerAgent）。
// 超出截断——并发闸只挡「瞬时打爆」，数量闸挡「总量失控」：depth=2 启用 orchestrator 中间层后，
// 树是 N×M 笛卡尔积，没有数量闸时 LLM 每层拆 20 个就是 400 次真模型调用 + token 烧穿。
var maxChildrenPerAgent = 8

// SetMaxChildrenPerAgent 设定单次拆分上限（>=1 生效）。
func SetMaxChildrenPerAgent(n int) {
	if n >= 1 {
		maxChildrenPerAgent = n
	}
}

// ───────────────────── 跨树「总量闸」(对齐 Claude Code 1000 硬总量 backstop) ─────────────────────
//
// 深度闸(maxSpawnDepth) + 并发闸(maxOrchestrateConcurrency) + 单次数量闸(maxChildrenPerAgent)
// 各自只钳住「一层 / 一瞬间 / 一次拆分」。但 orchestrate 会 **跨深度 × 跨监工轮** 组合扇出：
// depth0 派 8 → 每个 depth1 再 orchestrate 8 = 72 子；× 监工多轮 ≈ 数百次 LLM 调用 / 单次工具调用。
// 删了成本闸后，唯一剩的 orchestrateMaxWall=5min 只兜「时间」不兜「数量」——云端快模型
// 5 分钟能打数百次。本闸是纯计数 backstop（不记账成本，符合「成本低优先」），跨整棵派生树累加，
// 给组合爆炸封一个硬顶。计数器挂在 orchestrate/spawn 根 ctx 上，经 executeFunc→Process 透传给
// 所有后代（同一指针），任意深度 / 任意轮的新派发都从同一预算扣减，超限即拒。

// maxTotalFanout 是单次顶层 orchestrate/spawn 调用下、整棵派生树跨深度跨轮可派发的子 Agent 总数硬顶。
// 默认 128：足够容纳一棵完整嵌套树(8 + 8×8 = 72)加余量，又把多轮 × 嵌套的组合爆炸(216+)钳在数百级以下，
// 比 Claude Code 的 1000 backstop 更贴合单用户桌面规模。用 var 便于按云/本地后端调参与测试收紧。
var maxTotalFanout int64 = 128

// SetMaxTotalFanout 设定跨树总量闸（>=1 生效）。云端 rate-limit 宽可调高，本地推理慢可调低。
func SetMaxTotalFanout(n int64) {
	if n >= 1 {
		atomic.StoreInt64(&maxTotalFanout, n)
	}
}

// fanoutBudget 是一棵派生树共享的总量预算：一个原子计数器 + 上限。根 ctx 持有它，后代经 ctx 共享
// 同一指针，故任意深度/轮的派发都对同一计数器做 CAS 扣减——精确钳在 max，不会因竞态过冲。
type fanoutBudget struct {
	n   atomic.Int64
	max int64
}

// tryAcquire 原子占用一个派发名额：未超上限则 +1 返回 true，已达上限返回 false（不再 +1，精确封顶）。
func (b *fanoutBudget) tryAcquire() bool {
	if b == nil {
		return true // 未注入预算（nil-safe）：不限，零侵入降级。
	}
	for {
		cur := b.n.Load()
		if cur >= b.max {
			return false
		}
		if b.n.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

type ctxKeyFanoutBudgetType struct{}

var ctxKeyFanoutBudget = ctxKeyFanoutBudgetType{}

// withFanoutBudget 在 ctx 上挂一个总量预算：**仅当 ctx 尚无预算时铸新**（顶层 orchestrate/spawn 即根，
// 铸一个；后代 ctx 已携带根预算 → 原样返回，复用同一指针）。这样整棵树共享一个计数器，跨深度跨轮累加。
func withFanoutBudget(ctx context.Context) context.Context {
	if fanoutBudgetFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyFanoutBudget, &fanoutBudget{max: atomic.LoadInt64(&maxTotalFanout)})
}

// fanoutBudgetFromContext 读取 ctx 上的总量预算（未设则 nil）。
func fanoutBudgetFromContext(ctx context.Context) *fanoutBudget {
	if ctx == nil {
		return nil
	}
	if b, ok := ctx.Value(ctxKeyFanoutBudget).(*fanoutBudget); ok {
		return b
	}
	return nil
}

// fanoutBudgetExceededMessage 在派发触顶时作为正常工具结果返回（非 error），让 LLM 据此自己收手。
func fanoutBudgetExceededMessage(tool string) string {
	return fmt.Sprintf(
		"%s 不可用：本次任务的子 Agent 总量已达上限 (max=%d)。请不要再派生 / 编排更多子 Agent，直接用已有结果自行完成。",
		tool, atomic.LoadInt64(&maxTotalFanout))
}

// multiAgentRecursiveTools 是子 Agent 到达深度上限后必须剔除的编排工具：放任子 Agent
// 再 spawn / orchestrate / handoff 是无限递归与 fan-out 爆炸的入口。名字是真实的
// ToolDefinition 函数名（SpawnSkill→spawn_agent，OrchestrateSkill→orchestrate，
// HandoffSkill→transfer_to_agent）。
var multiAgentRecursiveTools = []string{"spawn_agent", "orchestrate", "transfer_to_agent"}

type ctxKeySpawnDepthType struct{}

var ctxKeySpawnDepth = ctxKeySpawnDepthType{}

// withSpawnDepth 把当前派生深度盖到 ctx 上，供 spawn/orchestrate skill 的深度闸读取。
func withSpawnDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, ctxKeySpawnDepth, depth)
}

// spawnDepthFromContext 读取 ctx 上的当前派生深度（未设则 0）。
func spawnDepthFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if d, ok := ctx.Value(ctxKeySpawnDepth).(int); ok {
		return d
	}
	return 0
}

// SpawnDepthFromContext 是导出访问器：cmd/hexclaw 的子任务派发闭包（package main）用它
// 给子消息盖 spawn_depth = 调用方深度 + 1。导出是因为派发闭包不在 engine 包内。
func SpawnDepthFromContext(ctx context.Context) int {
	return spawnDepthFromContext(ctx)
}

// spawnDepthFromMessage 从被派发的消息读取 spawn_depth（未设 / 非法则 0）。它是子 Agent
// 深度跨 Process 边界的真值来源——cmd/hexclaw 构建子任务消息时盖上。
func spawnDepthFromMessage(msg *adapter.Message) int {
	if msg == nil || msg.Metadata == nil {
		return 0
	}
	if d, err := strconv.Atoi(msg.Metadata["spawn_depth"]); err == nil && d > 0 {
		return d
	}
	return 0
}

// spawnDepthExceeded 报告当前 ctx 深度的 Agent 是否已不允许再派生 / 编排子 Agent。
func spawnDepthExceeded(ctx context.Context) bool {
	return spawnDepthFromContext(ctx) >= maxSpawnDepth
}

// spawnDepthLimitMessage 在 Agent 试图越过 maxSpawnDepth 时作为「正常工具结果」返回
// （而非 error），让 LLM 能据此调整策略——自己把子任务做掉，而不是看到一个不透明的失败。
func spawnDepthLimitMessage(ctx context.Context, tool string) string {
	return fmt.Sprintf(
		"%s 不可用：已达到子 Agent 派生深度上限 (depth=%d, max=%d)。请直接自己完成该子任务，不要再派生子 Agent。",
		tool, spawnDepthFromContext(ctx), maxSpawnDepth)
}

// stripSpawnRecursiveTools 在子 Agent 到达 / 越过 maxSpawnDepth 时，从其工具集剔除多
// Agent 编排工具。与 stripCronRecursiveTools 同构，但以 spawn 深度（而非 cron source）为闸。
// 名字大小写不敏感匹配，与 cron 防护一致。
func stripSpawnRecursiveTools(msg *adapter.Message, tools []llm.ToolDefinition) []llm.ToolDefinition {
	if len(tools) == 0 || spawnDepthFromMessage(msg) < maxSpawnDepth {
		return tools
	}
	deny := make(map[string]bool, len(multiAgentRecursiveTools))
	for _, n := range multiAgentRecursiveTools {
		deny[strings.ToLower(n)] = true
	}
	out := make([]llm.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if !deny[strings.ToLower(t.Function.Name)] {
			out = append(out, t)
		}
	}
	return out
}

// ───────────────────── 工具继承链（feature 2，对齐 OpenClaw inheritedToolAllow/Deny）─────────────────────
//
// 父 Agent 派生子 Agent 时可收窄子的工具集（白名单 allow / 黑名单 deny）。策略随 spawn_depth 一路
// 透传，并在每一层「只能更窄、不能更宽」（链式交集）——子拿不到父没有的工具。

type ctxKeyInheritedToolsType struct{}

var ctxKeyInheritedTools = ctxKeyInheritedToolsType{}

type inheritedTools struct {
	allow []string // 空 = 不限白名单
	deny  []string
}

// withInheritedTools 把当前 Agent 继承到的工具策略盖到 ctx，供其再派生子 Agent 时链式收窄。
func withInheritedTools(ctx context.Context, allow, deny []string) context.Context {
	return context.WithValue(ctx, ctxKeyInheritedTools, inheritedTools{allow: allow, deny: deny})
}

// inheritedToolsFromContext 读取 ctx 上当前 Agent 的继承工具策略。
func inheritedToolsFromContext(ctx context.Context) (allow, deny []string) {
	if ctx == nil {
		return nil, nil
	}
	if it, ok := ctx.Value(ctxKeyInheritedTools).(inheritedTools); ok {
		return it.allow, it.deny
	}
	return nil, nil
}

// inheritedToolsFromMessage 从被派发消息的 metadata(tool_allow/tool_deny，逗号分隔) 读取继承策略。
func inheritedToolsFromMessage(msg *adapter.Message) (allow, deny []string) {
	if msg == nil || msg.Metadata == nil {
		return nil, nil
	}
	return splitCSV(msg.Metadata["tool_allow"]), splitCSV(msg.Metadata["tool_deny"])
}

// narrowChildTools 计算子 Agent 的有效工具策略：在父继承策略上「只能更窄」。
//   - allow：父子白名单求交（任一为空表示该侧不限，取另一侧）。
//   - deny：父子黑名单求并。
func narrowChildTools(parentAllow, parentDeny, reqAllow, reqDeny []string) (allow, deny []string) {
	switch {
	case len(parentAllow) == 0:
		allow = reqAllow
	case len(reqAllow) == 0:
		allow = parentAllow
	default:
		allow = intersectCSV(parentAllow, reqAllow)
	}
	deny = unionCSV(parentDeny, reqDeny)
	return allow, deny
}

// applyInheritedToolPolicy 按消息携带的继承策略过滤工具集：先去 deny，再（若 allow 非空）只留 allow。
// 大小写不敏感。allow 永远不放行被剥夺的多 Agent 工具（leaf 防护在前已剔除，这里不会重新引入）。
func applyInheritedToolPolicy(msg *adapter.Message, tools []llm.ToolDefinition) []llm.ToolDefinition {
	allow, deny := inheritedToolsFromMessage(msg)
	if len(allow) == 0 && len(deny) == 0 {
		return tools
	}
	denySet := toLowerSet(deny)
	allowSet := toLowerSet(allow)
	out := make([]llm.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		name := strings.ToLower(t.Function.Name)
		if denySet[name] {
			continue
		}
		if len(allowSet) > 0 && !allowSet[name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func joinCSV(items []string) string { return strings.Join(items, ",") }

func toLowerSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[strings.ToLower(strings.TrimSpace(s))] = true
	}
	return m
}

func intersectCSV(a, b []string) []string {
	bs := toLowerSet(b)
	out := make([]string, 0, len(a))
	for _, x := range a {
		if bs[strings.ToLower(strings.TrimSpace(x))] {
			out = append(out, x)
		}
	}
	return out
}

func unionCSV(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, x := range append(append([]string{}, a...), b...) {
		k := strings.ToLower(strings.TrimSpace(x))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, x)
	}
	return out
}
