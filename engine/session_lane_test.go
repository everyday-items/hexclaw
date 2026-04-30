package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/session"
)

func TestLocalSessionLaneSerializesSameSession(t *testing.T) {
	lane := NewLocalSessionLane(session.NewSessionLock())
	first, err := lane.Acquire(context.Background(), LaneKey{SessionID: "s1", RequestID: "r1"})
	if err != nil {
		t.Fatalf("Acquire first: %v", err)
	}

	var acquired bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		second, err := lane.Acquire(context.Background(), LaneKey{SessionID: "s1", RequestID: "r2"})
		if err != nil {
			t.Errorf("Acquire second: %v", err)
			return
		}
		acquired = true
		_ = second.Release(context.Background())
	}()

	time.Sleep(20 * time.Millisecond)
	if acquired {
		t.Fatal("second lease acquired before first release")
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatalf("Release first: %v", err)
	}
	wg.Wait()
	if !acquired {
		t.Fatal("second lease did not acquire after first release")
	}
}

func TestLocalSessionLaneRejectsEmptySession(t *testing.T) {
	lane := NewLocalSessionLane(session.NewSessionLock())
	if _, err := lane.Acquire(context.Background(), LaneKey{RequestID: "r1"}); err == nil {
		t.Fatal("expected empty session id error")
	}
}
