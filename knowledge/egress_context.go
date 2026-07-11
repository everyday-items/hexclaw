package knowledge

import (
	"context"

	"github.com/hexagon-codes/hexclaw/egress"
)

// ragEmbedContext starts an isolated, explicitly classified request envelope
// for a query or document embedding call. The cloud boundary rejects missing
// envelopes, so every knowledge-owned embedding path must use this helper.
func ragEmbedContext(ctx context.Context) context.Context {
	if hasRAGEgressContext(ctx, egress.PurposeRAGEmbed) {
		return ctx
	}
	return egress.WithRequest(ctx, egress.PurposeRAGEmbed, "", egress.ClassDocument)
}

// sharedEmbedderContext is for generic decorators reused by both the knowledge
// base and private-memory recall. A semantic caller's existing classification
// must win; otherwise relabeling ClassMemory as ClassDocument would turn a
// policy denial into an allowed cloud request. Direct decorator users without
// an envelope retain the historical/document default.
func sharedEmbedderContext(ctx context.Context) context.Context {
	if requests, ok := egress.RequestsFromContext(ctx); ok && len(requests) > 0 {
		return ctx
	}
	return egress.WithRequest(ctx, egress.PurposeRAGEmbed, "", egress.ClassDocument)
}

// ragEnrichContext classifies RAG auxiliary model calls (query rewrite/HyDE,
// contextual summaries, reranking, captioning, judging and answer synthesis).
func ragEnrichContext(ctx context.Context) context.Context {
	if hasRAGEgressContext(ctx, egress.PurposeRAGEnrich) {
		return ctx
	}
	return egress.WithRequest(ctx, egress.PurposeRAGEnrich, "", egress.ClassDocument)
}

func hasRAGEgressContext(ctx context.Context, purpose egress.Purpose) bool {
	requests, ok := egress.RequestsFromContext(ctx)
	return ok && len(requests) == 1 &&
		requests[0].Purpose == purpose && requests[0].DataClass == egress.ClassDocument
}
