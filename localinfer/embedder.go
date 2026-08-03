package localinfer

import (
	"context"
	"errors"
)

var ErrEmbedderNotConfigured = errors.New("local inference: embedder is not configured")

// Embedder is the narrow ai-core/hexagon vector boundary. Keeping the local
// interface here avoids coupling the scheduler to a concrete provider SDK.
type Embedder interface {
	Embed(context.Context, []string) ([][]float32, error)
	EmbedOne(context.Context, string) ([]float32, error)
	Dimension() int
}

type CoordinatedEmbedder struct {
	next        Embedder
	coordinator *Coordinator
	fallback    Operation
}

// ProviderBoundEmbedder marks a fully decorated embedding chain whose physical
// local-inference admission sits inside its cache. Higher layers can discover
// this capability instead of relying on a manually synchronized boolean.
type ProviderBoundEmbedder struct{ next Embedder }

func MarkProviderBoundEmbedder(next Embedder) *ProviderBoundEmbedder {
	return &ProviderBoundEmbedder{next: next}
}

func (*ProviderBoundEmbedder) LocalInferenceAdmissionAtProviderBoundary() bool { return true }

func (e *ProviderBoundEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.next.Embed(ctx, texts)
}

func (e *ProviderBoundEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	return e.next.EmbedOne(ctx, text)
}

func (e *ProviderBoundEmbedder) Dimension() int {
	if e == nil || e.next == nil {
		return 0
	}
	return e.next.Dimension()
}

// Ready preserves the optional readiness contract through the marker. A
// provider-bound chain is commonly the outermost Manager/Memory embedder; if
// this capability is erased, callers incorrectly treat an offline local model
// as ready and start avoidable query expansion and cache-miss work.
func (e *ProviderBoundEmbedder) Ready(ctx context.Context) bool {
	if e == nil || e.next == nil {
		return false
	}
	readiness, ok := e.next.(interface {
		Ready(context.Context) bool
	})
	return !ok || readiness.Ready(ctx)
}

// NewCoordinatedEmbedder must be placed immediately outside the raw local
// provider and inside caches/readiness decorators. Cache hits then consume no
// physical permit, while every actual model miss is covered.
func NewCoordinatedEmbedder(next Embedder, coordinator *Coordinator, fallback Operation) *CoordinatedEmbedder {
	return &CoordinatedEmbedder{next: next, coordinator: coordinator, fallback: fallback}
}

func (e *CoordinatedEmbedder) Embed(ctx context.Context, texts []string) (vectors [][]float32, err error) {
	if e == nil || e.next == nil || e.coordinator == nil {
		return nil, ErrEmbedderNotConfigured
	}
	operation := OperationFromContext(ctx, e.fallback)
	callCtx, lease, err := e.coordinator.Acquire(WithOperation(ctx, operation), operation)
	if err != nil {
		return nil, err
	}
	defer func() { lease.Finish(err) }()
	vectors, err = e.next.Embed(callCtx, texts)
	if len(vectors) > 0 {
		lease.MarkFirstOutput()
	}
	return vectors, err
}

func (e *CoordinatedEmbedder) EmbedOne(ctx context.Context, text string) (vector []float32, err error) {
	if e == nil || e.next == nil || e.coordinator == nil {
		return nil, ErrEmbedderNotConfigured
	}
	operation := OperationFromContext(ctx, e.fallback)
	callCtx, lease, err := e.coordinator.Acquire(WithOperation(ctx, operation), operation)
	if err != nil {
		return nil, err
	}
	defer func() { lease.Finish(err) }()
	vector, err = e.next.EmbedOne(callCtx, text)
	if len(vector) > 0 {
		lease.MarkFirstOutput()
	}
	return vector, err
}

func (e *CoordinatedEmbedder) Dimension() int {
	if e == nil || e.next == nil {
		return 0
	}
	return e.next.Dimension()
}
