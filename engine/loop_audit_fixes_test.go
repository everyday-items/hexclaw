package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// loop_audit_fixes_test.go 钉死 2026-06-28 深度审计揪出的 12 条缺陷（绿测试系统绕开的真 bug）。
// 每条先 RED（修前失败）后 GREEN（修后通过）。编号对应审计报告 #1..#12。

// failingSaveStore：Load 永远空、Save 永远失败——用于验证终态落盘失败可被感知。
type failingSaveStore struct{}

func (failingSaveStore) Load(context.Context, string) (RunState, bool, error) {
	return RunState{}, false, nil
}
func (failingSaveStore) Save(context.Context, RunState) error { return errors.New("disk full") }

// AUDIT #1 — rubric 验收门必须 fail-CLOSED：裁判返回空/不可解析时计 0 分（不是 10 分误放行）。
func TestAuditRubricUnparseableFailsClosed(t *testing.T) {
	ctx := context.Background()
	for _, reply := range []string{"", "我无法评估", "这篇写得挺好的"} {
		judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { return reply, nil })
		e := &AcceptanceEngine{Judge: judge}
		v := e.Evaluate(ctx, "任意产出", []AcceptanceSpec{{Kind: AcceptLLMRubric, Value: "是否达标", Threshold: 7}})
		if v.Passed {
			t.Fatalf("裁判返回 %q（不可解析）应 fail-closed 不过，而非误判 done: %+v", reply, v.Results)
		}
	}
}

// AUDIT #2 — 跨 tick 的 Step 不应被日历墙钟误判触顶（cron 常驻目标 tick 间隔可能数小时）。
func TestAuditStepIgnoresCalendarWallClock(t *testing.T) {
	ctx := context.Background()
	store := NewMemStateStore()
	g := doneGoal("audit-wall", 10)
	g.Budget.MaxWall = 10 * time.Minute // 默认级别
	g.Escalation.NoProgressRounds = 99
	maker := MakerFunc(func(_ context.Context, _, _ string, r int) (MakeResult, error) {
		if r >= 3 {
			return MakeResult{Output: "全部完成"}, nil
		}
		return MakeResult{Output: fmt.Sprintf("第 %d 步增量", r)}, nil
	})
	base := time.Unix(1700000000, 0)
	tick := 0
	var res LoopResult
	for {
		tick++
		l := NewGoalLoop(maker, acceptanceChecker(), store, nil)
		l.now = func() time.Time { return base.Add(time.Duration(tick) * time.Hour) } // 每 tick 间隔 1h ≫ 10min
		r, running, err := l.Step(ctx, g)
		if err != nil {
			t.Fatal(err)
		}
		res = r
		if !running {
			break
		}
		if tick > 10 {
			t.Fatal("未在合理 tick 内收敛")
		}
	}
	if res.Outcome != OutcomeDone {
		t.Fatalf("跨 tick（间隔1h>MaxWall10min）的 Step 应正常收敛到 done，而非被日历墙钟误判 capped: %+v", res)
	}
}

// AUDIT #3 — 验收断言全为 optional 时不应空过（即便全失败也别返回 Passed=true）。
func TestAuditAllOptionalNotVacuousPass(t *testing.T) {
	e := NewAcceptanceEngine()
	v := e.Evaluate(context.Background(), "完全不相关的产出", []AcceptanceSpec{
		{Kind: AcceptContains, Value: "甲", Optional: true},
		{Kind: AcceptContains, Value: "乙", Optional: true},
	})
	if v.Passed {
		t.Fatalf("全 optional 且全失败不应 Passed=true（空过）: %+v", v)
	}
}

