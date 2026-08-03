package localinfer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/resourcegov"
)

func newTestGovernor(t *testing.T) *resourcegov.Governor {
	t.Helper()
	governor, err := resourcegov.New(resourcegov.Config{
		Limits: map[resourcegov.Resource]int{
			resourcegov.ResourceVLM:            1,
			resourcegov.ResourceLocalInference: 1,
			resourcegov.ResourceCPUHeavy:       1,
			resourcegov.ResourceSQLiteWrite:    1,
		},
		BackgroundAging:     time.Hour,
		MaxInteractiveBurst: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	return governor
}

func waitQueuedPriority(
	t *testing.T,
	governor *resourcegov.Governor,
	priority resourcegov.Priority,
	want int,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		metric := governor.Snapshot().Resources[resourcegov.ResourceLocalInference]
		if metric.QueuedByPriority[priority] == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue did not reach priority=%s count=%d: %+v", priority, want,
		governor.Snapshot().Resources[resourcegov.ResourceLocalInference])
}

func TestCoordinatorOrdersOperationsAndSharesOneSlot(t *testing.T) {
	governor := newTestGovernor(t)
	coordinator := New(governor)

	_, hold, err := coordinator.Acquire(context.Background(), OperationDocumentEmbedding)
	if err != nil {
		t.Fatal(err)
	}

	order := make(chan Operation, 4)
	start := func(operation Operation) {
		go func() {
			_, lease, acquireErr := coordinator.Acquire(context.Background(), operation)
			if acquireErr != nil {
				t.Errorf("Acquire(%s): %v", operation, acquireErr)
				return
			}
			order <- operation
			lease.Release()
		}()
	}
	start(OperationDocumentEmbedding)
	waitQueuedPriority(t, governor, resourcegov.PriorityBackground, 1)
	start(OperationRerank)
	waitQueuedPriority(t, governor, resourcegov.PriorityRerank, 1)
	start(OperationChat)
	waitQueuedPriority(t, governor, resourcegov.PriorityInteractive, 1)
	start(OperationQueryEmbedding)
	waitQueuedPriority(t, governor, resourcegov.PriorityQuery, 1)

	hold.Release()
	want := []Operation{
		OperationQueryEmbedding,
		OperationChat,
		OperationRerank,
		OperationDocumentEmbedding,
	}
	for _, expected := range want {
		select {
		case got := <-order:
			if got != expected {
				t.Fatalf("grant=%s, want %s", got, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", expected)
		}
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceLocalInference]
	if metric.InUse != 0 {
		t.Fatalf("local inference in_use=%d, want 0", metric.InUse)
	}
}

func TestCoordinatorReusesContextLeaseWithoutDoubleAcquire(t *testing.T) {
	governor := newTestGovernor(t)
	coordinator := New(governor)

	leaseCtx, outer, err := coordinator.Acquire(context.Background(), OperationQueryEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	_, nested, err := coordinator.Acquire(leaseCtx, OperationQueryEmbedding)
	if err != nil {
		t.Fatalf("nested acquire must reuse the valid context lease: %v", err)
	}
	nested.Release()
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference]; got.AcquireCount != 1 || got.InUse != 1 {
		t.Fatalf("nested lease must not acquire/release physical capacity: %+v", got)
	}
	outer.Release()
	outer.Release()
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("in_use=%d after idempotent release, want 0", got)
	}
}

func TestCoordinatorRejectsMismatchedNestedOperation(t *testing.T) {
	governor := newTestGovernor(t)
	coordinator := New(governor)

	leaseCtx, lease, err := coordinator.Acquire(context.Background(), OperationDocumentEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, _, err := coordinator.Acquire(leaseCtx, OperationQueryEmbedding); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("mismatched nested operation error=%v, want ErrLeaseConflict", err)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].AcquireCount; got != 1 {
		t.Fatalf("conflicting nested call acquired capacity: count=%d", got)
	}
}

func TestCoordinatorPreleaseCanBeBorrowedByOnlyOnePhysicalBoundary(t *testing.T) {
	governor := newTestGovernor(t)
	coordinator := New(governor)

	leaseCtx, outer, err := coordinator.Acquire(context.Background(), OperationDocumentEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, first, err := coordinator.Acquire(leaseCtx, OperationDocumentEmbedding)
	if err != nil || firstCtx == nil || first == nil {
		t.Fatalf("first physical-boundary borrow: ctx=%v lease=%v err=%v", firstCtx, first, err)
	}
	if _, _, err := coordinator.Acquire(leaseCtx, OperationDocumentEmbedding); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("concurrent second physical-boundary borrow error=%v, want ErrLeaseConflict", err)
	}
	first.Release()
	if _, _, err := coordinator.Acquire(leaseCtx, OperationDocumentEmbedding); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("reused prelease error=%v, want ErrLeaseConflict", err)
	}
	outer.Release()
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference]; got.AcquireCount != 1 || got.InUse != 0 {
		t.Fatalf("single-use prelease resource metric=%+v", got)
	}
}

