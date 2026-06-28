package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func acceptanceChecker() *Checker { return NewChecker(NewAcceptanceEngine(), nil, 1) }

func doneGoal(id string, maxRounds int) Goal {
	return Goal{
		ID:         id,
		Intent:     "把事做完",
		Acceptance: []AcceptanceSpec{{Kind: AcceptContains, Value: "完成"}},
		Budget:     GoalBudget{MaxRounds: maxRounds, MaxWall: time.Hour},
		Escalation: EscalationPolicy{NoProgressRounds: 99}, // 默认关无进展升级，单独测
	}
}

func TestGoalLoopReachesDone(t *testing.T) {
	ctx := context.Background()
	maker := MakerFunc(func(_ context.Context, _, _ string, round int) (MakeResult, error) {
		if round >= 2 {
			return MakeResult{Output: "任务完成"}, nil
		}
		return MakeResult{Output: fmt.Sprintf("进展 %d", round)}, nil
	})
	res, err := NewGoalLoop(maker, acceptanceChecker(), NewMemStateStore(), nil).Run(ctx, doneGoal("g-done", 5))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeDone || res.Rounds != 2 {
		t.Fatalf("应在第 2 轮达成: %+v", res)
	}
}

func TestGoalLoopCappedMaxRounds(t *testing.T) {
	ctx := context.Background()
	maker := MakerFunc(func(_ context.Context, _, _ string, round int) (MakeResult, error) {
		return MakeResult{Output: fmt.Sprintf("尝试 %d 但没成", round)}, nil // 每轮不同、都不过
	})
	res, _ := NewGoalLoop(maker, acceptanceChecker(), NewMemStateStore(), nil).Run(ctx, doneGoal("g-cap", 3))
	if res.Outcome != OutcomeCapped || res.Rounds != 3 {
		t.Fatalf("应触顶: %+v", res)
	}
}

func TestGoalLoopEscalateNoProgress(t *testing.T) {
	ctx := context.Background()
	g := doneGoal("g-stuck", 10)
	g.Escalation.NoProgressRounds = 2
	maker := MakerFunc(func(_ context.Context, _, _ string, _ int) (MakeResult, error) {
		return MakeResult{Output: "卡住了，没动"}, nil // 每轮产出完全相同 → 复读机
	})
	var got *Escalation
	sink := EscalationFunc(func(_ context.Context, e Escalation) error { got = &e; return nil })
	res, _ := NewGoalLoop(maker, acceptanceChecker(), NewMemStateStore(), sink).Run(ctx, g)
	if res.Outcome != OutcomeEscalated || res.Escalation == nil || res.Escalation.Reason != "no_progress" {
		t.Fatalf("复读机应升级 no_progress: %+v", res)
	}
	if got == nil || got.Reason != "no_progress" {
		t.Fatalf("sink 应收到升级: %+v", got)
	}
	// 修 #4（score 恒 0 不再被误算停滞）后：复读机第 1 轮是基线（无可比对象，不算无进展），
	// 第 2 轮起识别重复，连续 2 轮无进展（NoProgressRounds=2）在第 3 轮触发——与震荡检测
	// 同一语义（A,B,A,B 第 4 轮触发 = streak 满 2）。旧的"第 2 轮"是 score-stall-at-0 bug 多算了第 1 轮。
	if res.Rounds != 3 {
		t.Fatalf("应在第 3 轮触发（复读机第 2 轮起计无进展，streak 满 2 于第 3 轮）: %d", res.Rounds)
	}
}

