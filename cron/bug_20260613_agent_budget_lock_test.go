package cron

// BUG-20260613 (review F2): runBudget's zero-default is the script default
// (defaultScriptTimeoutSec=300), which differs from the agent default
// (defaultAgentTimeoutSec=600). That divergence is only safe because agent
// specs are ALWAYS created with their timeout already resolved (agentTimeoutSec
// clamps 0 → 600), so runBudget never sees a 0 for an agent job. This is a
// regression lock on that invariant — if an agent job ever reaches the budget
// layer with an unresolved timeout, the script default would silently
// under-budget it, and this test fails.

import (
	"context"
	"testing"
	"time"
)

func TestBug20260613_AgentJobTimeoutAlwaysResolvedAtCreation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	// Agent jobs still go through AddJobFromPrompt's compiler guard, so a
	// (stub) compiler must be injected even though agent specs aren't compiled.
	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()))
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// An agent-classified prompt (reasoning verb) with NO explicit timeout.
	job, err := s.AddJobFromPrompt(context.Background(), AddJobRequest{
		Name: "晨报", Schedule: "@daily", Prompt: "每天总结要点", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	if job.Spec.Runtime != RuntimeAgent {
		t.Fatalf("expected an agent-mode job, got runtime %q", job.Spec.Runtime)
	}

	// Invariant 1: the agent timeout is resolved at creation, never left 0.
	if job.Spec.TimeoutSec != defaultAgentTimeoutSec {
		t.Errorf("agent job must carry the resolved default %d, got %d — runBudget's script default would under-budget it",
			defaultAgentTimeoutSec, job.Spec.TimeoutSec)
	}

	// Invariant 2: the outer budget covers the agent's full default run window.
	if got, want := runBudget(job.Spec.TimeoutSec), time.Duration(defaultAgentTimeoutSec)*time.Second; got < want {
		t.Errorf("runBudget(%d)=%s must cover the agent default %s", job.Spec.TimeoutSec, got, want)
	}
}

// agentTimeoutSec is the single resolver; pin that it never yields 0/short for
// the unset case, which is what protects runBudget's script-default divergence.
func TestBug20260613_AgentTimeoutResolverNeverZero(t *testing.T) {
	if agentTimeoutSec(0) != defaultAgentTimeoutSec {
		t.Errorf("unset agent timeout must resolve to %d, got %d", defaultAgentTimeoutSec, agentTimeoutSec(0))
	}
	if agentTimeoutSec(-1) != defaultAgentTimeoutSec {
		t.Errorf("negative agent timeout must resolve to %d, got %d", defaultAgentTimeoutSec, agentTimeoutSec(-1))
	}
}
