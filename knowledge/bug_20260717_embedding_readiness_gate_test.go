package knowledge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

type readinessGateEmbedder struct {
	calls atomic.Int32
	err   error
}

func (e *readinessGateEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func (e *readinessGateEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	out, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

func (*readinessGateEmbedder) Dimension() int { return 3 }

func TestBug20260717_ReadinessGateDoesNotCallUnavailableEmbeddingEndpoint(t *testing.T) {
	inner := &readinessGateEmbedder{}
	ready := atomic.Bool{}
	gated := NewReadinessGatedEmbedder(inner, func(context.Context) bool { return ready.Load() }, false, 0)

	if _, err := gated.Embed(context.Background(), []string{"query"}); !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Fatalf("Embed error = %v, want ErrEmbeddingUnavailable", err)
	}
	if got := inner.calls.Load(); got != 0 {
		t.Fatalf("unavailable endpoint calls = %d, want 0", got)
	}
}

func TestBug20260717_ReadinessGateActivatesWithoutRestartAfterModelInstall(t *testing.T) {
	inner := &readinessGateEmbedder{}
	ready := atomic.Bool{}
	gated := NewReadinessGatedEmbedder(inner, func(context.Context) bool { return ready.Load() }, false, 0)

	if gated.Ready(context.Background()) {
		t.Fatal("gate must start unavailable")
	}
	ready.Store(true)

	got, err := gated.Embed(context.Background(), []string{"query"})
	if err != nil {
		t.Fatalf("Embed after install: %v", err)
	}
	if len(got) != 1 || len(got[0]) != inner.Dimension() {
		t.Fatalf("unexpected embedding shape: %#v", got)
	}
	if calls := inner.calls.Load(); calls != 1 {
		t.Fatalf("endpoint calls = %d, want 1", calls)
	}
}

func TestBug20260717_ReadinessGateOpensAfterEndpointFailure(t *testing.T) {
	inner := &readinessGateEmbedder{err: errors.New("connection refused")}
	ready := atomic.Bool{}
	ready.Store(true)
	gated := NewReadinessGatedEmbedder(inner, func(context.Context) bool { return ready.Load() }, true, 0)

	if _, err := gated.Embed(context.Background(), []string{"first"}); err == nil {
		t.Fatal("first endpoint failure must be returned")
	}
	ready.Store(false)
	if _, err := gated.Embed(context.Background(), []string{"second"}); !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Fatalf("second Embed error = %v, want ErrEmbeddingUnavailable", err)
	}
	if calls := inner.calls.Load(); calls != 1 {
		t.Fatalf("open gate must suppress repeated endpoint calls, got %d", calls)
	}
}
