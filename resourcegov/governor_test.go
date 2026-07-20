package resourcegov

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		Limits: map[Resource]int{
			ResourceVLM:         1,
			ResourceAccelerator: 1,
			ResourceCPUHeavy:    1,
			ResourceSQLiteWrite: 1,
		},
		BackgroundAging:     time.Hour,
		MaxInteractiveBurst: 2,
	}
}

func waitQueued(t *testing.T, governor *Governor, resource Resource, interactive, background int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		metric := governor.Snapshot().Resources[resource]
		if metric.QueuedInteractive == interactive && metric.QueuedBackground == background {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue did not reach interactive=%d background=%d: %+v",
		interactive, background, governor.Snapshot().Resources[resource])
}

func TestGovernorSharesOneVLMPeakAcrossGradingAndKnowledge(t *testing.T) {
	governor, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)

	first, err := governor.Acquire(context.Background(), ResourceVLM, PriorityInteractive)
	if err != nil {
		t.Fatal(err)
	}

	var active atomic.Int32
	var peak atomic.Int32
	knowledgeStarted := make(chan struct{})
	knowledgeDone := make(chan error, 1)
	go func() {
		close(knowledgeStarted)
		permit, acquireErr := governor.Acquire(context.Background(), ResourceVLM, PriorityBackground)
		if acquireErr != nil {
			knowledgeDone <- acquireErr
			return
		}
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		active.Add(-1)
		permit.Release()
		knowledgeDone <- nil
	}()
	<-knowledgeStarted
	waitQueued(t, governor, ResourceVLM, 0, 1)

	// The grading permit and the knowledge waiter consume one shared capacity,
	// rather than one semaphore per subsystem whose limits would add together.
	if got := governor.Snapshot().Resources[ResourceVLM]; got.InUse != 1 || got.QueuedBackground != 1 {
		t.Fatalf("unexpected shared VLM metric: %+v", got)
	}
	first.Release()
	if err := <-knowledgeDone; err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > 1 {
		t.Fatalf("shared VLM peak=%d, want <=1", got)
	}
	if got := governor.Snapshot().Resources[ResourceVLM].InUse; got != 0 {
		t.Fatalf("in_use=%d after releases, want 0", got)
	}
}

func TestGovernorInteractiveOvertakesFreshBackground(t *testing.T) {
	governor, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(context.Background(), ResourceAccelerator, PriorityInteractive)
	if err != nil {
		t.Fatal(err)
	}

	order := make(chan Priority, 2)
	acquire := func(priority Priority) {
		permit, acquireErr := governor.Acquire(context.Background(), ResourceAccelerator, priority)
		if acquireErr != nil {
			t.Errorf("Acquire(%s): %v", priority, acquireErr)
			return
		}
		order <- priority
		permit.Release()
	}
	go acquire(PriorityBackground)
	waitQueued(t, governor, ResourceAccelerator, 0, 1)
	go acquire(PriorityInteractive)
	waitQueued(t, governor, ResourceAccelerator, 1, 1)
	hold.Release()

	if got := <-order; got != PriorityInteractive {
		t.Fatalf("first grant=%s, want interactive", got)
	}
	if got := <-order; got != PriorityBackground {
		t.Fatalf("second grant=%s, want background", got)
	}
}

func TestGovernorBackgroundEventuallyRunsUnderInteractivePressure(t *testing.T) {
	governor, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(context.Background(), ResourceVLM, PriorityInteractive)
	if err != nil {
		t.Fatal(err)
	}

	type acquireResult struct {
		priority Priority
		err      error
	}
	const interactiveWaiters = 5
	const totalWaiters = interactiveWaiters + 1
	results := make(chan acquireResult, totalWaiters)
	var waiters sync.WaitGroup
	acquire := func(priority Priority) {
		defer waiters.Done()
		permit, acquireErr := governor.Acquire(context.Background(), ResourceVLM, priority)
		if acquireErr != nil {
			results <- acquireResult{priority: priority, err: acquireErr}
			return
		}
		results <- acquireResult{priority: priority}
		permit.Release()
	}
	waiters.Add(1)
	go acquire(PriorityBackground)
	waitQueued(t, governor, ResourceVLM, 0, 1)
	for i := 0; i < interactiveWaiters; i++ {
		waiters.Add(1)
		go acquire(PriorityInteractive)
	}
	waitQueued(t, governor, ResourceVLM, interactiveWaiters, 1)
	hold.Release()

	backgroundPosition := -1
	for i := 0; i < totalWaiters; i++ {
		result := <-results
		if result.err != nil {
			t.Errorf("Acquire(%s): %v", result.priority, result.err)
		}
		if result.priority == PriorityBackground {
			backgroundPosition = i
		}
	}
	waiters.Wait()
	if backgroundPosition < 0 || backgroundPosition > testConfig().MaxInteractiveBurst {
		t.Fatalf("background grant position=%d, want <=%d", backgroundPosition, testConfig().MaxInteractiveBurst)
	}
}

