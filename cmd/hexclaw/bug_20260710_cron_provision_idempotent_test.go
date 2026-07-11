package main

// BUG-20260710-H3（/review-go 审查 High）：k12CronRegistrar.Register 丢弃 kind
// 参数、无任何查重，直接 AddJobFromScript 落新 job——违反接口契约
// （scenarios/k12/apihttp/handler.go CronRegistrar：「幂等键 = agent+kind，重复
// 注册应覆盖或跳过」）。重复 provision（前端重试/重装/误点）→ 6 个默认任务
// 成倍复制 → 每日提醒/周五错题卷按份数重复投递到家庭群。
//
// 期望不变量：
//  1. 同 agent 同 kind 重复 Register 后，该用户 job 总数不变（覆盖或跳过）；
//  2. 不同 agent（多孩）同 kind 互不挤占，各保有自己的 job。

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/cron"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

func newCronRegistrarFixture(t *testing.T) k12CronRegistrar {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// SQLite 的 :memory: 数据库按连接隔离；并发用例必须固定到同一连接，
	// 否则失败原因会变成测试夹具的 "no such table"，而非待修复的原子幂等问题。
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	sched := cron.NewScheduler(db, nil, nil)
	if err := sched.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return k12CronRegistrar{sched: sched}
}

func TestBUG20260710_H3_CronStableKeySurvivesDisplayNameChange(t *testing.T) {
	ctx := context.Background()
	reg := newCronRegistrarFixture(t)
	spec := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", "mingming", nil)[0]
	kind := string(k12usecase.KindWeeklySheet)
	firstID, err := reg.Register(ctx, kind, spec, "api", "chat-1", "u1")
	if err != nil {
		t.Fatal(err)
	}

	renamed := spec
	renamed.Name = "每周错题复盘（新展示名）"
	secondID, err := reg.Register(ctx, kind, renamed, "api", "chat-1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := reg.sched.ListJobs(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || firstID != secondID || jobs[0].Name != renamed.Name {
		t.Fatalf("稳定 key 必须原位更新而非随展示名复制: first=%s second=%s jobs=%+v", firstID, secondID, jobs)
	}
}

func TestBUG20260710_H3_CronConcurrentRegisterCreatesOneJob(t *testing.T) {
	ctx := context.Background()
	reg := newCronRegistrarFixture(t)
	spec := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", "mingming", nil)[0]
	kind := string(k12usecase.KindWeeklySheet)

	const n = 8
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reg.Register(ctx, kind, spec, "api", "chat-1", "u1")
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("并发 Register 不得失败: %v", err)
		}
	}
	jobs, err := reg.sched.ListJobs(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("并发重复注册必须收敛为 1 条，got=%d", len(jobs))
	}
}

func TestBUG20260710_H3_CronProvisionIdempotentPerAgentKind(t *testing.T) {
	ctx := context.Background()
	reg := newCronRegistrarFixture(t)

	specsA := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", "mingming", nil)
	kindA := string(k12usecase.KindWeeklySheet)

	if _, err := reg.Register(ctx, kindA, specsA[0], "api", "chat-1", "u1"); err != nil {
		t.Fatalf("首次 Register: %v", err)
	}
	if _, err := reg.Register(ctx, kindA, specsA[0], "api", "chat-1", "u1"); err != nil {
		t.Fatalf("重复 Register: %v", err)
	}
	jobs, err := reg.sched.ListJobs(ctx, "u1")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("BUG 复现：同 agent 同 kind 重复 provision 产生 %d 个 job（want 1）——"+
			"每日提醒/错题卷将按份数重复投递", len(jobs))
	}

	// 多孩隔离：另一个孩子的同 kind 任务不得被挤占/误覆盖。
	specsB := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", "lele", nil)
	if _, err := reg.Register(ctx, kindA, specsB[0], "api", "chat-1", "u1"); err != nil {
		t.Fatalf("第二个 agent Register: %v", err)
	}
	jobs, err = reg.sched.ListJobs(ctx, "u1")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("多孩隔离被破坏：mingming+lele 各一个 job（want 2），got %d", len(jobs))
	}
}

func TestBUG20260710_H3_CronFailedReplacementKeepsPreviousJob(t *testing.T) {
	ctx := context.Background()
	reg := newCronRegistrarFixture(t)
	spec := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", "mingming", nil)[0]
	kind := string(k12usecase.KindWeeklySheet)

	firstID, err := reg.Register(ctx, kind, spec, "api", "chat-1", "u1")
	if err != nil {
		t.Fatalf("首次 Register: %v", err)
	}

	invalid := spec
	invalid.Schedule = "not-a-cron"
	if _, err := reg.Register(ctx, kind, invalid, "api", "chat-1", "u1"); err == nil {
		t.Fatal("无效替换必须返回错误")
	}

	jobs, err := reg.sched.ListJobs(ctx, "u1")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != firstID || jobs[0].Schedule != spec.Schedule {
		t.Fatalf("替换失败后旧任务必须字节语义不变: first=%s jobs=%+v", firstID, jobs)
	}
}
