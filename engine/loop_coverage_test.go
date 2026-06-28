package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// IT-04: 公共装配入口 BuildGoalLoop 端到端——用注入的 TaskExecFunc + judge 装配，跑到 done。
func TestBuildGoalLoopAssembles(t *testing.T) {
	ctx := context.Background()
	exec := TaskExecFunc(func(_ context.Context, task string) (string, error) {
		if strings.Contains(task, "上一轮验收反馈") { // 第 2 轮带反馈进来才交"完成"
			return "任务完成", nil
		}
		return "进行中", nil
	})
	judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { return "DONE", nil })
	loop := BuildGoalLoop(exec, judge, NewMemStateStore(), nil, GoalLoopOptions{CheckerSamples: 1})
	goal := Goal{
		Intent:     "做完这件事",
		Acceptance: []AcceptanceSpec{{Kind: AcceptContains, Value: "完成"}},
		Budget:     GoalBudget{MaxRounds: 5, MaxWall: time.Hour},
		Escalation: EscalationPolicy{NoProgressRounds: 99},
	}
	res, err := loop.Run(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeDone || res.Rounds != 2 {
		t.Fatalf("装配后的 loop 应在反馈驱动下第 2 轮达成: %+v", res)
	}
}

// UT-STATE-10: 损坏状态文件——Load 应返回错误而非静默当成空状态（防带病续跑）。
func TestFileStateStoreCorruptLoad(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := NewFileStateStore(dir)
	if err := os.WriteFile(filepath.Join(dir, fileSafe("bad")+".json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load(ctx, "bad"); err == nil {
		t.Fatal("损坏 JSON 应返回错误，不能静默当空状态续跑")
	}
}

// UT-STATE-11: 覆盖写——同一 goal 第二次 Save 应原子替换，读回新值。
func TestFileStateStoreOverwrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := NewFileStateStore(dir)
	if err := s.Save(ctx, RunState{GoalID: "ow", Round: 1, Outcome: OutcomeRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, RunState{GoalID: "ow", Round: 7, Outcome: OutcomeDone}); err != nil {
		t.Fatal(err)
	}
	st, ok, _ := s.Load(ctx, "ow")
	if !ok || st.Round != 7 || st.Outcome != OutcomeDone {
		t.Fatalf("覆盖写后应读回新值: %+v", st)
	}
}
