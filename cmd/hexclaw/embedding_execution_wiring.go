package main

import (
	"context"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/localinfer"
)

// wrapKnowledgeEmbeddingExecutionProfile installs the calibrated physical-call
// boundary for a known exact model. Unknown compatible models retain the
// conservative legacy truncation boundary until they have an explicit profile.
func wrapKnowledgeEmbeddingExecutionProfile(
	inner hexagon.VectorEmbedder,
	model string,
) hexagon.VectorEmbedder {
	if profile, ok := knowledge.EmbeddingExecutionProfileForModel(model); ok {
		return knowledge.NewExecutionProfileEmbedder(inner, profile)
	}
	return knowledge.NewTruncatingEmbedder(inner, 0)
}

// assembleKnowledgeSharedEmbedder keeps the physical-provider decorators in a
// single, testable order:
//
//	profile -> cache -> readiness -> local admission -> provider
//
// A cache hit therefore performs neither readiness I/O nor local admission;
// only a physical miss reaches those boundaries. The provider-bound marker is
// outermost so Manager/revision callers cannot accidentally acquire twice.
func assembleKnowledgeSharedEmbedder(
	provider hexagon.VectorEmbedder,
	model string,
	local bool,
	nativeOllama bool,
	coordinator *localinfer.Coordinator,
	initialReady bool,
	readinessProbe func(context.Context) bool,
) hexagon.VectorEmbedder {
	if provider == nil {
		return nil
	}
	var physical hexagon.VectorEmbedder = provider
	if local && coordinator != nil {
		physical = localinfer.NewCoordinatedEmbedder(
			physical, coordinator, localinfer.OperationQueryEmbedding,
		)
	}
	if nativeOllama {
		if readinessProbe == nil {
			panic("assembleKnowledgeSharedEmbedder: native Ollama readiness probe is required")
		}
		physical = knowledge.NewReadinessGatedEmbedder(
			physical, readinessProbe, initialReady, 5*time.Second,
		)
	}
	result := wrapKnowledgeEmbeddingExecutionProfile(
		knowledge.NewCapabilityPreservingCachedEmbedder(physical), model,
	)
	if local && coordinator != nil {
		result = localinfer.MarkProviderBoundEmbedder(result)
	}
	return result
}
