package usecase

import (
	"context"
	"sync"
	"testing"
	"time"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestImageTaskCoordinatorShutdownSealsAdmissionBeforeSourceDrain(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	releaseSource := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseSource) })
	}
	queue := &sourceReprocessQueueSpy{
		job: k12storage.ProblemSourceReprocessJob{
			WorkID: "work-shutdown-order", OwnerScope: "owner-1", AgentName: "mingming",
			JobID: "job-1", Action: "resume", InputRevision: 2,
			AffectedProblemIDs: []string{"problem-1"},
		},
		claimable: true, heartbeatObserved: make(chan struct{}),
	}
	worker := &ProblemSourceReprocessWorker{
		Records: queue,
		Processor: sourceReprocessProcessorFunc(func(
			ctx context.Context,
			_ k12storage.ProblemSourceReprocessJob,
		) error {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-releaseSource
			return ctx.Err()
		}),
		WorkerID: "worker-shutdown-order", LeaseDuration: time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
	}
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := worker.Shutdown(ctx); err != nil {
			t.Errorf("cleanup source worker: %v", err)
		}
	})
	coordinator := &ImageTaskCoordinator{SourceReprocess: worker}
	if !worker.Nudge() {
		t.Fatal("source work was not scheduled")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("source work did not enter processor")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- coordinator.Shutdown(shutdownCtx)
	}()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("source shutdown did not cancel its active processor")
	}

	_, finish, accepted := coordinator.beginTrackedWorkerContext(context.Background())
	if accepted {
		finish()
	}
	release()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after source drain was released")
	}
	if accepted {
		t.Fatal("shutdown accepted ordinary work while waiting for source drain")
	}
}
