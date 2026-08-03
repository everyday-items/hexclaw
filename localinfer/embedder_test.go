package localinfer

import (
	"context"
	"testing"
)

type embedderDouble struct {
	dimension int
	calls     int
	seen      Operation
	ready     bool
}

func (e *embedderDouble) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls++
	e.seen = OperationFromContext(ctx, "")
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = make([]float32, e.dimension)
	}
	return result, nil
}

func (e *embedderDouble) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{text})
	if err != nil || len(vectors) == 0 {
		return nil, err
	}
	return vectors[0], nil
}

func (e *embedderDouble) Dimension() int { return e.dimension }

func (e *embedderDouble) Ready(context.Context) bool { return e.ready }

func TestCoordinatedEmbedderReusesWorkerPrelease(t *testing.T) {
	governor := newTestGovernor(t)
	coordinator := New(governor)
	next := &embedderDouble{dimension: 2}
	embedder := NewCoordinatedEmbedder(next, coordinator, OperationQueryEmbedding)

	leaseCtx, lease, err := coordinator.Acquire(context.Background(), OperationDocumentEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	leaseCtx = WithOperation(leaseCtx, OperationDocumentEmbedding)
	if _, err := embedder.Embed(leaseCtx, []string{"document"}); err != nil {
		t.Fatal(err)
	}
	if next.calls != 1 || next.seen != OperationDocumentEmbedding {
		t.Fatalf("raw embedder calls=%d operation=%s", next.calls, next.seen)
	}
	metric := governor.Snapshot().Resources["embedding_accelerator"]
	if metric.AcquireCount != 1 || metric.InUse != 1 {
		t.Fatalf("raw wrapper double-acquired worker lease: %+v", metric)
	}
	lease.Release()
}

func TestCoordinatedEmbedderCoversUnstampedEmbeddingByFallback(t *testing.T) {
	governor := newTestGovernor(t)
	coordinator := New(governor)
	next := &embedderDouble{dimension: 3}
	embedder := NewCoordinatedEmbedder(next, coordinator, OperationQueryEmbedding)

	vector, err := embedder.EmbedOne(context.Background(), "memory recall")
	if err != nil {
		t.Fatal(err)
	}
	if len(vector) != 3 || next.seen != OperationQueryEmbedding {
		t.Fatalf("vector=%v operation=%s", vector, next.seen)
	}
	metric := governor.Snapshot().Resources["embedding_accelerator"]
	if metric.AcquireCount != 1 || metric.InUse != 0 {
		t.Fatalf("fallback embedding admission metric: %+v", metric)
	}
}

func TestProviderBoundEmbedderPreservesReadinessCapability(t *testing.T) {
	marked := MarkProviderBoundEmbedder(&embedderDouble{dimension: 3, ready: false})
	readiness, ok := any(marked).(interface {
		Ready(context.Context) bool
	})
	if !ok {
		t.Fatal("provider-bound marker erased the wrapped readiness capability")
	}
	if readiness.Ready(context.Background()) {
		t.Fatal("provider-bound marker reported an unavailable embedder as ready")
	}
}
