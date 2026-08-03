package main

import (
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/knowledge"
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