// AUDIT #4 — 二元验收 + 每轮不同产出，不应因 score 恒 0 被误判 no_progress 过早升级。
func TestAuditBinaryAcceptanceNoFalseNoProgress(t *testing.T) {
	ctx := context.Background()
	g := Goal{ID: "audit-bin", Intent: "做个东西",
		Acceptance: []AcceptanceSpec{{Kind: AcceptContains, Value: "通关密语"}},
		Budget:     GoalBudget{MaxRounds: 4, MaxWall: time.Hour},
		Escalation: EscalationPolicy{NoProgressRounds: 2}} // 默认级别
	maker := MakerFunc(func(_ context.Context, _, _ string, r int) (MakeResult, error) {
		return MakeResult{Output: fmt.Sprintf("第%d次尝试，内容完全不同 %d", r, r*7)}, nil // 每轮不同
	})
	res, _ := NewGoalLoop(maker, acceptanceChecker(), NewMemStateStore(), nil).Run(ctx, g)
	if res.Outcome == OutcomeEscalated {
		t.Fatalf("每轮不同的真实尝试不应被误判 no_progress 升级，应跑到 MaxRounds capped: %+v", res)
	}
	if res.Outcome != OutcomeCapped || res.Rounds != 4 {
		t.Fatalf("应跑满 4 轮触顶: %+v", res)
	}
}

// AUDIT #5 — 裁判全部出错时置信度应为 0（而非伪高置信 1.0），以触发低置信 HITL。
func TestAuditAllErrorJudgeZeroConfidence(t *testing.T) {
	errJudge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { return "", errors.New("boom") })
	v := NewChecker(nil, errJudge, 3).Assess(context.Background(), Goal{Intent: "x"}, "y")
	if v.Done {
		t.Fatalf("全错应 not done: %+v", v)
	}
	if v.Confidence != 0 {
		t.Fatalf("裁判全错应置信 0（触发低置信升级），实际 %v", v.Confidence)
	}
}

// AUDIT #6 — 终态落盘失败应向调用方报错（否则续跑会重做已完成的工作 / 重放副作用）。
func TestAuditTerminalSaveErrorPropagates(t *testing.T) {
	ctx := context.Background()
	maker := MakerFunc(func(_ context.Context, _, _ string, _ int) (MakeResult, error) {
		return MakeResult{Output: "任务完成"}, nil
	})
	res, err := NewGoalLoop(maker, acceptanceChecker(), failingSaveStore{}, nil).Run(ctx, doneGoal("audit-save", 5))
	if res.Outcome != OutcomeDone {
		t.Fatalf("应判 done: %+v", res)
	}
	if err == nil {
		t.Fatal("终态落盘失败应向调用方报错")
	}
}

// AUDIT #8 — 同意图但不同验收的目标应派生不同的持久化 key（不共用 state）。
func TestAuditDistinctAcceptanceDistinctID(t *testing.T) {
	a := NewGoalTicker(nil, Goal{Intent: "整理资料", Acceptance: []AcceptanceSpec{{Kind: AcceptContains, Value: "甲"}}})
	b := NewGoalTicker(nil, Goal{Intent: "整理资料", Acceptance: []AcceptanceSpec{{Kind: AcceptContains, Value: "乙"}}})
	if a.GoalID() == b.GoalID() {
		t.Fatalf("同意图不同验收应派生不同 id，避免共用落盘状态: %s", a.GoalID())
	}
	// 但纯同意图（无验收）仍应稳定一致（续跑命脉）
	c := NewGoalTicker(nil, Goal{Intent: "整理资料"})
	d := NewGoalTicker(nil, Goal{Intent: "整理资料"})
	if c.GoalID() != d.GoalID() {
		t.Fatal("同意图无验收应派生相同稳定 id")
	}
}

