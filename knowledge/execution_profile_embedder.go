package knowledge

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/localinfer"
)

// ExecutionProfileEmbedder is the model-capacity boundary shared by every
// consumer of one exact embedding model. It shapes actual provider calls by
// count and aggregate runes, applies the calibrated per-call ceiling, and
// preserves readiness. Business stages may install a shorter parent deadline;
// this decorator never extends it.
//
// Place it outside cache/readiness/admission decorators. Cache hits then avoid
// physical admission, while misses still reach the provider in profile-sized
// batches.
type ExecutionProfileEmbedder struct {
	inner   hexagon.VectorEmbedder
	profile EmbeddingExecutionProfile
}

func NewExecutionProfileEmbedder(
	inner hexagon.VectorEmbedder,
	profile EmbeddingExecutionProfile,
) *ExecutionProfileEmbedder {
	if inner == nil {
		panic("knowledge.NewExecutionProfileEmbedder: inner must not be nil")
	}
	if profile.MaxInputRunes <= 0 || profile.BatchMaxCount <= 0 ||
		profile.BatchMaxRunes < profile.MaxInputRunes || profile.BatchTimeout <= 0 ||
		profile.QueryTimeout <= 0 {
		panic("knowledge.NewExecutionProfileEmbedder: invalid execution profile")
	}
	return &ExecutionProfileEmbedder{inner: inner, profile: profile}
}

func (e *ExecutionProfileEmbedder) Embed(
	ctx context.Context,
	texts []string,
) ([][]float32, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(texts) == 0 {
		return nil, nil
	}
	prepared := make([]string, len(texts))
	for i, text := range texts {
		prepared[i] = clampRunes(text, e.profile.MaxInputRunes)
	}

	result := make([][]float32, 0, len(prepared))
	for start := 0; start < len(prepared); {
		end, totalRunes := start, 0
		for end < len(prepared) && end-start < e.profile.BatchMaxCount {
			inputRunes := utf8.RuneCountInString(prepared[end])
			if end > start && totalRunes+inputRunes > e.profile.BatchMaxRunes {
				break
			}
			totalRunes += inputRunes
			end++
		}
		if end == start {
			return nil, fmt.Errorf("knowledge: embedding profile could not shape input batch")
		}

		batchCtx, cancel := context.WithTimeout(ctx, e.timeoutFor(ctx))
		vectors, err := e.inner.Embed(batchCtx, prepared[start:end])
		cancel()
		if err != nil {
			return nil, err
		}
		if len(vectors) != end-start {
			return nil, fmt.Errorf(
				"knowledge: embedding count mismatch: got %d, want %d",
				len(vectors), end-start,
			)
		}
		result = append(result, vectors...)
		start = end
	}
	return result, nil
}

func (e *ExecutionProfileEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("knowledge: embedding returned no vector")
	}
	return vectors[0], nil
}

func (e *ExecutionProfileEmbedder) Dimension() int {
	if e == nil || e.inner == nil {
		return 0
	}
	return e.inner.Dimension()
}

func (e *ExecutionProfileEmbedder) Ready(ctx context.Context) bool {
	if e == nil || e.inner == nil {
		return false
	}
	return EmbeddingReady(ctx, e.inner)
}

func (e *ExecutionProfileEmbedder) timeoutFor(ctx context.Context) time.Duration {
	switch localinfer.OperationFromContext(ctx, localinfer.OperationQueryEmbedding) {
	case localinfer.OperationDocumentEmbedding, localinfer.OperationWarmup:
		return e.profile.BatchTimeout
	default:
		return e.profile.QueryTimeout
	}
}
