package knowledge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hexagon-codes/hexagon"
	"golang.org/x/sync/singleflight"
)

// ErrEmbeddingUnavailable means the configured embedding capability is in
// standby (for example, Ollama is stopped or its embedding model is not
// installed). Callers should degrade to lexical retrieval without treating it
// as a request failure.
var ErrEmbeddingUnavailable = errors.New("knowledge: embedding unavailable")

// EmbeddingReady lets decorators and retrieval callers discover optional
// readiness without coupling to a concrete embedder implementation. Embedders
// that do not implement Ready are treated as available.
func EmbeddingReady(ctx context.Context, embedder any) bool {
	if embedder == nil {
		return false
	}
	if readiness, ok := embedder.(interface {
		Ready(context.Context) bool
	}); ok {
		return readiness.Ready(ctx)
	}
	return true
}

// ReadinessGatedEmbedder keeps an optional embedding endpoint out of the hot
// path while it is unavailable, but re-probes it so a locally installed model
// can become active without restarting HexClaw.
//
// Place this decorator inside the embedding cache: cached vectors remain usable
// while the endpoint is down, and only cache misses consult readiness.
type ReadinessGatedEmbedder struct {
	inner         hexagon.VectorEmbedder
	probe         func(context.Context) bool
	retryInterval time.Duration

	mu        sync.RWMutex
	ready     bool
	nextProbe time.Time
	probes    singleflight.Group
}

func NewReadinessGatedEmbedder(
	inner hexagon.VectorEmbedder,
	probe func(context.Context) bool,
	initialReady bool,
	retryInterval time.Duration,
) *ReadinessGatedEmbedder {
	if inner == nil {
		panic("knowledge.NewReadinessGatedEmbedder: inner must not be nil")
	}
	if probe == nil {
		panic("knowledge.NewReadinessGatedEmbedder: probe must not be nil")
	}
	if retryInterval < 0 {
		retryInterval = 0
	}
	gated := &ReadinessGatedEmbedder{
		inner:         inner,
		probe:         probe,
		retryInterval: retryInterval,
		ready:         initialReady,
	}
	if !initialReady && retryInterval > 0 {
		// Startup resolution already performed the same probe. Do not repeat a
		// potentially slow health request on the first user message.
		gated.nextProbe = time.Now().Add(retryInterval)
	}
	return gated
}

// Ready reports whether a real embedding request may run. While unavailable,
// at most one caller performs the periodic readiness probe.
func (e *ReadinessGatedEmbedder) Ready(ctx context.Context) bool {
	e.mu.RLock()
	if e.ready {
		e.mu.RUnlock()
		return true
	}
	nextProbe := e.nextProbe
	e.mu.RUnlock()
	if !nextProbe.IsZero() && time.Now().Before(nextProbe) {
		return false
	}

	value, _, _ := e.probes.Do("readiness", func() (any, error) {
		e.mu.RLock()
		if e.ready {
			e.mu.RUnlock()
			return true, nil
		}
		next := e.nextProbe
		e.mu.RUnlock()
		if !next.IsZero() && time.Now().Before(next) {
			return false, nil
		}

		ready := e.probe(ctx)
		e.mu.Lock()
		e.ready = ready
		if ready {
			e.nextProbe = time.Time{}
		} else {
			e.nextProbe = time.Now().Add(e.retryInterval)
		}
		e.mu.Unlock()
		return ready, nil
	})
	ready, _ := value.(bool)
	return ready
}

// ObserveReady synchronizes an out-of-band control-plane probe with the data
// gate. Semantic profile Apply probes use this to ensure a freshly observed
// outage suppresses active search/worker cache misses for the same TTL instead
// of allowing one uncontrolled request per caller.
func (e *ReadinessGatedEmbedder) ObserveReady(ready bool) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.ready = ready
	if ready {
		e.nextProbe = time.Time{}
	} else {
		e.nextProbe = time.Now().Add(e.retryInterval)
	}
	e.mu.Unlock()
}

func (e *ReadinessGatedEmbedder) markUnavailable() {
	e.ObserveReady(false)
}

func (e *ReadinessGatedEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if !e.Ready(ctx) {
		return nil, ErrEmbeddingUnavailable
	}
	out, err := e.inner.Embed(ctx, texts)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		e.markUnavailable()
	}
	return out, err
}

func (e *ReadinessGatedEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	if !e.Ready(ctx) {
		return nil, ErrEmbeddingUnavailable
	}
	out, err := e.inner.EmbedOne(ctx, text)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		e.markUnavailable()
	}
	return out, err
}

func (e *ReadinessGatedEmbedder) Dimension() int { return e.inner.Dimension() }
