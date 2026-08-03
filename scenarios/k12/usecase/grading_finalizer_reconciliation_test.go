package usecase

import (
	"context"
	"errors"
	"testing"
)

// REG-P0: an outcome_unknown reconciliation may consume an already committed
// final artifact, but absence of that artifact must never authorize a new page
// summary/model invocation.
func TestBuildFinalTutoringTipsReconciliationOnlyNeverCreatesOrSendsSummary(t *testing.T) {
	orchestrator := &GradingOrchestrator{}
	_, invocationID, err := orchestrator.buildFinalTutoringTips(
		withProblemSourceReconciliationOnly(context.Background()),
		GradingJobView{},
		1,
		[]byte(`[]`),
	)
	if !errors.Is(err, ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("reconciliation-only summary err=%v, want reconciliation required", err)
	}
	if invocationID != "" {
		t.Fatalf("reconciliation-only summary invocation=%q, want none", invocationID)
	}
}
