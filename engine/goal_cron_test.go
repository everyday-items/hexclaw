package engine

import (
	"context"
	"fmt"
	"testing"
)

// TestGoalTickerCrossTickResume 模拟 cron：每个 tick 用**全新的 GoalLoop 实例**
// （= 进程在 tick 间被杀重启）+ 共享落盘 FileStateStore，验证长目标被摊到多 tick、
// 跨 tick 续跑、最终收敛到 done（#6 触发×收敛合流 + #5 level-triggered）。
func TestGoalTickerCrossTickResume(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	goal := doneGoal("g-cron", 10)
	goal.Escalation.NoProgressRounds = 99 // 本测试聚焦跨 tick 推进，不测升级

	newTicker := func() *GoalTicker {
		maker := MakerFunc(func(_ context.Context, _, _ string, round int) (MakeResult, error) {
			if round >= 3 {
				return MakeResult{Output: "全部完成"}, nil
			}
			return MakeResult{Output: fmt.Sprintf("第 %d 步增量", round)}, nil
		})
		// 每个 tick 全新 loop（新 maker、新 store 句柄），但 store 指向同一落盘目录 → 续跑。
		loop := NewGoalLoop(maker, acceptanceChecker(), NewFileStateStore(dir), nil)
		return NewGoalTicker(loop, goal)
	}

	var res LoopResult
	ticks := 0
	for {
		ticks++
		r, running, err := newTicker().Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		res = r
		if !running {
			break
		}
		if ticks > 20 {
			t.Fatal("未在合理 tick 内收敛")
		}
	}
	if res.Outcome != OutcomeDone {
		t.Fatalf("应跨 tick 收敛到 done: %+v", res)
	}
	if ticks != 3 {
		t.Fatalf("应在第 3 个 tick 达成（每 tick 推进一步）: ticks=%d", ticks)
	}
}

func TestGoalTickerStableID(t *testing.T) {
	tk := NewGoalTicker(nil, Goal{Intent: "整理资料"})
	if tk.GoalID() != NewGoalTicker(nil, Goal{Intent: "整理资料"}).GoalID() {
		t.Fatal("同意图应派生相同稳定 id")
	}
	if NewGoalTicker(nil, Goal{ID: "fixed", Intent: "x"}).GoalID() != "fixed" {
		t.Fatal("显式 id 应原样返回")
	}
}
