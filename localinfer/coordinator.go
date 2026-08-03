// Package localinfer coordinates every physical call that competes for local
// model-runtime capacity. It carries scheduling metadata only; prompts,
// documents, endpoints, credentials, and error strings never enter metrics.
package localinfer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexclaw/resourcegov"
)

var ErrLeaseConflict = errors.New("local inference: nested operation conflicts with active lease")

// Operation is a fixed, privacy-safe scheduling lane.
type Operation string

const (
	OperationQueryEmbedding    Operation = "query_embedding"
	OperationChat              Operation = "chat"
	OperationRerank            Operation = "rerank"
	OperationDocumentEmbedding Operation = "document_embedding"
	OperationWarmup            Operation = "warmup"
	OperationProbe             Operation = "probe"
)

var allOperations = [...]Operation{
	OperationQueryEmbedding,
	OperationChat,
	OperationRerank,
	OperationDocumentEmbedding,
	OperationWarmup,
	OperationProbe,
}

func priorityFor(operation Operation) (resourcegov.Priority, error) {
	switch operation {
	case OperationQueryEmbedding:
		return resourcegov.PriorityQuery, nil
	case OperationChat:
		return resourcegov.PriorityInteractive, nil
	case OperationRerank:
		return resourcegov.PriorityRerank, nil
	case OperationDocumentEmbedding, OperationWarmup, OperationProbe:
		return resourcegov.PriorityBackground, nil
	default:
		return 0, fmt.Errorf("local inference: unknown operation %q", operation)
	}
}

// OperationMetrics contains process-lifetime aggregates only.
type OperationMetrics struct {
	Attempts           uint64  `json:"attempts"`
	Admitted           uint64  `json:"admitted"`
	Completed          uint64  `json:"completed"`
	Failed             uint64  `json:"failed"`
	Cancelled          uint64  `json:"cancelled"`
	QueueWaitTotalMS   float64 `json:"queue_wait_total_ms"`
	QueueWaitMaxMS     float64 `json:"queue_wait_max_ms"`
	FirstOutputCount   uint64  `json:"first_output_count"`
	FirstOutputTotalMS float64 `json:"first_output_total_ms"`
	FirstOutputMaxMS   float64 `json:"first_output_max_ms"`
	GenerationCount    uint64  `json:"generation_count"`
	GenerationTotalMS  float64 `json:"generation_total_ms"`
	GenerationMaxMS    float64 `json:"generation_max_ms"`
	TotalDurationMS    float64 `json:"total_duration_ms"`
	TotalDurationMaxMS float64 `json:"total_duration_max_ms"`
}

type MetricsSnapshot struct {
	// ModelLoadAvailable remains false until a provider exposes authoritative
	// load duration. Total latency is never relabelled as model-load time.
	ModelLoadAvailable bool                           `json:"model_load_available"`
	Operations         map[Operation]OperationMetrics `json:"operations"`
}

type operationCounter struct {
	attempts, admitted, completed, failed, cancelled uint64
	queueWaitTotal, queueWaitMax                     time.Duration
	firstOutputCount                                 uint64
	firstOutputTotal, firstOutputMax                 time.Duration
	generationCount                                  uint64
	generationTotal, generationMax                   time.Duration
	totalDuration, totalDurationMax                  time.Duration
}

// Coordinator reuses the one process-scoped governor. It deliberately does
// not split loopback endpoints or models into separate capacity pools: without
// an independently attested device boundary, all local runtimes are assumed to
// compete for the same accelerator/unified-memory resource.
type Coordinator struct {
	governor *resourcegov.Governor
	now      func() time.Time
	mu       sync.Mutex
	counters map[Operation]*operationCounter
}

func New(governor *resourcegov.Governor) *Coordinator {
	return &Coordinator{governor: governor, now: time.Now, counters: make(map[Operation]*operationCounter)}
}

type leaseContextKey struct{}
type operationContextKey struct{}

// WithOperation annotates a call chain with fixed scheduling metadata only.
func WithOperation(ctx context.Context, operation Operation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := priorityFor(operation); err != nil {
		return ctx
	}
	return context.WithValue(ctx, operationContextKey{}, operation)
}

