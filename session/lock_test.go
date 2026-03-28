package session

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSessionLock_Serialization(t *testing.T) {
	sl := NewSessionLock()
	var counter atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := sl.Acquire("session-1")
			defer unlock()
			// Critical section: increment should be serialized
			val := counter.Load()
			counter.Store(val + 1)
		}()
	}
	wg.Wait()

	if got := counter.Load(); got != 100 {
		t.Errorf("expected 100, got %d (race condition?)", got)
	}
}

func TestSessionLock_DifferentSessions(t *testing.T) {
	sl := NewSessionLock()
	var wg sync.WaitGroup
	done := make(chan struct{}, 2)

	// Two different sessions should not block each other
	wg.Add(2)
	go func() {
		defer wg.Done()
		unlock := sl.Acquire("session-a")
		done <- struct{}{}
		<-done // wait for both to be inside
		unlock()
	}()
	go func() {
		defer wg.Done()
		unlock := sl.Acquire("session-b")
		done <- struct{}{}
		<-done
		unlock()
	}()

	wg.Wait()
}

func TestSessionLock_EntryReuse(t *testing.T) {
	sl := NewSessionLock()

	// Lock entries persist and are reused across Acquire calls
	unlock1 := sl.Acquire("session-a")
	unlock1()
	unlock2 := sl.Acquire("session-a")
	unlock2()

	// Entry still exists (by design — entries are never deleted)
	_, loaded := sl.locks.Load("session-a")
	if !loaded {
		t.Error("lock entry should persist for reuse")
	}
}