func TestGovernorAgingPromotesBackgroundAndRecordsWait(t *testing.T) {
	var clockMu sync.Mutex
	now := time.Unix(1_000, 0)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	cfg := testConfig()
	cfg.Now = clock
	cfg.BackgroundAging = 5 * time.Second
	governor, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(context.Background(), ResourceCPUHeavy, PriorityInteractive)
	if err != nil {
		t.Fatal(err)
	}

	order := make(chan Priority, 2)
	acquire := func(priority Priority) {
		permit, acquireErr := governor.Acquire(context.Background(), ResourceCPUHeavy, priority)
		if acquireErr != nil {
			t.Errorf("Acquire(%s): %v", priority, acquireErr)
			return
		}
		order <- priority
		permit.Release()
	}
	go acquire(PriorityBackground)
	waitQueued(t, governor, ResourceCPUHeavy, 0, 1)
	go acquire(PriorityInteractive)
	waitQueued(t, governor, ResourceCPUHeavy, 1, 1)
	clockMu.Lock()
	now = now.Add(6 * time.Second)
	clockMu.Unlock()
	hold.Release()

	if got := <-order; got != PriorityBackground {
		t.Fatalf("aged first grant=%s, want background", got)
	}
	<-order
	metric := governor.Snapshot().Resources[ResourceCPUHeavy]
	if metric.WaitCount != 2 || metric.WaitTotal < 6*time.Second || metric.WaitMax < 6*time.Second {
		t.Fatalf("wait metrics do not include queued delay: %+v", metric)
	}
}

func TestGovernorCancellationRemovesWaiterAndRestoresCapacity(t *testing.T) {
	governor, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(context.Background(), ResourceVLM, PriorityInteractive)
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() {
		_, acquireErr := governor.Acquire(waitCtx, ResourceVLM, PriorityBackground)
		waitErr <- acquireErr
	}()
	waitQueued(t, governor, ResourceVLM, 0, 1)
	cancel()
	if err := <-waitErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Acquire error=%v", err)
	}
	waitQueued(t, governor, ResourceVLM, 0, 0)

	hold.Release()
	next, err := governor.Acquire(context.Background(), ResourceVLM, PriorityInteractive)
	if err != nil {
		t.Fatalf("capacity did not recover: %v", err)
	}
	next.Release()
	next.Release() // idempotent release must not inflate capacity.
	metric := governor.Snapshot().Resources[ResourceVLM]
	if metric.InUse != 0 || metric.QueuedInteractive != 0 || metric.QueuedBackground != 0 {
		t.Fatalf("leaked resource after cancellation: %+v", metric)
	}
}

func TestGovernorCloseWakesQueuedWaiters(t *testing.T) {
	governor, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	hold, err := governor.Acquire(context.Background(), ResourceSQLiteWrite, PriorityInteractive)
	if err != nil {
		t.Fatal(err)
	}

	waitErr := make(chan error, 1)
	go func() {
		_, acquireErr := governor.Acquire(context.Background(), ResourceSQLiteWrite, PriorityBackground)
		waitErr <- acquireErr
	}()
	waitQueued(t, governor, ResourceSQLiteWrite, 0, 1)
	governor.Close()
	if err := <-waitErr; !errors.Is(err, ErrClosed) {
		t.Fatalf("queued Acquire after Close error=%v", err)
	}
	hold.Release()
	if got := governor.Snapshot().Resources[ResourceSQLiteWrite].InUse; got != 0 {
		t.Fatalf("in_use=%d after close/release, want 0", got)
	}
}
