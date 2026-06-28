package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// P2 桌面防卡死：整次 orchestrate 套总墙钟上限，到点取消所有在飞子 Agent，不把学生晾着干等。
func TestOrchestrate_AntiHang_WallClockCancels(t *testing.T) {
	defer func(d time.Duration) { orchestrateMaxWall = d }(orchestrateMaxWall)
	SetOrchestrateMaxWall(60 * time.Millisecond)
	SetOrchestrateSynthesis(false)

	// 子 Agent 睡 2s（尊重 ctx）：墙钟若生效，应在 ~60ms 取消而非干等 2s。
	exec := func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		select {
		case <-time.After(2 * time.Second):
			return SubAgentResult{Output: "out:" + spec.Agent}, nil
		case <-ctx.Done():
			return SubAgentResult{}, ctx.Err()
		}
	}
	o := NewOrchestrateSkill(exec, nil)
	start := time.Now()
	res, err := o.Execute(context.Background(), fanoutSubtasks(3))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("墙钟应在 ~60ms 取消整次 orchestrate，实际耗时 %v（防卡死未生效）", elapsed)
	}
	if !strings.Contains(res.Content, "completed") {
		t.Errorf("正文应含完成度尾注（部分未完成），得：%s", res.Content)
	}
}

// SetOrchestrateMaxWall 仅接受 >0；非正值不改既有上限。
func TestSetOrchestrateMaxWall_RejectsNonPositive(t *testing.T) {
	defer func(d time.Duration) { orchestrateMaxWall = d }(orchestrateMaxWall)
	SetOrchestrateMaxWall(123 * time.Second)
	SetOrchestrateMaxWall(0)
	SetOrchestrateMaxWall(-1)
	if orchestrateMaxWall != 123*time.Second {
		t.Fatalf("非正值不应覆盖上限，得 %v", orchestrateMaxWall)
	}
}
