package main

import (
	"context"
	"testing"

	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestBug20260717_K12AgentCleanupDetachesAndCanRestoreCronJobs(t *testing.T) {
	ctx := context.Background()
	cleaner := newCronRegistrarFixture(t)
	specs := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", "kid-agent", nil)
	// kind 由描述符自带（CronSpec.Kind，§3.13 对齐后的单一事实源），不再平行维护 kinds 数组。
	for _, spec := range specs {
		if _, err := cleaner.Register(ctx, string(spec.Kind), spec, "", "", "k12"); err != nil {
			t.Fatal(err)
		}
	}

	detached, err := cleaner.DetachAgentResources(ctx, agentrouter.AgentConfig{
		Name:     "kid-agent",
		Metadata: map[string]string{"scenario": "k12-tutor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cleaner.sched.ListJobs(ctx, "k12")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("K12 agent deletion left %d cron jobs", len(jobs))
	}
	if detached.Rollback == nil {
		t.Fatal("cleanup must return compensation callback")
	}
	if err := detached.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, err = cleaner.sched.ListJobs(ctx, "k12")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != len(specs) {
		t.Fatalf("rollback restored %d jobs, want %d", len(jobs), len(specs))
	}
}

func TestBug20260717_NonK12AgentCleanupDoesNotTouchJobs(t *testing.T) {
	ctx := context.Background()
	cleaner := newCronRegistrarFixture(t)
	spec := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", "general-agent", nil)[0]
	if _, err := cleaner.Register(ctx, string(k12usecase.KindWeeklySheet), spec, "", "", "k12"); err != nil {
		t.Fatal(err)
	}

	detached, err := cleaner.DetachAgentResources(ctx, agentrouter.AgentConfig{
		Name:     "general-agent",
		Metadata: map[string]string{"scenario": "general"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if detached.Rollback != nil || detached.Commit != nil {
		t.Fatal("non-K12 cleanup should be a no-op")
	}
	jobs, err := cleaner.sched.ListJobs(ctx, "k12")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("non-K12 cleanup removed jobs: %d remain", len(jobs))
	}
}

func TestBug20260717_K12CronRejectsMissingAgentWhenRouterIsWired(t *testing.T) {
	ctx := context.Background()
	cleaner := newCronRegistrarFixture(t)
	cleaner.router = agentrouter.New()
	spec := k12usecase.DefaultCronSpecs("http://127.0.0.1:1", "deleted-agent", nil)[0]

	if _, err := cleaner.Register(
		ctx,
		string(k12usecase.KindWeeklySheet),
		spec,
		"",
		"",
		"k12",
	); err == nil {
		t.Fatal("provisioning a deleted/missing Agent must fail instead of creating orphan cron jobs")
	}
	jobs, err := cleaner.sched.ListJobs(ctx, "k12")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("missing Agent provision left %d orphan jobs", len(jobs))
	}
}