// AUDIT #7 — 同 goalID 并发 Step 不应丢更新（串行化后总轮数 = 调用数）。-race 下验证逻辑 TOCTOU。
func TestAuditConcurrentSameGoalNoLostUpdate(t *testing.T) {
	ctx := context.Background()
	store := NewMemStateStore()
	g := Goal{ID: "audit-conc", Intent: "持续推进",
		Acceptance: []AcceptanceSpec{{Kind: AcceptContains, Value: "永不出现的密语"}}, // 永不 done
		Budget:     GoalBudget{MaxRounds: 100, MaxWall: time.Hour},
		Escalation: EscalationPolicy{NoProgressRounds: 999}}
	maker := MakerFunc(func(_ context.Context, _, _ string, r int) (MakeResult, error) {
		return MakeResult{Output: fmt.Sprintf("增量 %d", r)}, nil // 每轮不同
	})
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = NewGoalLoop(maker, acceptanceChecker(), store, nil).Step(ctx, g)
		}()
	}
	wg.Wait()
	st, _, _ := store.Load(ctx, "audit-conc")
	if st.Round != n {
		t.Fatalf("同 goal 并发 %d 次 Step 应串行推进到 %d 轮（无丢更新），实际 %d", n, n, st.Round)
	}
}

// AUDIT #9 — Release 失败时应保留 release 供重试（不能静默吞错变 no-op 致 worktree 泄漏）。
func TestAuditReleaseRetriesOnFailure(t *testing.T) {
	calls := 0
	ws := &WriteWorkspace{Dir: "/tmp/x", release: func() error {
		calls++
		if calls == 1 {
			return errors.New("worktree busy")
		}
		return nil
	}}
	if err := ws.Release(); err == nil {
		t.Fatal("首次 release 失败应返回错误")
	}
	if err := ws.Release(); err != nil {
		t.Fatalf("重试 release 应成功（release 未被提前清空）: %v", err)
	}
	if calls != 2 {
		t.Fatalf("release 应被调用 2 次（首次失败+重试），实际 %d", calls)
	}
}

// AUDIT #10 — 大整数 json_field 不应被 float64 科学计数法搞坏（1000000 != "1e+06"）。
func TestAuditJSONFieldLargeNumber(t *testing.T) {
	e := NewAcceptanceEngine()
	out := `结果 {"count":1000000} 完`
	v := e.Evaluate(context.Background(), out, []AcceptanceSpec{{Kind: AcceptJSONField, Path: "count", Value: "1000000"}})
	if !v.Passed {
		t.Fatalf("大整数字段应能正确匹配（不被科学计数法搞坏）: %+v", v.Results)
	}
}

// AUDIT #11 — llm_rubric 阈值 0 应被 Validate 拒绝（否则 score>=0 恒过 fail-open）。
func TestAuditRubricThresholdZeroRejected(t *testing.T) {
	g := &Goal{Intent: "x", Acceptance: []AcceptanceSpec{{Kind: AcceptLLMRubric, Value: "r", Threshold: 0}}}
	if err := g.Validate(); err == nil {
		t.Fatal("llm_rubric Threshold=0 应被拒绝（恒过 fail-open）")
	}
	// 合法阈值仍应通过
	ok := &Goal{Intent: "x", Acceptance: []AcceptanceSpec{{Kind: AcceptLLMRubric, Value: "r", Threshold: 7}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法阈值 7 不应被拒: %v", err)
	}
}

// AUDIT #12 — 加权得分不应逐条四舍五入（rubric 0.7 不应被记为满分）。
func TestAuditScoreNoPerCriterionRounding(t *testing.T) {
	judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { return "7\n还行", nil })
	e := &AcceptanceEngine{Judge: judge}
	specs := []AcceptanceSpec{
		{Kind: AcceptContains, Value: "ok", Weight: 1},                                // 过，score 1.0
		{Kind: AcceptLLMRubric, Value: "质量", Threshold: 9, Weight: 1, Optional: true}, // 7<9 不过但 optional，score 0.7
	}
	v := e.Evaluate(context.Background(), "ok 内容", specs)
	if !v.Passed {
		t.Fatalf("必过项过 + 可选项不过，应整体 Passed: %+v", v)
	}
	// 期望 Score=(1*1.0 + 1*0.7)/2=0.85；旧逻辑逐条 round(0.7)=1 → 1.0
	if v.Score > 0.86 || v.Score < 0.84 {
		t.Fatalf("加权得分应为 0.85（无逐条四舍五入），实际 %v", v.Score)
	}
}
