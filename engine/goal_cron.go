// goal_cron.go 实现 Loop Engineering #6：**触发 × 收敛合流**。
//
// 现状 cron 是开环（到点跑一段确定脚本，单次），orchestrate 是会话内的收敛环——两者没打通。
// 一个长目标（"持续把某主题资料整理进知识库""逐章梳理长文档"）既不该在单个 tick 里一次做完
// （超时、把 cron 拖成长任务），也不该每个 tick 从头重来。GoalTicker 把一个结构化目标接到
// cron/webhook 心跳：每个 tick 调一次 Step 推进**一个最小增量**，跨 tick 用 StateStore 续跑
// （level-triggered），直到 done/capped/escalated。这把"单次任务"升级成"常驻职能"。
//
// 接线（package cron 侧很薄）：调度器为每个 goal 型任务持有一个 GoalTicker，到点调 Tick；
// running==false 时把它从调度里摘除（目标已达终态）。注意：与并发会话的 cron/continuous.go
// 语义同源（跨 tick 检查点 + 三道硬闸），合并时取其一即可。
package engine

import (
	"context"
)

// GoalTicker 把一个结构化目标接到周期性心跳。
type GoalTicker struct {
	loop *GoalLoop
	goal Goal
}

// NewGoalTicker 绑定 reconciler + 目标。
func NewGoalTicker(loop *GoalLoop, goal Goal) *GoalTicker {
	return &GoalTicker{loop: loop, goal: goal}
}

// Tick 推进一个增量。running==false 表示目标已达终态、可停止调度。
func (t *GoalTicker) Tick(ctx context.Context) (res LoopResult, running bool, err error) {
	return t.loop.Step(ctx, t.goal)
}

// GoalID 返回（派生后的）稳定目标 id，供调度器作 key / 落盘 key。
// 与 reconciler 的 prepare 同源（resolveGoalID）：显式 ID 优先，否则按意图 + 验收派生（#8）。
func (t *GoalTicker) GoalID() string {
	return resolveGoalID(t.goal)
}
