package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexagon/rag"
	"github.com/hexagon-codes/hexclaw/egress"
)

type captureDocReranker struct{ calls int }

func (*captureDocReranker) Name() string { return "capture" }
func (r *captureDocReranker) Rerank(context.Context, string, []rag.Document) ([]rag.Document, error) {
	r.calls++
	return nil, nil
}

func TestBUG20260710_RerankerEgressDenialStopsCloudCall(t *testing.T) {
	next := &captureDocReranker{}
	want := errors.New("denied")
	var requests []egress.Request
	r := guardedDocReranker{next: next, guard: func(ctx context.Context) error {
		requests, _ = egress.RequestsFromContext(ctx)
		return want
	}}
	ctx := egress.WithRequest(context.Background(), egress.PurposeRAGEnrich, "audit-rerank", egress.ClassDocument)
	if _, err := r.Rerank(ctx, "q", nil); !errors.Is(err, want) {
		t.Fatalf("Rerank error=%v", err)
	}
	if next.calls != 0 {
		t.Fatal("denied rerank reached cloud")
	}
	if len(requests) != 1 || requests[0].Purpose != egress.PurposeRAGEnrich ||
		requests[0].DataClass != egress.ClassDocument || requests[0].AuditID != "audit-rerank" {
		t.Fatalf("reranker egress classification=%+v", requests)
	}
}
