package router

// BUG-20260616: RouteWithFallback copied only the map header (agents := r.agents)
// under RLock, released the lock, then iterated that shared map inside the
// classifier. A concurrent Register/Unregister (which delete/assign on the same
// map under the write lock) races the unlocked read -> "concurrent map read and
// map write" fatal that crashes the process. Run under -race to surface it.

import (
	"context"
	"sync"
	"testing"
)

func TestBug20260616_RouteConcurrentAgentMutation(t *testing.T) {
	d := New()
	if err := d.Register(AgentConfig{Name: "a1"}); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if err := d.Register(AgentConfig{Name: "a2"}); err != nil {
		t.Fatalf("register a2: %v", err)
	}
	d.SetClassifier(NewLLMClassifier(func(_ context.Context, _, _ string) (string, error) {
		return "a1", nil
	}))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = d.Register(AgentConfig{Name: "tmp"})
			_ = d.Unregister("tmp")
		}
	}()

	for i := 0; i < 3000; i++ {
		d.RouteWithFallback(context.Background(), RouteRequest{Platform: "no-rule"}, "route me")
	}
	close(stop)
	wg.Wait()
}
