package knowledge

import (
	"context"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/localinfer"
	"github.com/hexagon-codes/hexclaw/resourcegov"
)

func TestManagerExplicitCloudEmbeddingBypassesLocalAccelerator(t *testing.T) {
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(
		context.Background(), resourcegov.ResourceLocalInference, resourcegov.PriorityInteractive,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()

	manager := &Manager{resourceGovernor: governor}
	WithEmbeddingProviderLocation(ProviderLocationCloud)(manager)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, lease, permit, err := manager.acquireEmbedding(
		ctx, localinfer.OperationQueryEmbedding, resourcegov.PriorityQuery,
	)
	if err != nil {
		t.Fatalf("cloud embedding waited on local slot: %v", err)
	}
	if lease != nil || permit != nil {
		t.Fatalf("cloud embedding acquired local admission: lease=%v permit=%v", lease, permit)
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceLocalInference]
	if metric.InUse != 1 || metric.QueuedInteractive != 0 || metric.QueuedBackground != 0 {
		t.Fatalf("cloud embedding changed local slot state: %+v", metric)
	}
}

func TestManagerExplicitLocalEmbeddingUsesLegacyGovernorWhenCoordinatorDisabled(t *testing.T) {
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(
		context.Background(), resourcegov.ResourceLocalInference, resourcegov.PriorityInteractive,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()

	manager := &Manager{resourceGovernor: governor}
	WithEmbeddingProviderLocation(ProviderLocationLocal)(manager)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, lease, permit, err := manager.acquireEmbedding(
		ctx, localinfer.OperationQueryEmbedding, resourcegov.PriorityQuery,
	)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("local rollback path bypassed occupied legacy slot: err=%v ctx_err=%v", err, ctx.Err())
	}
	if lease != nil || permit != nil {
		t.Fatalf("cancelled local rollback acquired admission: lease=%v permit=%v", lease, permit)
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceLocalInference]
	if metric.InUse != 1 || metric.QueuedInteractive != 0 || metric.QueuedBackground != 0 {
		t.Fatalf("cancelled local rollback leaked queue/capacity: %+v", metric)
	}
}

func TestManagerProviderBoundLocalEmbeddingDoesNotAcquireBeforeCache(t *testing.T) {
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(
		context.Background(), resourcegov.ResourceLocalInference, resourcegov.PriorityInteractive,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	coordinator := localinfer.New(governor)

	manager := &Manager{
		resourceGovernor: governor,
		localInference:   coordinator,
		embedder: localinfer.MarkProviderBoundEmbedder(
			&countingLegacyEmbedder{},
		),
	}
	WithEmbeddingProviderLocation(ProviderLocationLocal)(manager)
	ctx, lease, permit, err := manager.acquireEmbedding(
		context.Background(), localinfer.OperationQueryEmbedding, resourcegov.PriorityQuery,
	)
	if err != nil || lease != nil || permit != nil {
		t.Fatalf("provider-bound admission must defer until a cache miss: lease=%v permit=%v err=%v", lease, permit, err)
	}
	if got := localinfer.OperationFromContext(ctx, ""); got != localinfer.OperationQueryEmbedding {
		t.Fatalf("provider-bound context operation=%s", got)
	}
	if got := coordinator.Snapshot().Operations[localinfer.OperationQueryEmbedding].Attempts; got != 0 {
		t.Fatalf("cache boundary was acquired early: attempts=%d", got)
	}
}

func TestInvokeEmbeddingWithAdmissionReleasesCapacityOnPanic(t *testing.T) {
	for _, useCoordinator := range []bool{false, true} {
		name := "legacy-governor"
		if useCoordinator {
			name = "coordinator"
		}
		t.Run(name, func(t *testing.T) {
			governor, err := resourcegov.New(resourcegov.DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(governor.Close)
			var lease *localinfer.Lease
			var permit *resourcegov.Permit
			if useCoordinator {
				_, lease, err = localinfer.New(governor).Acquire(
					context.Background(), localinfer.OperationQueryEmbedding,
				)
			} else {
				permit, err = governor.Acquire(
					context.Background(), resourcegov.ResourceLocalInference, resourcegov.PriorityQuery,
				)
			}
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if recovered := recover(); recovered == nil {
						t.Fatal("embedding panic was not propagated")
					}
				}()
				_, _ = invokeEmbeddingWithAdmission(
					context.Background(), lease, permit,
					func(context.Context) ([][]float32, error) { panic("provider panic canary") },
				)
			}()
			if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
				t.Fatalf("panic leaked local capacity: in_use=%d", got)
			}
		})
	}
}