func TestCoordinatorOwnerCannotReleaseWhileBorrowedPhysicalCallIsLive(t *testing.T) {
	governor := newTestGovernor(t)
	coordinator := New(governor)
	leaseCtx, outer, err := coordinator.Acquire(context.Background(), OperationDocumentEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	_, physical, err := coordinator.Acquire(leaseCtx, OperationDocumentEmbedding)
	if err != nil {
		t.Fatal(err)
	}

	outer.Release()
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 1 {
		t.Fatalf("owner released live borrowed call: in_use=%d, want 1", got)
	}
	physical.Release()
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("borrow terminal did not release deferred owner: in_use=%d", got)
	}
}

func TestCoordinatorRejectsFirstBorrowAfterOwnerFinishBegins(t *testing.T) {
	governor := newTestGovernor(t)
	coordinator := New(governor)
	leaseCtx, owner, err := coordinator.Acquire(context.Background(), OperationDocumentEmbedding)
	if err != nil {
		t.Fatal(err)
	}

	finishEntered := make(chan struct{})
	allowFinish := make(chan struct{})
	var clockGate sync.Once
	coordinator.now = func() time.Time {
		clockGate.Do(func() {
			close(finishEntered)
			<-allowFinish
		})
		return time.Now()
	}
	var allowOnce sync.Once
	allow := func() { allowOnce.Do(func() { close(allowFinish) }) }
	defer allow()

	ownerReturned := make(chan struct{})
	go func() {
		owner.Release()
		close(ownerReturned)
	}()
	select {
	case <-finishEntered:
	case <-time.After(time.Second):
		t.Fatal("owner Finish did not enter terminal publication")
	}

	_, nested, nestedErr := coordinator.Acquire(leaseCtx, OperationDocumentEmbedding)
	allow()
	select {
	case <-ownerReturned:
	case <-time.After(time.Second):
		t.Fatal("owner Finish did not return after terminal publication was released")
	}
	if nested != nil {
		nested.Release()
	}
	if !errors.Is(nestedErr, ErrLeaseConflict) {
		t.Fatalf("borrow after owner Finish began error=%v, want ErrLeaseConflict", nestedErr)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceLocalInference].InUse; got != 0 {
		t.Fatalf("owner terminal publication leaked capacity: in_use=%d", got)
	}
}

func TestCoordinatorCancellationDoesNotLeakAndRecordsSafeMetrics(t *testing.T) {
	governor := newTestGovernor(t)
	coordinator := New(governor)

	_, hold, err := coordinator.Acquire(context.Background(), OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, acquireErr := coordinator.Acquire(waitCtx, OperationQueryEmbedding)
		result <- acquireErr
	}()
	waitQueuedPriority(t, governor, resourcegov.PriorityQuery, 1)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire error=%v", err)
	}
	hold.Release()

	snapshot := coordinator.Snapshot()
	query := snapshot.Operations[OperationQueryEmbedding]
	if query.Attempts != 1 || query.Admitted != 0 || query.Cancelled != 1 {
		t.Fatalf("query metrics=%+v", query)
	}
	chat := snapshot.Operations[OperationChat]
	if chat.Attempts != 1 || chat.Admitted != 1 || chat.Completed != 1 {
		t.Fatalf("chat metrics=%+v", chat)
	}
}
