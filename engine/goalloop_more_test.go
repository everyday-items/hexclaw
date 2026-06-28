package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// UT-LOOP-10: 震荡检测——产出在旧状态间来回（A,B,A,B），命中"曾出现过的哈希"→ 连续无进展 → 升级
func TestGoalLoopOscillationDetection(t *testing.T) {
	ctx := context.Background()
	judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { return "NOT_DONE", nil })
	checker := NewChecker(nil, judge, 1) // llm_vote 模式：scoreStalled 不触发，隔离出"震荡"信号
	g := Goal{ID: "osc", Intent: "x", Budget: GoalBudget{MaxRounds: 10, MaxWall: time.Hour},
		Escalation: EscalationPolicy{NoProgressRounds: 2}}
	seq := []string{"A", "B", "A", "B"}
	i := 0
	maker := MakerFunc(func(_ context.Context, _, _ string, _ int) (MakeResult, error) {
		o := seq[i%len(seq)]
		i++
		return MakeResult{Output: o}, nil
	})
	res, _ := NewGoalLoop(maker, checker, NewMemStateStore(), nil).Run(ctx, g)
	if res.Outcome != OutcomeEscalated || res.Escalation == nil || res.Escalation.Reason != "no_progress" {
		t.Fatalf("震荡应升级 no_progress: %+v", res)
	}
	if res.Rounds != 4 {
		t.Fatalf("A,B,A,B 在第 3、4 轮连续命中旧态，应第 4 轮触发: %d", res.Rounds)
	}
}

// UT-LOOP-11: token 预算闸——累计达上限即触顶
func TestGoalLoopTokenBudgetCap(t *testing.T) {
	ctx := context.Background()
	g := doneGoal("tok", 10)
	g.Budget.MaxTokens = 10
	maker := MakerFunc(func(_ context.Context, _, _ string, r int) (MakeResult, error) {
		return MakeResult{Output: fmt.Sprintf("步骤%d", r), Tokens: 6}, nil
	})
	res, _ := NewGoalLoop(maker, acceptanceChecker(), NewMemStateStore(), nil).Run(ctx, g)
	if res.Outcome != OutcomeCapped {
		t.Fatalf("应 token 触顶: %+v", res)
	}
	if res.Rounds != 2 {
		t.Fatalf("6+6=12>=10，应跑 2 轮后在第 3 轮前停: %d", res.Rounds)
	}
}

// UT-LOOP-12: 墙钟闸——注入时钟越过 MaxWall 即触顶
func TestGoalLoopWallClockCap(t *testing.T) {
	ctx := context.Background()
	g := doneGoal("wall", 10)
	g.Budget.MaxWall = 50 * time.Millisecond
	l := NewGoalLoop(
		MakerFunc(func(_ context.Context, _, _ string, r int) (MakeResult, error) {
			return MakeResult{Output: fmt.Sprintf("s%d", r)}, nil
		}),
		acceptanceChecker(), NewMemStateStore(), nil)
	base := time.Unix(1700000000, 0)
	calls := 0
	l.now = func() time.Time { d := time.Duration(calls) * 40 * time.Millisecond; calls++; return base.Add(d) }
	res, _ := l.Run(ctx, g)
	if res.Outcome != OutcomeCapped {
		t.Fatalf("应墙钟触顶: %+v", res)
	}
}

// UT-LOOP-13: maker 出错 → Error 终态
func TestGoalLoopMakerError(t *testing.T) {
	maker := MakerFunc(func(_ context.Context, _, _ string, _ int) (MakeResult, error) {
		return MakeResult{}, fmt.Errorf("boom")
	})
	res, _ := NewGoalLoop(maker, acceptanceChecker(), NewMemStateStore(), nil).Run(context.Background(), doneGoal("err", 5))
	if res.Outcome != OutcomeError {
		t.Fatalf("maker 错误应 Error: %+v", res)
	}
}

// UT-LOOP-14: 升级优先级——high_risk > low_confidence > no_progress
func TestGoalLoopEscalationPriority(t *testing.T) {
	ctx := context.Background()
	seq := []string{"DONE", "NOT_DONE"} // 2 票分裂 → 置信 0.5 < 0.9（low_confidence 也会触发）
	i := 0
	judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { r := seq[i%2]; i++; return r, nil })
	checker := NewChecker(nil, judge, 2)
	g := Goal{ID: "prio", Intent: "x", Budget: GoalBudget{MaxRounds: 5, MaxWall: time.Hour},
		Escalation: EscalationPolicy{OnHighRisk: true, MinConfidence: 0.9, NoProgressRounds: 99}}
	maker := MakerFunc(func(_ context.Context, _, _ string, r int) (MakeResult, error) {
		return MakeResult{Output: fmt.Sprintf("v%d", r), HighRisk: true}, nil
	})
	res, _ := NewGoalLoop(maker, checker, NewMemStateStore(), nil).Run(ctx, g)
	if res.Escalation == nil || res.Escalation.Reason != "high_risk" {
		t.Fatalf("高风险应优先于低置信: %+v", res)
	}
}
