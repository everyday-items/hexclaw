// Package resourcegov provides one process-scoped admission controller for
// expensive local resources. Callers keep their own durable job queues; this
// package only arbitrates short execution permits and never owns goroutines.
package resourcegov

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrClosed          = errors.New("resource governor is closed")
	ErrUnknownResource = errors.New("resource governor: unknown resource")
	ErrInvalidPriority = errors.New("resource governor: invalid priority")
)

// Resource identifies a process-wide capacity pool. Subsystems must use the
// same class for equivalent hardware so their independently owned worker-pool
// limits cannot add together.
type Resource string

const (
	ResourceVLM         Resource = "vlm_ocr"
	ResourceAccelerator Resource = "embedding_accelerator"
	ResourceCPUHeavy    Resource = "cpu_heavy"
	ResourceSQLiteWrite Resource = "sqlite_write"
)

var allResources = [...]Resource{
	ResourceVLM,
	ResourceAccelerator,
	ResourceCPUHeavy,
	ResourceSQLiteWrite,
}

// Priority separates latency-sensitive work from durable background work.
type Priority uint8

const (
	PriorityBackground Priority = iota + 1
	PriorityInteractive
)

type priorityContextKey struct{}

// WithPriority annotates a call chain without carrying document or prompt
// content. Consumers below both interactive and durable workflows can inspect
// this scheduling-only value at their actual resource boundary.
func WithPriority(ctx context.Context, priority Priority) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if priority != PriorityInteractive && priority != PriorityBackground {
		return ctx
	}
	return context.WithValue(ctx, priorityContextKey{}, priority)
}

func PriorityFromContext(ctx context.Context, fallback Priority) Priority {
	if ctx != nil {
		if priority, ok := ctx.Value(priorityContextKey{}).(Priority); ok &&
			(priority == PriorityInteractive || priority == PriorityBackground) {
			return priority
		}
	}
	return fallback
}

func (p Priority) String() string {
	switch p {
	case PriorityBackground:
		return "background"
	case PriorityInteractive:
		return "interactive"
	default:
		return fmt.Sprintf("priority(%d)", p)
	}
}

// Config is copied by New and is immutable thereafter.
//
// MaxInteractiveBurst supplies a deterministic starvation bound: while a
// background waiter exists, at most this many fresh interactive grants may
// overtake it. BackgroundAging independently promotes an old background waiter
// at the next release even when the burst has not been exhausted.
type Config struct {
	Limits              map[Resource]int
	BackgroundAging     time.Duration
	MaxInteractiveBurst int
	Now                 func() time.Time
}

func DefaultConfig() Config {
	return Config{
		Limits: map[Resource]int{
			ResourceVLM:         1,
			ResourceAccelerator: 1,
			ResourceCPUHeavy:    2,
			ResourceSQLiteWrite: 1,
		},
		BackgroundAging:     5 * time.Second,
		MaxInteractiveBurst: 8,
	}
}

type waiter struct {
	priority Priority
	queuedAt time.Time
	ready    chan struct{}
	queued   bool
	granted  bool
	err      error
}

type resourceState struct {
	capacity int
	inUse    int

	interactive []*waiter
	background  []*waiter
	// interactiveBurst counts grants that overtook a waiting background job.
	interactiveBurst int

	acquireCount       uint64
	waitCount          uint64
	cancelledWaitCount uint64
	waitTotal          time.Duration
	waitMax            time.Duration
}

// Governor is safe for concurrent use. One instance is intended to be created
// at the process composition root and injected into every expensive consumer.
type Governor struct {
	mu                  sync.Mutex
	states              map[Resource]*resourceState
	backgroundAging     time.Duration
	maxInteractiveBurst int
	now                 func() time.Time
	closed              bool
}

