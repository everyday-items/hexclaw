// goal_wiring.go 把 GoalLoop reconciler 接到现网执行栈——让这 8 项不是孤岛，而是能被
// cmd/hexclaw 一行装配起来的闭环。
//
// 刻意只依赖本子系统自己的类型 + 一个极简的 TaskExecFunc，**不耦合**正在重构中的
// SubAgent* / orchestrate API（那批在并发会话里未提交）。cmd 侧把现有执行栈（子 Agent /
// orchestrate / 单轮 agent）适配成 TaskExecFunc 即可，互不踩踏。
//
// 接缝：
//   - maker  = TaskExecFunc：每轮把"意图 + 上轮验收反馈"当任务跑一遍。
//   - checker= 形式化验收引擎（#2）+ 独立 LLM 裁判（注入，建议用 eng.JudgeText）+ 自一致投票（#3）。
//
// cmd 接线（一行）：
//
//	loop := engine.BuildGoalLoop(
//	    func(ctx context.Context, task string) (string, error) {  // 适配现有子 Agent 执行器
//	        r, err := agentExecFn(ctx, engine.SubAgentSpec{Agent: "worker", Task: task, Mode: "run"})
//	        return r.Output, err
//	    },
//	    engine.LLMJudgeFunc(eng.JudgeText),                        // 独立裁判，复用引擎 router
//	    engine.NewFileStateStore(stateDir), escalationSink,
//	    engine.GoalLoopOptions{CheckerSamples: 3},
//	)
package engine

import (
	"context"
	"fmt"
)

// TaskExecFunc 跑一个任务、返回产出文本。cmd 侧把现有执行栈适配成它，
// 避免本子系统耦合到正在重构的 SubAgent* API。
type TaskExecFunc func(ctx context.Context, task string) (string, error)

// MakerFromExec 把 TaskExecFunc 包成 Maker：每轮把意图 + 上轮反馈作为任务跑一遍。
func MakerFromExec(exec TaskExecFunc) Maker {
	return MakerFunc(func(ctx context.Context, intent, feedback string, _ int) (MakeResult, error) {
		task := intent
		if feedback != "" {
			task = fmt.Sprintf("%s\n\n[上一轮验收反馈，请针对性改进，不要重复已完成的部分]\n%s", intent, feedback)
		}
		out, err := exec(ctx, task)
		if err != nil {
			return MakeResult{}, err
		}
		return MakeResult{Output: out}, nil
	})
}

// GoalLoopOptions 是装配选项。
type GoalLoopOptions struct {
	CheckerSamples int           // 自一致采样次数（<=0 用 3）
	CmdRunner      CommandRunner // 命令验收执行器（可空 → 命令断言 inconclusive）
	Workdir        string        // 命令验收工作目录（#7 写隔离时指向 worktree）
}

// BuildGoalLoop 用注入的执行器 + 裁判装配一个闭环 reconciler（cmd/hexclaw 接线单点入口）。
// judge 建议用独立/更强模型（≠ maker）；store 为空用内存存储；sink 可空。
func BuildGoalLoop(exec TaskExecFunc, judge LLMJudge, store StateStore, sink EscalationSink, opts GoalLoopOptions) *GoalLoop {
	samples := opts.CheckerSamples
	if samples <= 0 {
		samples = 3
	}
	ae := &AcceptanceEngine{Judge: judge, CmdRunner: opts.CmdRunner, Workdir: opts.Workdir}
	checker := NewChecker(ae, judge, samples)
	return NewGoalLoop(MakerFromExec(exec), checker, store, sink)
}
