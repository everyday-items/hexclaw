package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// E2E-01: 全链路闭环——把 8 项串成一条真实链路：
//
//	#1 结构化 Goal（含命令验收）→ #7 git worktree 隔离工作区 → maker 往隔离区累积写文件
//	→ #2 命令验收在该工作区里跑（Workdir）→ #3 checker 判定 → #4 语义终止
//	→ #5 FileStateStore 落盘 → #6 cron 式逐 tick(Step) 推进 → 收敛 done。
func TestE2EFullChainWorktreeCommandAcceptance(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用，跳过 E2E")
	}
	ctx := context.Background()

	// (1) 建 git 仓 + 隔离工作区（#7）
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "user.email", "t@t.dev")
	mustGit(t, repo, "config", "user.name", "tester")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "init")
	iso, err := NewGitWorktreeIsolator(repo, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := iso.Acquire(ctx, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Release()

	// (2) 结构化目标：验收 = 在隔离工作区跑命令（产物存在 + 含 DONE 标记）（#1 + #2）
	ae := &AcceptanceEngine{CmdRunner: NewExecCommandRunner(10 * time.Second), Workdir: ws.Dir}
	goal := Goal{
		ID:     "e2e",
		Intent: "在工作区产出 report.txt 且标记 DONE",
		Acceptance: []AcceptanceSpec{
			{Kind: AcceptCommand, Value: "test -f report.txt", Description: "产物存在"},
			{Kind: AcceptCommand, Value: "cat report.txt", Pattern: "DONE", Description: "完成标记"},
		},
		Budget:     GoalBudget{MaxRounds: 5, MaxWall: time.Minute},
		Escalation: EscalationPolicy{NoProgressRounds: 99},
	}

	// (3) maker 往隔离工作区累积写文件（第 2 轮才落 DONE）
	maker := MakerFunc(func(_ context.Context, _, _ string, r int) (MakeResult, error) {
		content := "WIP\n"
		if r >= 2 {
			content = "DONE\n"
		}
		if werr := os.WriteFile(filepath.Join(ws.Dir, "report.txt"), []byte(content), 0o644); werr != nil {
			return MakeResult{}, werr
		}
		return MakeResult{Output: fmt.Sprintf("第%d轮写入 %s", r, strings.TrimSpace(content))}, nil
	})

	// (4) checker 纯确定性命令验收（无 LLM）（#3）
	checker := NewChecker(ae, nil, 1)
	// (5) level-triggered 落盘 + cron 式逐 tick 推进（#5 + #6）
	loop := NewGoalLoop(maker, checker, NewFileStateStore(t.TempDir()), nil)

	var res LoopResult
	ticks := 0
	for {
		ticks++
		r, running, terr := loop.Step(ctx, goal)
		if terr != nil {
			t.Fatal(terr)
		}
		res = r
		if !running {
			break
		}
		if ticks > 10 {
			t.Fatal("未在合理 tick 内收敛")
		}
	}

	// (6) 断言全链路闭环
	if res.Outcome != OutcomeDone {
		t.Fatalf("全链路应收敛 done: %+v", res)
	}
	if ticks != 2 {
		t.Fatalf("第 1 tick WIP 不过、第 2 tick DONE 过，应 2 tick: %d", ticks)
	}
	if !res.FinalVerdict.Verdict.Passed {
		t.Fatalf("终局验收应 Passed: %+v", res.FinalVerdict)
	}
	b, _ := os.ReadFile(filepath.Join(ws.Dir, "report.txt"))
	if !strings.Contains(string(b), "DONE") {
		t.Fatalf("隔离工作区产物应含 DONE: %q", b)
	}
}

// E2E-02: 升级链路全链路——目标无形式化验收（主观）+ 独立 LLM 裁判持续 NOT_DONE +
// 复读机产出 → 走到 #8 升级，sink 收到、状态落盘。
func TestE2EEscalationChain(t *testing.T) {
	ctx := context.Background()
	judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { return "NOT_DONE\n还差很多", nil })
	g := Goal{ID: "e2e-esc", Intent: "写一篇没有验收标准的主观稿", Budget: GoalBudget{MaxRounds: 8, MaxWall: time.Minute},
		Escalation: EscalationPolicy{NoProgressRounds: 2}}
	maker := MakerFunc(func(_ context.Context, _, _ string, _ int) (MakeResult, error) {
		return MakeResult{Output: "同一版反复交"}, nil // 复读机
	})
	var escd *Escalation
	sink := EscalationFunc(func(_ context.Context, e Escalation) error { escd = &e; return nil })
	store := NewFileStateStore(t.TempDir())
	res, _ := NewGoalLoop(maker, NewChecker(NewAcceptanceEngine(), judge, 1), store, sink).Run(ctx, g)
	if res.Outcome != OutcomeEscalated || escd == nil || escd.Reason != "no_progress" {
		t.Fatalf("主观目标复读机应升级 no_progress: res=%+v escd=%v", res, escd)
	}
	st, ok, _ := store.Load(ctx, "e2e-esc")
	if !ok || st.Outcome != OutcomeEscalated {
		t.Fatalf("升级态应落盘: ok=%v st=%+v", ok, st)
	}
}

// CONC-01: 多目标并发跑、共享 StateStore——配合 `go test -race` 检测数据竞争。
func TestE2EConcurrentGoalsRaceSafe(t *testing.T) {
	ctx := context.Background()
	store := NewMemStateStore() // 共享 store
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			g := doneGoal(fmt.Sprintf("cg-%d", k), 5)
			maker := MakerFunc(func(_ context.Context, _, _ string, r int) (MakeResult, error) {
				if r >= 2 {
					return MakeResult{Output: "完成"}, nil
				}
				return MakeResult{Output: fmt.Sprintf("g%d-r%d", k, r)}, nil
			})
			res, _ := NewGoalLoop(maker, acceptanceChecker(), store, nil).Run(ctx, g)
			if res.Outcome != OutcomeDone {
				errs <- fmt.Errorf("goal %d 未达成: %v", k, res.Outcome)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
