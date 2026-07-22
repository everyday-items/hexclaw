package cron

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func sqliteTotalChanges(t *testing.T, s *Scheduler) int64 {
	t.Helper()
	var changes int64
	if err := s.db.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
		t.Fatalf("read sqlite total_changes: %v", err)
	}
	return changes
}

func TestEnsureJobFromScriptMissingOnlyPreservesExactExistingJobByteSemantics(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	existing := &Job{
		ID:        "cron-parent-custom-weekly",
		Name:      "家长自定义的周复习",
		Type:      JobTypeCron,
		Schedule:  "13 7 * * 6",
		UserID:    "legacy-owner",
		Platform:  "dingtalk",
		ChatID:    "family-chat",
		Status:    StatusPaused,
		Spec:      minimalSpec(),
		Deliver:   []string{"dingtalk", "chat"},
		TZ:        "Asia/Shanghai",
		SourceKey: "mingming/weekly-sheet",
	}
	if err := s.AddJob(ctx, existing); err != nil {
		t.Fatal(err)
	}
	userTask := &Job{
		ID: "cron-user-authored", Name: "用户自己的任务", Type: JobTypeCron,
		Schedule: "0 8 * * *", UserID: "legacy-owner", Status: StatusActive,
		Spec: minimalSpec(),
	}
	if err := s.AddJob(ctx, userTask); err != nil {
		t.Fatal(err)
	}

	// Simulate an app restart: loadJobs intentionally does not put paused jobs in
	// the active in-memory map, so preservation must be decided from durable data.
	restarted := newTestScheduler(t, db)
	if err := restarted.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if jobs := restarted.JobsBySourceKeyPrefix("mingming/"); len(jobs) != 0 {
		t.Fatalf("fixture should keep the paused job outside the active map, got %+v", jobs)
	}
	durableBefore, err := restarted.ListJobs(ctx, "legacy-owner")
	if err != nil {
		t.Fatal(err)
	}
	var before *Job
	for _, candidate := range durableBefore {
		if candidate.ID == existing.ID {
			before = cloneJobSnapshot(candidate)
			break
		}
	}
	if before == nil {
		t.Fatalf("paused exact SourceKey fixture missing: %+v", durableBefore)
	}
	writesBefore := sqliteTotalChanges(t, restarted)
	got, created, err := restarted.EnsureJobFromScriptMissingOnly(ctx, AddJobRequest{
		Name: "默认错题卷", Schedule: "0 19 * * 5", UserID: "desktop-user",
		Platform: "chat", ChatID: "desktop", Deliver: []string{"chat"},
		SourceKey: "mingming/weekly-sheet",
	}, RuntimeStarlark, `emit({"status": "success"})`)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("exact SourceKey 已存在时只能 preserve，不能覆盖或新建")
	}
	if got.ID != existing.ID {
		t.Fatalf("应返回既有任务 id=%q, got %q", existing.ID, got.ID)
	}
	durableJobs, err := restarted.ListJobs(ctx, "legacy-owner")
	if err != nil {
		t.Fatal(err)
	}
	var after *Job
	for _, candidate := range durableJobs {
		if candidate.ID == existing.ID {
			after = candidate
			break
		}
	}
	if after == nil {
		t.Fatalf("paused exact SourceKey job disappeared: %+v", durableJobs)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("missing-only reconcile 改写了既有任务\nbefore=%+v\nafter=%+v", before, after)
	}
	if writesAfter := sqliteTotalChanges(t, restarted); writesAfter != writesBefore {
		t.Fatalf("preserve 分支必须零写: before=%d after=%d", writesBefore, writesAfter)
	}
	jobs, err := restarted.ListJobs(ctx, "legacy-owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("SourceKey 为空的用户任务不得被触碰, jobs=%+v", jobs)
	}
}

func TestEnsureJobFromScriptMissingOnlyCreatesOnceThenPerformsZeroWrites(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	req := AddJobRequest{
		Name: "回传提醒", Schedule: "0 20 * * *", UserID: "desktop-user",
		SourceKey: "mingming/return-reminder",
	}
	first, created, err := s.EnsureJobFromScriptMissingOnly(
		ctx, req, RuntimeStarlark, `emit({"status": "success"})`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("缺项必须创建")
	}
	writesAfterFirst := sqliteTotalChanges(t, s)
	second, created, err := s.EnsureJobFromScriptMissingOnly(
		ctx, req, RuntimeStarlark, `emit({"status": "success", "changed": True})`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("二次执行必须 preserve 同一任务: first=%+v second=%+v created=%v", first, second, created)
	}
	if writesAfterSecond := sqliteTotalChanges(t, s); writesAfterSecond != writesAfterFirst {
		t.Fatalf("二次执行必须零写: first=%d second=%d", writesAfterFirst, writesAfterSecond)
	}
}

func TestEnsureJobFromScriptMissingOnlyConcurrentAcrossOwnersCreatesOne(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	const attempts = 8
	var createdCount atomic.Int32
	errCh := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, created, err := s.EnsureJobFromScriptMissingOnly(ctx, AddJobRequest{
				Name: "学期确认", Schedule: "0 9 1 3 *", UserID: "owner-" + string(rune('a'+i)),
				SourceKey: "mingming/semester-spring",
			}, RuntimeStarlark, `emit({"status": "success"})`)
			if created {
				createdCount.Add(1)
			}
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("并发 missing-only reconcile: %v", err)
		}
	}
	if createdCount.Load() != 1 {
		t.Fatalf("跨 owner 并发只能创建一次, created=%d", createdCount.Load())
	}
	if jobs := s.JobsBySourceKeyPrefix("mingming/"); len(jobs) != 1 {
		t.Fatalf("exact SourceKey 应全局收敛为一条, jobs=%+v", jobs)
	}
}