// OperationFromContext returns fallback for unstamped or invalid contexts.
func OperationFromContext(ctx context.Context, fallback Operation) Operation {
	if ctx != nil {
		if operation, ok := ctx.Value(operationContextKey{}).(Operation); ok {
			if _, err := priorityFor(operation); err == nil {
				return operation
			}
		}
	}
	return fallback
}

type leaseState struct {
	coordinator *Coordinator
	operation   Operation
	permit      *resourcegov.Permit
	admittedAt  time.Time
	firstOutput atomic.Int64
	active      atomic.Bool
	lifecycleMu sync.Mutex
	borrowUsed  bool
	borrowLive  bool
	ownerDone   bool
	ownerErr    error
	finalize    sync.Once
}

// Lease is either the owner of one physical permit or a borrowed view created
// when a lower wrapper recognizes the same active context lease.
type Lease struct {
	state    *leaseState
	owner    bool
	borrower bool
	once     sync.Once
}

// Acquire returns a context carrying an unforgeable in-process lease token.
// A nested call for the same coordinator/operation reuses that token and does
// not acquire or release capacity a second time.
func (c *Coordinator) Acquire(ctx context.Context, operation Operation) (context.Context, *Lease, error) {
	if c == nil || c.governor == nil {
		return ctx, nil, errors.New("local inference: coordinator is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := priorityFor(operation); err != nil {
		return ctx, nil, err
	}
	if existing, _ := ctx.Value(leaseContextKey{}).(*leaseState); existing != nil &&
		existing.coordinator == c && existing.active.Load() {
		existing.lifecycleMu.Lock()
		if !existing.active.Load() {
			existing.lifecycleMu.Unlock()
		} else if existing.operation != operation {
			existing.lifecycleMu.Unlock()
			return ctx, nil, fmt.Errorf("%w: have=%s want=%s", ErrLeaseConflict, existing.operation, operation)
		} else if existing.ownerDone {
			existing.lifecycleMu.Unlock()
			return ctx, nil, fmt.Errorf("%w: owner already finished for %s", ErrLeaseConflict, operation)
		} else if existing.borrowUsed {
			existing.lifecycleMu.Unlock()
			return ctx, nil, fmt.Errorf("%w: prelease already consumed for %s", ErrLeaseConflict, operation)
		} else {
			existing.borrowUsed = true
			existing.borrowLive = true
			existing.lifecycleMu.Unlock()
			return ctx, &Lease{state: existing, borrower: true}, nil
		}
	}

	started := c.now()
	c.observeAttempt(operation)
	priority, _ := priorityFor(operation)
	permit, err := c.governor.Acquire(ctx, resourcegov.ResourceLocalInference, priority)
	wait := nonNegativeDuration(c.now().Sub(started))
	if err != nil {
		c.observeAdmissionFailure(operation, wait, err)
		return ctx, nil, err
	}
	state := &leaseState{
		coordinator: c,
		operation:   operation,
		permit:      permit,
		admittedAt:  c.now(),
	}
	state.active.Store(true)
	c.observeAdmitted(operation, wait)
	return context.WithValue(ctx, leaseContextKey{}, state), &Lease{state: state, owner: true}, nil
}

// MarkFirstOutput records TTFT/first-vector latency once for the physical call.
func (l *Lease) MarkFirstOutput() {
	if l == nil || l.state == nil || !l.state.active.Load() {
		return
	}
	now := l.state.coordinator.now()
	elapsed := nonNegativeDuration(now.Sub(l.state.admittedAt))
	if l.state.firstOutput.CompareAndSwap(0, int64(elapsed)+1) {
		l.state.coordinator.observeFirstOutput(l.state.operation, elapsed)
	}
}

// Finish releases an owned permit exactly once and records a classified
// terminal outcome. Borrowed leases intentionally do nothing.
func (l *Lease) Finish(err error) {
	if l == nil || l.state == nil {
		return
	}
	l.once.Do(func() {
		if l.borrower {
			l.finishBorrow()
			return
		}
		if l.owner {
			l.finishOwner(err)
		}
	})
}

func (l *Lease) Release() { l.Finish(nil) }

func (l *Lease) finishOwner(err error) {
	state := l.state
	state.lifecycleMu.Lock()
	state.ownerDone = true
	state.ownerErr = err
	ready := !state.borrowLive
	state.lifecycleMu.Unlock()
	if ready {
		state.finish(err)
	}
}

func (l *Lease) finishBorrow() {
	state := l.state
	state.lifecycleMu.Lock()
	state.borrowLive = false
	ready := state.ownerDone
	err := state.ownerErr
	state.lifecycleMu.Unlock()
	if ready {
		state.finish(err)
	}
}

func (s *leaseState) finish(err error) {
	s.finalize.Do(func() {
		now := s.coordinator.now()
		total := nonNegativeDuration(now.Sub(s.admittedAt))
		firstEncoded := s.firstOutput.Load()
		var generation time.Duration
		if firstEncoded > 0 {
			first := time.Duration(firstEncoded - 1)
			generation = nonNegativeDuration(total - first)
		}
		s.active.Store(false)
		s.permit.Release()
		s.coordinator.observeFinished(s.operation, total, generation, firstEncoded > 0, err)
	})
}

func nonNegativeDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}

