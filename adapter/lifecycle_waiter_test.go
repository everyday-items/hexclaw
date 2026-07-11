package adapter

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type blockingGroupWaiter struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func newBlockingGroupWaiter() *blockingGroupWaiter {
	return &blockingGroupWaiter{
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
	}
}

func (g *blockingGroupWaiter) Wait() {
	g.calls.Add(1)
	g.entered <- struct{}{}
	<-g.release
}

func TestLifecycleWaiterRepeatedTimeoutStartsOneWaiterPerLifecycle(t *testing.T) {
	var waiter LifecycleWaiter
	group := newBlockingGroupWaiter()

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- waiter.wait(ctx, group) }()
	select {
	case <-group.entered:
	case <-time.After(time.Second):
		t.Fatal("first lifecycle waiter did not start")
	}
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first wait error = %v; want context canceled", err)
	}

	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		err := waiter.wait(ctx, group)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("repeated wait %d error = %v; want deadline exceeded", i, err)
		}
	}
	if got := group.calls.Load(); got != 1 {
		t.Fatalf("underlying Wait calls in one lifecycle = %d; want 1", got)
	}

	close(group.release)
	if err := waiter.wait(context.Background(), group); err != nil {
		t.Fatalf("completed lifecycle wait: %v", err)
	}
	waiter.Reset()

	nextGroup := newBlockingGroupWaiter()
	nextCtx, nextCancel := context.WithCancel(context.Background())
	nextDone := make(chan error, 1)
	go func() { nextDone <- waiter.wait(nextCtx, nextGroup) }()
	select {
	case <-nextGroup.entered:
	case <-time.After(time.Second):
		t.Fatal("reset lifecycle waiter did not start")
	}
	nextCancel()
	if err := <-nextDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("next lifecycle wait error = %v; want context canceled", err)
	}
	if got := nextGroup.calls.Load(); got != 1 {
		t.Fatalf("underlying Wait calls after reset = %d; want 1", got)
	}
	close(nextGroup.release)
	if err := waiter.wait(context.Background(), nextGroup); err != nil {
		t.Fatalf("completed reset lifecycle wait: %v", err)
	}
}
