package usecase

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

var errAgentWorkerQuiesced = fmt.Errorf("K12 Agent worker quiesced")

type agentWorkerFence struct {
	refs int
}

type agentWorkerRegistration struct {
	agentName string
	cancel    context.CancelCauseFunc
	done      chan struct{}
}

// agentWorkerFenceRegistry is the single Agent-scoped lifecycle primitive shared
// by K12 coordinators. Durable stores remain the queue/source of truth; this
// registry only fences new local workers and drains already-owned contexts.
type agentWorkerFenceRegistry struct {
	mu      sync.Mutex
	fences  map[string]*agentWorkerFence
	workers map[string]*agentWorkerRegistration
}

func (r *agentWorkerFenceRegistry) start(
	parent context.Context,
	agentName, workerKey string,
) (context.Context, func(), bool) {
	agentName = strings.TrimSpace(agentName)
	workerKey = strings.TrimSpace(workerKey)
	if agentName == "" || workerKey == "" {
		return nil, nil, false
	}
	if parent == nil {
		parent = context.Background()
	}

	r.mu.Lock()
	if r.fences != nil && r.fences[agentName] != nil {
		r.mu.Unlock()
		return nil, nil, false
	}
	if r.workers == nil {
		r.workers = map[string]*agentWorkerRegistration{}
	}
	if r.workers[workerKey] != nil {
		r.mu.Unlock()
		return nil, nil, false
	}
	runCtx, cancel := context.WithCancelCause(parent)
	registration := &agentWorkerRegistration{
		agentName: agentName,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	r.workers[workerKey] = registration
	r.mu.Unlock()

	var finishOnce sync.Once
	return runCtx, func() {
		finishOnce.Do(func() {
			r.mu.Lock()
			if r.workers[workerKey] == registration {
				delete(r.workers, workerKey)
			}
			close(registration.done)
			r.mu.Unlock()
		})
	}, true
}

func (r *agentWorkerFenceRegistry) quiesceAgent(
	ctx context.Context,
	agentName string,
) (func(), error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return func() {}, fmt.Errorf("K12 Agent worker quiesce requires agent name")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.Lock()
	if r.fences == nil {
		r.fences = map[string]*agentWorkerFence{}
	}
	fence := r.fences[agentName]
	if fence == nil {
		fence = &agentWorkerFence{}
		r.fences[agentName] = fence
	}
	fence.refs++
	workers := make([]*agentWorkerRegistration, 0)
	for _, worker := range r.workers {
		if worker.agentName == agentName {
			workers = append(workers, worker)
		}
	}
	r.mu.Unlock()

	var resumeOnce sync.Once
	resume := func() {
		resumeOnce.Do(func() {
			r.mu.Lock()
			if r.fences[agentName] == fence && fence.refs > 0 {
				fence.refs--
			}
			if r.fences[agentName] == fence && fence.refs == 0 {
				delete(r.fences, agentName)
			}
			r.mu.Unlock()
		})
	}
	for _, worker := range workers {
		worker.cancel(errAgentWorkerQuiesced)
	}
	for _, worker := range workers {
		select {
		case <-worker.done:
		case <-ctx.Done():
			return resume, ctx.Err()
		}
	}
	return resume, nil
}
