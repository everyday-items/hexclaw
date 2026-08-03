package knowledge

import (
	"context"

	"github.com/hexagon-codes/hexagon"
)

// CapabilityPreservingCachedEmbedder keeps optional control-plane contracts
// visible while delegating data-plane calls to Hexagon's LRU cache. The cache
// intentionally remains outside readiness/admission: hits require neither an
// endpoint probe nor a local-inference permit, while misses reach the guarded
// physical provider.
type CapabilityPreservingCachedEmbedder struct {
	inner  hexagon.VectorEmbedder
	cached hexagon.VectorEmbedder
}

func NewCapabilityPreservingCachedEmbedder(
	inner hexagon.VectorEmbedder,
) *CapabilityPreservingCachedEmbedder {
	if inner == nil {
		panic("knowledge.NewCapabilityPreservingCachedEmbedder: inner must not be nil")
	}
	return &CapabilityPreservingCachedEmbedder{
		inner:  inner,
		cached: hexagon.NewCachedEmbedder(inner),
	}
}

func (e *CapabilityPreservingCachedEmbedder) Embed(
	ctx context.Context,
	texts []string,
) ([][]float32, error) {
	return e.cached.Embed(ctx, texts)
}

func (e *CapabilityPreservingCachedEmbedder) EmbedOne(
	ctx context.Context,
	text string,
) ([]float32, error) {
	return e.cached.EmbedOne(ctx, text)
}

func (e *CapabilityPreservingCachedEmbedder) Dimension() int {
	if e == nil || e.cached == nil {
		return 0
	}
	return e.cached.Dimension()
}

// Ready propagates endpoint availability without moving the readiness check
// in front of cache lookups. Callers may use it to suppress optional expansion;
// direct Embed calls can still serve an already cached vector while offline.
func (e *CapabilityPreservingCachedEmbedder) Ready(ctx context.Context) bool {
	if e == nil || e.inner == nil {
		return false
	}
	return EmbeddingReady(ctx, e.inner)
}

// LocalInferenceAdmissionAtProviderBoundary preserves the structural marker
// through the cache layer so higher-level retrieval code never double-acquires
// a permit around a provider-bound chain.
func (e *CapabilityPreservingCachedEmbedder) LocalInferenceAdmissionAtProviderBoundary() bool {
	return e != nil && hasProviderBoundEmbeddingAdmission(e.inner)
}
