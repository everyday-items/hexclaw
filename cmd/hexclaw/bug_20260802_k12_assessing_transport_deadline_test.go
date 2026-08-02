package main

import (
	"context"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/egress"
)

func TestBUG20260802K12AssessingContextsCarryTheDurableResponseHeaderBudget(t *testing.T) {
	stageCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for name, got := range map[string]context.Context{
		"solve/verify/grade": classifiedSolveExecutor{}.classify(stageCtx),
		"parent guide":       k12ParentTeachingGuideRequestContext(stageCtx),
	} {
		t.Run(name, func(t *testing.T) {
			budget, ok := egress.ProviderRequestResponseHeaderTimeoutFromContext(got)
			if !ok || budget <= time.Second {
				t.Fatalf("response-header budget=%s ok=%t, want durable stage budget", budget, ok)
			}
			deadline, hasDeadline := got.Deadline()
			if !hasDeadline || budget > time.Until(deadline)+20*time.Millisecond {
				t.Fatalf("response-header budget=%s outlives deadline=%v", budget, deadline)
			}
		})
	}
}

func TestBUG20260802K12ContextsWithoutDurableDeadlineDoNotExtendTransport(t *testing.T) {
	if _, ok := egress.ProviderRequestResponseHeaderTimeoutFromContext(classifiedSolveExecutor{}.classify(context.Background())); ok {
		t.Fatal("legacy solve context unexpectedly extends the provider transport")
	}
	if _, ok := egress.ProviderRequestResponseHeaderTimeoutFromContext(k12ParentTeachingGuideRequestContext(context.Background())); ok {
		t.Fatal("legacy parent-guide context unexpectedly extends the provider transport")
	}
}
