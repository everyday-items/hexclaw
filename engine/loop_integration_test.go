package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// IT-01: 反馈闭环——真实 AcceptanceEngine + checker + reconciler；maker 读上一轮验收
// 反馈、补齐缺失项，第 2 轮达成。证明 feedback 真的 maker→checker→maker 流转。
func TestIntegrationFeedbackImprovesAcrossRounds(t *testing.T) {
	ctx := context.Background()
	goal := Goal{
		ID:     "fb",
		Intent: "写一份报告",
		Acceptance: []AcceptanceSpec{
			{Kind: AcceptContains, Value: "摘要", Description: "含摘要"},
			{Kind: AcceptContains, Value: "结论", Description: "含结论"},
		},
		Budget:     GoalBudget{MaxRounds: 5, MaxWall: time.Hour},
		Escalation: EscalationPolicy{NoProgressRounds: 99},
	}
	maker := MakerFunc(func(_ context.Context, _, feedback string, _ int) (MakeResult, error) {
		out := "摘要：本文概述……"
		if strings.Contains(feedback, "结论") { // 只有看到"缺结论"的反馈才补结论
			out += "\n结论：综上……"
		}
		return MakeResult{Output: out}, nil
	})
	res, _ := NewGoalLoop(maker, NewChecker(NewAcceptanceEngine(), nil, 1), NewMemStateStore(), nil).Run(ctx, goal)
	if res.Outcome != OutcomeDone {
		t.Fatalf("反馈驱动应达成: %+v", res)
	}
	if res.Rounds != 2 {
		t.Fatalf("第 1 轮缺结论、反馈后第 2 轮补上，应 2 轮: %d", res.Rounds)
	}
	if !strings.Contains(res.FinalOutput, "结论") {
		t.Fatalf("最终产出应含结论: %q", res.FinalOutput)
	}
}

// IT-02: 升级 + 落盘 + 幂等续跑——卡死目标升级，状态落盘，重启后幂等返回升级态不再推进
func TestIntegrationEscalationPersistsAndResumes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := NewFileStateStore(dir)
	g := doneGoal("esc-persist", 10)
	g.Escalation.NoProgressRounds = 2
	maker := MakerFunc(func(_ context.Context, _, _ string, _ int) (MakeResult, error) {
		return MakeResult{Output: "卡住了，没动"}, nil
	})
	var escd *Escalation
	sink := EscalationFunc(func(_ context.Context, e Escalation) error { escd = &e; return nil })

	res, _ := NewGoalLoop(maker, acceptanceChecker(), store, sink).Run(ctx, g)
	if res.Outcome != OutcomeEscalated || escd == nil {
		t.Fatalf("应升级且 sink 收到: res=%+v escd=%v", res, escd)
	}
	st, ok, _ := store.Load(ctx, "esc-persist")
	if !ok || st.Outcome != OutcomeEscalated || len(st.History) == 0 {
		t.Fatalf("升级态应落盘 + 留痕: ok=%v st=%+v", ok, st)
	}

	// 模拟进程重启：新 loop 实例 + 新 maker，幂等返回升级态、不再调 maker
	called := false
	m2 := MakerFunc(func(_ context.Context, _, _ string, _ int) (MakeResult, error) {
		called = true
		return MakeResult{}, nil
	})
	res2, _ := NewGoalLoop(m2, acceptanceChecker(), store, nil).Run(ctx, Goal{ID: "esc-persist", Intent: "x"})
	if called || res2.Outcome != OutcomeEscalated {
		t.Fatalf("升级后应幂等不再推进: called=%v res2=%+v", called, res2)
	}
}

// IT-03: 命令验收集成——真实 ExecCommandRunner + Workdir 指向临时目录，maker 往该目录
// 写文件，命令断言读它；走完 maker→命令验收→done 一条链。
func TestIntegrationCommandAcceptanceInWorkdir(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	ae := &AcceptanceEngine{CmdRunner: NewExecCommandRunner(10 * time.Second), Workdir: work}
	goal := Goal{
		ID:         "cmd",
		Intent:     "生成 out.json 且 status=ok",
		Acceptance: []AcceptanceSpec{{Kind: AcceptCommand, Value: `grep -q '"status":"ok"' out.json`, Description: "产物含 ok 状态"}},
		Budget:     GoalBudget{MaxRounds: 4, MaxWall: time.Minute},
		Escalation: EscalationPolicy{NoProgressRounds: 99},
	}
	maker := MakerFunc(func(_ context.Context, _, _ string, r int) (MakeResult, error) {
		body := `{"status":"pending"}`
		if r >= 2 {
			body = `{"status":"ok"}`
		}
		if err := os.WriteFile(filepath.Join(work, "out.json"), []byte(body), 0o644); err != nil {
			return MakeResult{}, err
		}
		return MakeResult{Output: "wrote " + body}, nil
	})
	res, _ := NewGoalLoop(maker, NewChecker(ae, nil, 1), NewMemStateStore(), nil).Run(ctx, goal)
	if res.Outcome != OutcomeDone {
		t.Fatalf("命令验收应在产物达标后判 done: %+v", res)
	}
	if res.Rounds != 2 {
		t.Fatalf("第 1 轮 pending 不过、第 2 轮 ok 过，应 2 轮: %d", res.Rounds)
	}
}
