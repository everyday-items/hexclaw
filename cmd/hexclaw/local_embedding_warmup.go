package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/localinfer"
)

const defaultLocalEmbeddingWarmupBudget = 120 * time.Second

type localEmbeddingWarmupHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func (h *localEmbeddingWarmupHandle) Cancel() {
	if h != nil && h.cancel != nil {
		h.cancel()
	}
}

func (h *localEmbeddingWarmupHandle) Wait(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-h.done:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *localEmbeddingWarmupHandle) finish(err error) {
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	close(h.done)
}

// startLocalEmbeddingWarmup starts asynchronously. The composition root chains
// chat warmup from this handle's terminal state, so startup never blocks and
// the two cold model loads still cannot overlap. The raw coordinated embedder
// reuses the context prelease, preserving exactly one physical admission.
func startLocalEmbeddingWarmup(
	ctx context.Context,
	coordinator *localinfer.Coordinator,
	embedder hexagon.VectorEmbedder,
	budget time.Duration,
) *localEmbeddingWarmupHandle {
	if ctx == nil {
		ctx = context.Background()
	}
	if budget <= 0 {
		budget = defaultLocalEmbeddingWarmupBudget
	}
	runCtx, cancel := context.WithTimeout(
		localinfer.WithOperation(ctx, localinfer.OperationWarmup), budget,
	)
	handle := &localEmbeddingWarmupHandle{cancel: cancel, done: make(chan struct{})}
	if coordinator == nil || embedder == nil {
		cancel()
		handle.finish(nil)
		return handle
	}
	go func() {
		var (
			lease  *localinfer.Lease
			runErr error
		)
		defer func() {
			if recovered := recover(); recovered != nil {
				runErr = fmt.Errorf("local embedding warmup panic: %v", recovered)
			}
			if lease != nil {
				lease.Finish(runErr)
			}
			cancel()
			handle.finish(runErr)
		}()
		leaseCtx, acquired, acquireErr := coordinator.Acquire(runCtx, localinfer.OperationWarmup)
		if acquireErr != nil {
			runErr = acquireErr
			return
		}
		lease = acquired
		vectors, embedErr := embedder.Embed(leaseCtx, []string{knowledgeEmbeddingProbeText})
		runErr = embedErr
		if len(vectors) > 0 {
			lease.MarkFirstOutput()
		}
	}()
	return handle
}

// afterLocalEmbeddingWarmup serializes cold loads without spending the chat
// model's own execution budget in the embedding queue. Cancellation prevents a
// late chat warmup from starting during shutdown.
func afterLocalEmbeddingWarmup(
	ctx context.Context,
	handle *localEmbeddingWarmupHandle,
	start func(error),
) {
	if start == nil {
		return
	}
	if handle == nil {
		start(nil)
		return
	}
	go func() {
		err := handle.Wait(ctx)
		if ctx != nil && ctx.Err() != nil {
			return
		}
		start(err)
	}()
}

// startSerialLocalWarmups starts the embedding warmup immediately and chains
// chat warmup from its terminal state. startChat receives the original parent
// context rather than the embedding timeout context, so the chat budget begins
// only when its own starter runs.
func startSerialLocalWarmups(
	ctx context.Context,
	coordinator *localinfer.Coordinator,
	embedder hexagon.VectorEmbedder,
	embeddingBudget time.Duration,
	onEmbeddingDone func(error),
	startChat func(context.Context),
) *localEmbeddingWarmupHandle {
	if ctx == nil {
		ctx = context.Background()
	}
	handle := startLocalEmbeddingWarmup(ctx, coordinator, embedder, embeddingBudget)
	afterLocalEmbeddingWarmup(ctx, handle, func(err error) {
		if onEmbeddingDone != nil {
			onEmbeddingDone(err)
		}
		if startChat != nil {
			startChat(ctx)
		}
	})
	return handle
}