func (c *Coordinator) counterLocked(operation Operation) *operationCounter {
	counter := c.counters[operation]
	if counter == nil {
		counter = &operationCounter{}
		c.counters[operation] = counter
	}
	return counter
}

func (c *Coordinator) observeAttempt(operation Operation) {
	c.mu.Lock()
	c.counterLocked(operation).attempts++
	c.mu.Unlock()
}

func (c *Coordinator) observeAdmissionFailure(operation Operation, wait time.Duration, err error) {
	c.mu.Lock()
	counter := c.counterLocked(operation)
	counter.queueWaitTotal += wait
	if wait > counter.queueWaitMax {
		counter.queueWaitMax = wait
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		counter.cancelled++
	} else {
		counter.failed++
	}
	c.mu.Unlock()
}

func (c *Coordinator) observeAdmitted(operation Operation, wait time.Duration) {
	c.mu.Lock()
	counter := c.counterLocked(operation)
	counter.admitted++
	counter.queueWaitTotal += wait
	if wait > counter.queueWaitMax {
		counter.queueWaitMax = wait
	}
	c.mu.Unlock()
}

func (c *Coordinator) observeFirstOutput(operation Operation, duration time.Duration) {
	c.mu.Lock()
	counter := c.counterLocked(operation)
	counter.firstOutputCount++
	counter.firstOutputTotal += duration
	if duration > counter.firstOutputMax {
		counter.firstOutputMax = duration
	}
	c.mu.Unlock()
}

func (c *Coordinator) observeFinished(
	operation Operation,
	total, generation time.Duration,
	hasFirstOutput bool,
	err error,
) {
	c.mu.Lock()
	counter := c.counterLocked(operation)
	counter.completed++
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			counter.cancelled++
		} else {
			counter.failed++
		}
	}
	counter.totalDuration += total
	if total > counter.totalDurationMax {
		counter.totalDurationMax = total
	}
	if hasFirstOutput {
		counter.generationCount++
		counter.generationTotal += generation
		if generation > counter.generationMax {
			counter.generationMax = generation
		}
	}
	c.mu.Unlock()
}

func durationMillis(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}

// Snapshot returns a content-free copy safe for local diagnostic APIs.
func (c *Coordinator) Snapshot() MetricsSnapshot {
	snapshot := MetricsSnapshot{Operations: make(map[Operation]OperationMetrics, len(allOperations))}
	if c == nil {
		return snapshot
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, operation := range allOperations {
		counter := c.counters[operation]
		if counter == nil {
			snapshot.Operations[operation] = OperationMetrics{}
			continue
		}
		snapshot.Operations[operation] = OperationMetrics{
			Attempts: counter.attempts, Admitted: counter.admitted,
			Completed: counter.completed, Failed: counter.failed, Cancelled: counter.cancelled,
			QueueWaitTotalMS: durationMillis(counter.queueWaitTotal), QueueWaitMaxMS: durationMillis(counter.queueWaitMax),
			FirstOutputCount: counter.firstOutputCount, FirstOutputTotalMS: durationMillis(counter.firstOutputTotal),
			FirstOutputMaxMS: durationMillis(counter.firstOutputMax), GenerationCount: counter.generationCount,
			GenerationTotalMS: durationMillis(counter.generationTotal), GenerationMaxMS: durationMillis(counter.generationMax),
			TotalDurationMS: durationMillis(counter.totalDuration), TotalDurationMaxMS: durationMillis(counter.totalDurationMax),
		}
	}
	return snapshot
}
