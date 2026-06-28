package engine

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// orchestrate 用裸 goroutine + 信号量做有界 fan-out，并在超时分支起 drain goroutine 收尾。
// 本测试证明正常完成路径下不泄漏 goroutine（对齐 advanced-matrix race+goleak）。
func TestOrchestrate_NoGoroutineLeak(t *testing.T) {
	// IgnoreCurrent 快照测试开始时已存在的 goroutine（全量套件里前序测试可能遗留后台 goroutine，
	// 如记忆抽取/心跳），只盯本测试新增的泄漏，避免全量跑时的假阳。
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer func(old int) { maxChildrenPerAgent = old }(maxChildrenPerAgent)
	SetMaxChildrenPerAgent(100) // 抬高数量闸，让 25 个子任务全跑以测 drain 不泄漏

	rec := &recordingExec{delay: 5 * time.Millisecond}
	o := NewOrchestrateSkill(rec.fn, nil)
	const n = 25 // > maxOrchestrateConcurrency，必然触发排队
	subtasks := make([]any, n)
	for i := 0; i < n; i++ {
		subtasks[i] = map[string]any{"agent": "a" + strconv.Itoa(i), "task": "t"}
	}
	if _, err := o.Execute(context.Background(), map[string]any{"subtasks": subtasks}); err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if rec.count() != n {
		t.Fatalf("应全部完成 %d，得 %d", n, rec.count())
	}
}
