package main

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexagon/rag"
	"github.com/hexagon-codes/hexagon/rag/reranker"
	"github.com/hexagon-codes/hexclaw/egress"
)

// guardedDocReranker protects the dedicated /rerank HTTP client, which does
// not travel through llmrouter's Provider facade.
type guardedDocReranker struct {
	next  reranker.Reranker
	guard func(context.Context) error
}

func (r guardedDocReranker) Name() string {
	if r.next == nil {
		return "egress-guarded-reranker"
	}
	return r.next.Name()
}

func (r guardedDocReranker) Rerank(ctx context.Context, query string, docs []rag.Document) ([]rag.Document, error) {
	ctx = ragEnrichEgressContext(ctx)
	if r.guard == nil {
		return nil, fmt.Errorf("egress 拦截: reranker policy 未注入")
	}
	if err := r.guard(ctx); err != nil {
		return nil, err
	}
	if r.next == nil {
		return nil, fmt.Errorf("reranker 未注入")
	}
	return r.next.Rerank(ctx, query, docs)
}

func ragEnrichEgressContext(ctx context.Context) context.Context {
	if requests, ok := egress.RequestsFromContext(ctx); ok && len(requests) == 1 &&
		requests[0].Purpose == egress.PurposeRAGEnrich && requests[0].DataClass == egress.ClassDocument {
		return ctx
	}
	return egress.WithRequest(ctx, egress.PurposeRAGEnrich, "", egress.ClassDocument)
}
