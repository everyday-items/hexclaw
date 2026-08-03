package usecase

import (
	"context"
	"testing"
	"time"
)

func TestAgentWorkerFenceRegistryOverlappingQuiesceRequiresLastRelease(t *testing.T) {
	var registry agentWorkerFenceRegistry
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	releaseFirst, err := registry.quiesceAgent(ctx, "mingming")
	if err != nil {
		t.Fatalf("first quiesce: %v", err)
	}
	releaseSecond, err := registry.quiesceAgent(ctx, "mingming")
	if err != nil {
		t.Fatalf("second quiesce: %v", err)
	}

	releaseFirst()
	_, finish, accepted := registry.start(
		context.Background(),
		"mingming",
		"after-first-release",
	)
	if accepted {
		finish()
		t.Fatal("first release removed a fence still held by the second caller")
	}

	releaseSecond()
	_, finish, accepted = registry.start(
		context.Background(),
		"mingming",
		"after-last-release",
	)
	if !accepted {
		t.Fatal("last release did not restore Agent worker admission")
	}
	finish()
}
