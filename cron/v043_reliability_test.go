package cron

// cron 引擎可靠性测试：全局并发上限、确定性错峰、per-job 时区。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingEngine 是一个假 ScriptEngine：在 Execute 内阻塞直到 release 关闭，
// 期间用原子计数记录峰值并发，用来观测调度器的全局并发上限是否真的生效。
type blockingEngine struct {
	release  chan struct{}
	inflight int64
	peak     int64
}

func (e *blockingEngine) Name() string         { return "blocking" }
func (e *blockingEngine) Available() bool       { return true }
func (e *blockingEngine) Validate(string) error { return nil }
func (e *blockingEngine) Execute(ctx context.Context, _ *JobSpec) (*RunResult, error) {
	n := atomic.AddInt64(&e.inflight, 1)
	for {
		p := atomic.LoadInt64(&e.peak)
		if n <= p || atomic.CompareAndSwapInt64(&e.peak, p, n) {
			break
		}
	}
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	atomic.AddInt64(&e.inflight, -1)
	return &RunResult{Status: "success"}, nil
}

func TestConcurrency_CapEnforced(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewScheduler(db, nil, nil)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	const cap = 2
	s.SetMaxConcurrentRuns(cap)
	eng := &blockingEngine{release: make(chan struct{})}
	s.engines["blocking"] = eng

	const M = 6
	now := time.Now()
	s.mu.Lock()
	for i := 0; i < M; i++ {
		id := fmt.Sprintf("job-%d", i)
		s.jobs[id] = &Job{
			ID: id, Name: id, Type: JobTypeCron, Status: StatusActive,
			NextRunAt: now.Add(-time.Minute),
			Spec:      &JobSpec{Runtime: "blocking", TimeoutSec: 60},
		}
	}
	s.mu.Unlock()

	s.checkAndExecute(now)

	// Wait until the cap is saturated.
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&eng.inflight) < cap {
		if time.Now().After(deadline) {
			t.Fatalf("never saturated cap; inflight=%d", atomic.LoadInt64(&eng.inflight))
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give any over-cap goroutines a window to (wrongly) start.
	time.Sleep(250 * time.Millisecond)
	if p := atomic.LoadInt64(&eng.peak); p > cap {
		close(eng.release)
		t.Fatalf("concurrency cap breached: peak=%d > cap=%d", p, cap)
	}
	close(eng.release) // drain the rest
}

func TestStagger_Deterministic(t *testing.T) {
	const max = 5 * time.Second
	d1 := staggerFor("job-abc", max)
	d2 := staggerFor("job-abc", max)
	if d1 != d2 {
		t.Fatalf("stagger not deterministic for same id: %v vs %v", d1, d2)
	}
	if d1 < 0 || d1 >= max {
		t.Fatalf("stagger out of [0,max): %v", d1)
	}
	if got := staggerFor("job-abc", 0); got != 0 {
		t.Fatalf("max=0 must disable stagger, got %v", got)
	}
	if got := staggerFor("", max); got != 0 {
		t.Fatalf("empty id must yield 0, got %v", got)
	}
}

func TestFailureDeliver_RoutesToTarget(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewScheduler(db, nil, nil)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var mu sync.Mutex
	got := map[string]string{}
	s.SetDeliverer(func(_ *Job, target, content string) error {
		mu.Lock()
		defer mu.Unlock()
		got[target] = content
		return nil
	})

	job := &Job{
		ID: "j1", Name: "采集", Spec: &JobSpec{Runtime: "python3"},
		FailureDeliver: []string{"feishu", "discord"},
	}
	s.maybeFailureDeliver(job, &RunResult{Status: "error", Error: "boom upstream 500"})

	mu.Lock()
	defer mu.Unlock()
	for _, ch := range []string{"feishu", "discord"} {
		c, ok := got[ch]
		if !ok {
			t.Fatalf("failure summary not delivered to %q", ch)
		}
		if !strings.Contains(c, "boom upstream 500") {
			t.Fatalf("%s summary missing error detail: %q", ch, c)
		}
		if !strings.Contains(c, "采集") {
			t.Fatalf("%s summary missing job name: %q", ch, c)
		}
	}

	// Empty FailureDeliver → no-op, no panic.
	s.maybeFailureDeliver(&Job{ID: "j2", Name: "x"}, &RunResult{Status: "error"})
}

func TestContextFrom_InjectsUpstream(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewScheduler(db, nil, nil)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Upstream job "A" produced a result.
	if err := s.persistHistory(context.Background(), "A", "success", "", "", 12,
		time.Now(), "alpha product", "", 0, nil); err != nil {
		t.Fatalf("persistHistory: %v", err)
	}
	if got := s.latestRunOutput("A"); got != "alpha product" {
		t.Fatalf("latestRunOutput(A) = %q, want %q", got, "alpha product")
	}
	if got := s.latestRunOutput("nonexistent"); got != "" {
		t.Fatalf("missing upstream must yield empty, got %q", got)
	}

	// Downstream job "B" reads A's latest product — injected into a per-run copy.
	b := &Job{
		ID: "B", Name: "汇总", SourcePrompt: "summarize the feed",
		ContextFrom: []string{"A"},
		Spec:        &JobSpec{Runtime: RuntimeStarlark, Inputs: map[string]any{"x": 1}, TimeoutSec: 60},
	}
	injected := withUpstreamContext(b, s.latestRunOutput("A"))

	if injected.Spec.Inputs["upstream_context"] != "alpha product" {
		t.Fatalf("script Inputs missing upstream_context: %+v", injected.Spec.Inputs)
	}
	if injected.Spec.Inputs["x"] != 1 {
		t.Fatalf("existing Inputs lost: %+v", injected.Spec.Inputs)
	}
	if !strings.Contains(injected.SourcePrompt, "alpha product") || !strings.Contains(injected.SourcePrompt, "summarize the feed") {
		t.Fatalf("agent prompt not prefixed with upstream: %q", injected.SourcePrompt)
	}

	// Non-mutation: the shared *JobSpec and original job are untouched.
	if _, leaked := b.Spec.Inputs["upstream_context"]; leaked {
		t.Fatalf("shared *JobSpec was mutated (cron.go:77 contract violated)")
	}
	if b.SourcePrompt != "summarize the feed" {
		t.Fatalf("original SourcePrompt mutated: %q", b.SourcePrompt)
	}
	if injected.Spec == b.Spec {
		t.Fatalf("injected job must use a copied Spec, not the shared pointer")
	}
}

func TestNextRunTime_PerJobTZ(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable on host")
	}
	// @daily computed in NY must land on NY midnight, regardless of the absolute instant.
	from := time.Date(2026, 6, 20, 15, 0, 0, 0, time.UTC).In(ny) // 11:00 EDT
	next, err := nextRunTime("@daily", JobTypeCron, from)
	if err != nil {
		t.Fatalf("nextRunTime @daily: %v", err)
	}
	if next.Hour() != 0 || next.Minute() != 0 {
		t.Fatalf("@daily in NY not midnight-NY: %v (hour=%d)", next, next.Hour())
	}
	if next.Location().String() != ny.String() {
		t.Fatalf("location not preserved: got %s", next.Location())
	}

	// 5-field cron "0 9 * * *" computed in NY must fire at 09:00 NY wall-clock.
	from2 := time.Date(2026, 6, 20, 5, 0, 0, 0, time.UTC).In(ny) // 01:00 EDT
	next2, err := nextRunTime("0 9 * * *", JobTypeCron, from2)
	if err != nil {
		t.Fatalf("nextRunTime cron: %v", err)
	}
	if next2.Hour() != 9 {
		t.Fatalf("cron 0 9 * * * in NY not 09:00-NY: %v (hour=%d)", next2, next2.Hour())
	}

	// jobLocation falls back to local on invalid/empty, never silently UTC.
	if jobLocation("").String() != time.Local.String() {
		t.Fatalf("empty tz must fall back to local")
	}
	if jobLocation("Not/AZone").String() != time.Local.String() {
		t.Fatalf("invalid tz must fall back to local")
	}
}