func New(config Config) (*Governor, error) {
	if config.BackgroundAging <= 0 {
		return nil, fmt.Errorf("resource governor: background aging must be positive")
	}
	if config.MaxInteractiveBurst <= 0 {
		return nil, fmt.Errorf("resource governor: max interactive burst must be positive")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	states := make(map[Resource]*resourceState, len(allResources))
	for _, resource := range allResources {
		capacity := config.Limits[resource]
		if capacity <= 0 {
			return nil, fmt.Errorf("resource governor: limit for %s must be positive", resource)
		}
		states[resource] = &resourceState{capacity: capacity}
	}
	return &Governor{
		states: states, backgroundAging: config.BackgroundAging,
		maxInteractiveBurst: config.MaxInteractiveBurst, now: config.Now,
	}, nil
}

// Permit releases one unit of capacity. Release is idempotent so cancellation
// and deferred cleanup can safely converge on the same permit.
type Permit struct {
	governor *Governor
	resource Resource
	once     sync.Once
}

func (p *Permit) Release() {
	if p == nil || p.governor == nil {
		return
	}
	p.once.Do(func() { p.governor.release(p.resource) })
}

// Acquire waits for one unit of resource capacity. Queue cancellation is
// removed synchronously. If cancellation races with a grant, the granted unit
// is returned before Acquire reports the context error.
func (g *Governor) Acquire(ctx context.Context, resource Resource, priority Priority) (*Permit, error) {
	if g == nil {
		return nil, fmt.Errorf("resource governor is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if priority != PriorityInteractive && priority != PriorityBackground {
		return nil, ErrInvalidPriority
	}

	g.mu.Lock()
	state, ok := g.states[resource]
	if !ok {
		g.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrUnknownResource, resource)
	}
	if g.closed {
		g.mu.Unlock()
		return nil, ErrClosed
	}
	w := &waiter{priority: priority, queuedAt: g.now(), ready: make(chan struct{})}
	if priority == PriorityInteractive {
		state.interactive = append(state.interactive, w)
	} else {
		state.background = append(state.background, w)
	}
	g.dispatchLocked(state)
	if w.granted {
		g.mu.Unlock()
		return &Permit{governor: g, resource: resource}, nil
	}
	w.queued = true
	g.mu.Unlock()

	select {
	case <-w.ready:
		g.mu.Lock()
		if w.err != nil {
			err := w.err
			g.mu.Unlock()
			return nil, err
		}
		if !w.granted {
			g.mu.Unlock()
			return nil, ErrClosed
		}
		// Prefer cancellation when it raced with the scheduler. Returning the
		// unit here prevents a caller that never received a Permit from leaking it.
		if err := ctx.Err(); err != nil {
			w.granted = false
			state.inUse--
			g.dispatchLocked(state)
			g.mu.Unlock()
			return nil, err
		}
		g.mu.Unlock()
		return &Permit{governor: g, resource: resource}, nil
	case <-ctx.Done():
		g.mu.Lock()
		if w.granted {
			w.granted = false
			state.inUse--
			g.dispatchLocked(state)
		} else if w.err == nil {
			if removeWaiter(state, w) {
				state.cancelledWaitCount++
			}
			g.dispatchLocked(state)
		}
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (g *Governor) release(resource Resource) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state, ok := g.states[resource]
	if !ok || state.inUse == 0 {
		return
	}
	state.inUse--
	g.dispatchLocked(state)
}

func (g *Governor) dispatchLocked(state *resourceState) {
	for !g.closed && state.inUse < state.capacity {
		w := g.nextWaiterLocked(state)
		if w == nil {
			return
		}
		state.inUse++
		state.acquireCount++
		w.granted = true
		if w.queued {
			wait := g.now().Sub(w.queuedAt)
			if wait < 0 {
				wait = 0
			}
			state.waitCount++
			state.waitTotal += wait
			if wait > state.waitMax {
				state.waitMax = wait
			}
		}
		close(w.ready)
	}
}

func (g *Governor) nextWaiterLocked(state *resourceState) *waiter {
	hasInteractive := len(state.interactive) > 0
	hasBackground := len(state.background) > 0
	if !hasInteractive && !hasBackground {
		state.interactiveBurst = 0
		return nil
	}

	chooseBackground := hasBackground && (!hasInteractive ||
		g.now().Sub(state.background[0].queuedAt) >= g.backgroundAging ||
		state.interactiveBurst >= g.maxInteractiveBurst)
	if chooseBackground {
		w := state.background[0]
		state.background = state.background[1:]
		state.interactiveBurst = 0
		return w
	}
	w := state.interactive[0]
	state.interactive = state.interactive[1:]
	if hasBackground {
		state.interactiveBurst++
	} else {
		state.interactiveBurst = 0
	}
	return w
}

func removeWaiter(state *resourceState, target *waiter) bool {
	queue := &state.background
	if target.priority == PriorityInteractive {
		queue = &state.interactive
	}
	for i, candidate := range *queue {
		if candidate != target {
			continue
		}
		copy((*queue)[i:], (*queue)[i+1:])
		(*queue)[len(*queue)-1] = nil
		*queue = (*queue)[:len(*queue)-1]
		if len(state.background) == 0 {
			state.interactiveBurst = 0
		}
		return true
	}
	return false
}

// ResourceMetrics is a read-only point-in-time copy. No payload text, model
// input, document name, or other user content is retained by the governor.
type ResourceMetrics struct {
	Capacity           int
	InUse              int
	QueuedInteractive  int
	QueuedBackground   int
	AcquireCount       uint64
	WaitCount          uint64
	CancelledWaitCount uint64
	WaitTotal          time.Duration
	WaitMax            time.Duration
	OldestQueuedWait   time.Duration
}

type MetricsSnapshot struct {
	At        time.Time
	Closed    bool
	Resources map[Resource]ResourceMetrics
}

func (g *Governor) Snapshot() MetricsSnapshot {
	if g == nil {
		return MetricsSnapshot{Resources: map[Resource]ResourceMetrics{}}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	snapshot := MetricsSnapshot{
		At: now, Closed: g.closed, Resources: make(map[Resource]ResourceMetrics, len(g.states)),
	}
	for resource, state := range g.states {
		metric := ResourceMetrics{
			Capacity: state.capacity, InUse: state.inUse,
			QueuedInteractive: len(state.interactive), QueuedBackground: len(state.background),
			AcquireCount: state.acquireCount, WaitCount: state.waitCount,
			CancelledWaitCount: state.cancelledWaitCount,
			WaitTotal:          state.waitTotal, WaitMax: state.waitMax,
		}
		for _, queue := range [][]*waiter{state.interactive, state.background} {
			if len(queue) == 0 {
				continue
			}
			wait := now.Sub(queue[0].queuedAt)
			if wait > metric.OldestQueuedWait {
				metric.OldestQueuedWait = wait
			}
		}
		snapshot.Resources[resource] = metric
	}
	return snapshot
}

// Close rejects new work and synchronously wakes every queued caller. Permits
// already handed to callers remain releasable and are reflected in metrics.
func (g *Governor) Close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	for _, state := range g.states {
		for _, queue := range [][]*waiter{state.interactive, state.background} {
			for _, w := range queue {
				w.err = ErrClosed
				close(w.ready)
			}
		}
		state.interactive = nil
		state.background = nil
		state.interactiveBurst = 0
	}
}
