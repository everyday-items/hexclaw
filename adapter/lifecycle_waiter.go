package adapter

import (
	"context"
	"sync"
)

type groupWaiter interface {
	Wait()
}

// LifecycleWaiter lets repeated bounded shutdown attempts share one goroutine
// waiting for a lifecycle WaitGroup. Its zero value is ready for use.
type LifecycleWaiter struct {
	mu   sync.Mutex
	once sync.Once
	done chan struct{}
}

// Wait waits for group or the caller's deadline. Timing out does not start a
// second group.Wait goroutine on a later call in the same lifecycle.
func (w *LifecycleWaiter) Wait(ctx context.Context, group *sync.WaitGroup) error {
	return w.wait(ctx, group)
}

func (w *LifecycleWaiter) wait(ctx context.Context, group groupWaiter) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	if w.done == nil {
		w.done = make(chan struct{})
	}
	done := w.done
	w.once.Do(func() {
		go func() {
			group.Wait()
			close(done)
		}()
	})
	w.mu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reset prepares the waiter for a new lifecycle after the previous group has
// completed. It deliberately does nothing while an older lifecycle is active.
func (w *LifecycleWaiter) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done == nil {
		return
	}
	select {
	case <-w.done:
		w.done = nil
		w.once = sync.Once{}
	default:
	}
}