func TestGoalLoopEscalateLowConfidence(t *testing.T) {
	ctx := context.Background()
	// 无形式化验收 → llm_vote；2 次采样 DONE/NOT_DONE → not done，置信 0.5
	seq := []string{"DONE", "NOT_DONE"}
	i := 0
	judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { r := seq[i%len(seq)]; i++; return r, nil })
	checker := NewChecker(nil, judge, 2)
	g := Goal{ID: "g-lowconf", Intent: "写点东西", Budget: GoalBudget{MaxRounds: 5, MaxWall: time.Hour},
		Escalation: EscalationPolicy{MinConfidence: 0.9, NoProgressRounds: 99}}
	maker := MakerFunc(func(_ context.Context, _, _ string, round int) (MakeResult, error) {
		return MakeResult{Output: fmt.Sprintf("草稿 v%d", round)}, nil
	})
	res, _ := NewGoalLoop(maker, checker, NewMemStateStore(), nil).Run(ctx, g)
	if res.Outcome != OutcomeEscalated || res.Escalation.Reason != "low_confidence" {
		t.Fatalf("低置信应升级: %+v", res)
	}
}

func TestGoalLoopEscalateHighRisk(t *testing.T) {
	ctx := context.Background()
	g := doneGoal("g-risk", 5)
	g.Escalation.OnHighRisk = true
	maker := MakerFunc(func(_ context.Context, _, _ string, _ int) (MakeResult, error) {
		return MakeResult{Output: "做了点事", HighRisk: true}, nil
	})
	res, _ := NewGoalLoop(maker, acceptanceChecker(), NewMemStateStore(), nil).Run(ctx, g)
	if res.Outcome != OutcomeEscalated || res.Escalation.Reason != "high_risk" {
		t.Fatalf("高风险应升级: %+v", res)
	}
}

func TestGoalLoopResumeFromPersisted(t *testing.T) {
	ctx := context.Background()
	store := NewMemStateStore()
	// 预置一个"跑了 2 轮、进程被杀、未达终态"的状态。
	_ = store.Save(ctx, RunState{GoalID: "g-resume", Round: 2, Outcome: OutcomeRunning,
		StartedUnixNano: time.Now().UnixNano(), OutputHashes: []string{"x", "y"}})
	firstRound := -1
	maker := MakerFunc(func(_ context.Context, _, _ string, round int) (MakeResult, error) {
		if firstRound < 0 {
			firstRound = round
		}
		return MakeResult{Output: fmt.Sprintf("续跑 r%d", round)}, nil
	})
	g := doneGoal("g-resume", 4)
	res, _ := NewGoalLoop(maker, acceptanceChecker(), store, nil).Run(ctx, g)
	if firstRound != 3 {
		t.Fatalf("应从持久化的第 3 轮续跑，实际首轮=%d", firstRound)
	}
	if res.Rounds != 4 || res.Outcome != OutcomeCapped {
		t.Fatalf("应续跑到触顶: %+v", res)
	}
}

func TestGoalLoopIdempotentTerminal(t *testing.T) {
	ctx := context.Background()
	store := NewMemStateStore()
	_ = store.Save(ctx, RunState{GoalID: "g-term", Round: 3, Outcome: OutcomeDone, LastOutput: "done"})
	makerCalled := false
	maker := MakerFunc(func(_ context.Context, _, _ string, _ int) (MakeResult, error) {
		makerCalled = true
		return MakeResult{Output: "x"}, nil
	})
	res, _ := NewGoalLoop(maker, acceptanceChecker(), store, nil).Run(ctx, Goal{ID: "g-term", Intent: "x"})
	if makerCalled {
		t.Fatal("已终态目标不应再调 maker")
	}
	if res.Outcome != OutcomeDone || res.Rounds != 3 {
		t.Fatalf("应幂等返回终态: %+v", res)
	}
}

func TestFileStateStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := NewFileStateStore(dir)
	if _, ok, _ := s.Load(ctx, "g"); ok {
		t.Fatal("不存在应返回 ok=false")
	}
	if err := s.Save(ctx, RunState{GoalID: "g", Round: 5, Outcome: OutcomeRunning}); err != nil {
		t.Fatal(err)
	}
	// 新实例（模拟重启）应读到落盘状态
	st, ok, err := NewFileStateStore(dir).Load(ctx, "g")
	if err != nil || !ok || st.Round != 5 {
		t.Fatalf("重启后应读回 Round=5: ok=%v st=%+v err=%v", ok, st, err)
	}
}
