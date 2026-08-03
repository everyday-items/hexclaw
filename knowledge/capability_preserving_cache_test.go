package knowledge

import (
	"context"
	"testing"
)

type capabilityCacheEmbedderDouble struct {
	ready bool
	calls int
}

func (e *capabilityCacheEmbedderDouble) Embed(
	_ context.Context,
	texts []string,
) ([][]float32, error) {
	e.calls++
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{1, 0, 0}
	}
	return vectors, nil
}

func (e *capabilityCacheEmbedderDouble) EmbedOne(
	ctx context.Context,
	text string,
) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (*capabilityCacheEmbedderDouble) Dimension() int { return 3 }

func (e *capabilityCacheEmbedderDouble) Ready(context.Context) bool { return e.ready }

func (*capabilityCacheEmbedderDouble) LocalInferenceAdmissionAtProviderBoundary() bool {
	return true
}

func TestCapabilityPreservingCachedEmbedderPropagatesOptionalContracts(t *testing.T) {
	inner := &capabilityCacheEmbedderDouble{ready: false}
	cached := NewCapabilityPreservingCachedEmbedder(inner)

	if EmbeddingReady(context.Background(), cached) {
		t.Fatal("cache erased unavailable readiness")
	}
	if !hasProviderBoundEmbeddingAdmission(cached) {
		t.Fatal("cache erased provider-bound admission marker")
	}
}

func TestCapabilityPreservingCachedEmbedderServesHitWhileInnerReportsUnavailable(t *testing.T) {
	inner := &capabilityCacheEmbedderDouble{ready: true}
	cached := NewCapabilityPreservingCachedEmbedder(inner)
	if _, err := cached.EmbedOne(context.Background(), "same"); err != nil {
		t.Fatal(err)
	}
	inner.ready = false
	if _, err := cached.EmbedOne(context.Background(), "same"); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls=%d, want one cache miss", inner.calls)
	}
}
