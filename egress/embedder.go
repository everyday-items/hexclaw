package egress

import (
	"context"
	"fmt"
)

// VectorEmbedder is the minimal ai-core/hexagon embedding capability. It is
// declared locally to keep the policy package independent from a concrete RAG
// implementation.
type VectorEmbedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	EmbedOne(ctx context.Context, text string) ([]float32, error)
	Dimension() int
}

// CloudEmbedder enforces an egress envelope immediately before a remote
// embedding call. Local embedders should be used directly and never wrapped.
type CloudEmbedder struct {
	next   VectorEmbedder
	policy *Policy
}

func NewCloudEmbedder(next VectorEmbedder, policy *Policy) *CloudEmbedder {
	return &CloudEmbedder{next: next, policy: policy}
}

func (e *CloudEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := e.guard(ctx); err != nil {
		return nil, err
	}
	return e.next.Embed(ctx, texts)
}

func (e *CloudEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	if err := e.guard(ctx); err != nil {
		return nil, err
	}
	return e.next.EmbedOne(ctx, text)
}

func (e *CloudEmbedder) Dimension() int {
	if e == nil || e.next == nil {
		return 0
	}
	return e.next.Dimension()
}

func (e *CloudEmbedder) guard(ctx context.Context) error {
	if e == nil || e.next == nil {
		return fmt.Errorf("egress 拦截: cloud embedder 未注入")
	}
	if e.policy == nil {
		return fmt.Errorf("egress 拦截: embedding policy 未注入")
	}
	if err := e.policy.GuardContext(ctx); err != nil {
		return fmt.Errorf("cloud embedding egress: %w", err)
	}
	return nil
}
