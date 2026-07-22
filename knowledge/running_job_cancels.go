package knowledge

import (
	"context"
	"sync"
)

// runningJobCancelRegistry bridges a durable cancellation command to provider
// work already executing in this process. Durable job state and lease fencing
// remain authoritative; this registry only shortens the time until the current
// provider context observes cancellation.
type runningJobCancelRegistry struct {
	mu      sync.Mutex
	nextID  uint64
	running map[string]runningJobCancel
}

type runningJobCancel struct {
	id     uint64
	cancel context.CancelFunc
}

func newRunningJobCancelRegistry() *runningJobCancelRegistry {
	return &runningJobCancelRegistry{running: make(map[string]runningJobCancel)}
}

func (r *runningJobCancelRegistry) register(jobID string, cancel context.CancelFunc) func() {
	if r == nil || jobID == "" || cancel == nil {
		return func() {}
	}
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.running[jobID] = runningJobCancel{id: id, cancel: cancel}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if current, ok := r.running[jobID]; ok && current.id == id {
			delete(r.running, jobID)
		}
		r.mu.Unlock()
	}
}

func (r *runningJobCancelRegistry) cancel(jobID string) {
	if r == nil || jobID == "" {
		return
	}
	r.mu.Lock()
	entry, ok := r.running[jobID]
	r.mu.Unlock()
	if ok {
		entry.cancel()
	}
}
