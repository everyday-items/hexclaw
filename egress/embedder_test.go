package egress

import (
	"context"
	"testing"
)

type captureEmbedder struct{ calls int }

func (e *captureEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	e.calls++
	return [][]float32{{1}}, nil
}
func (e *captureEmbedder) EmbedOne(context.Context, string) ([]float32, error) {
	e.calls++
	return []float32{1}, nil
}
func (*captureEmbedder) Dimension() int { return 1 }

func TestCloudEmbedderMissingEnvelopeFailsClosed(t *testing.T) {
	next := &captureEmbedder{}
	emb := NewCloudEmbedder(next, &Policy{})
	if _, err := emb.Embed(context.Background(), []string{"secret"}); err == nil {
		t.Fatal("missing embedding envelope must fail closed")
	}
	if _, err := emb.EmbedOne(context.Background(), "secret"); err == nil {
		t.Fatal("missing EmbedOne envelope must fail closed")
	}
	if next.calls != 0 {
		t.Fatalf("denied embedding reached cloud: %d", next.calls)
	}
}

func TestCloudEmbedderTaggedDocumentAllowedAndMemoryDenied(t *testing.T) {
	next := &captureEmbedder{}
	emb := NewCloudEmbedder(next, &Policy{})
	docCtx := WithRequest(context.Background(), PurposeRAGEmbed, "", ClassDocument)
	if _, err := emb.Embed(docCtx, []string{"doc"}); err != nil {
		t.Fatal(err)
	}
	memCtx := WithRequest(context.Background(), PurposeRAGEmbed, "", ClassMemory)
	if _, err := emb.EmbedOne(memCtx, "memory"); err == nil {
		t.Fatal("cloud memory embedding must be denied")
	}
	if next.calls != 1 {
		t.Fatalf("only document call should reach cloud, calls=%d", next.calls)
	}
}

func TestCloudEmbedderDelegatesDimensionWithoutNetwork(t *testing.T) {
	if got := NewCloudEmbedder(&captureEmbedder{}, &Policy{}).Dimension(); got != 1 {
		t.Fatalf("Dimension=%d", got)
	}
}
