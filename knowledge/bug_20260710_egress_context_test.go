package knowledge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	hrag "github.com/hexagon-codes/hexagon/rag"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/featureflag"
)

// ctxCapture records the request envelope observed at an outbound-capability
// boundary. Keeping the assertion outside the fake makes it safe for the two
// concurrent query-expansion goroutines.
type ctxCapture struct {
	mu   sync.Mutex
	seen [][]egress.Request
	ok   []bool
}

func (c *ctxCapture) record(ctx context.Context) {
	reqs, ok := egress.RequestsFromContext(ctx)
	c.mu.Lock()
	c.seen = append(c.seen, reqs)
	c.ok = append(c.ok, ok)
	c.mu.Unlock()
}

func (c *ctxCapture) assertAll(t *testing.T, purpose egress.Purpose, minCalls int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) < minCalls {
		t.Fatalf("outbound boundary calls=%d, want at least %d", len(c.seen), minCalls)
	}
	for i, reqs := range c.seen {
		if !c.ok[i] {
			t.Errorf("call %d missing egress request envelope", i)
			continue
		}
		if len(reqs) != 1 {
			t.Errorf("call %d requests=%v, want exactly one document classification", i, reqs)
			continue
		}
		if reqs[0].Purpose != purpose || reqs[0].DataClass != egress.ClassDocument {
			t.Errorf("call %d request=%+v, want purpose=%s class=%s", i, reqs[0], purpose, egress.ClassDocument)
		}
	}
}

type contextCapturingEmbedder struct{ capture *ctxCapture }

func (e *contextCapturingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.capture.record(ctx)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func (e *contextCapturingEmbedder) EmbedOne(ctx context.Context, _ string) ([]float32, error) {
	e.capture.record(ctx)
	return []float32{1, 0, 0}, nil
}

func (*contextCapturingEmbedder) Dimension() int { return 3 }

type contextCapturingLLM struct {
	capture *ctxCapture
	reply   string
}

func (l *contextCapturingLLM) Complete(ctx context.Context, _ string) (string, error) {
	l.capture.record(ctx)
	return l.reply, nil
}

type contextCapturingDocReranker struct{ capture *ctxCapture }

func (*contextCapturingDocReranker) Name() string { return "context-capture" }

func (r *contextCapturingDocReranker) Rerank(ctx context.Context, _ string, docs []hrag.Document) ([]hrag.Document, error) {
	r.capture.record(ctx)
	return docs, nil
}

func TestBUG20260710_KnowledgeEmbeddingCallsCarryRAGEgressContext(t *testing.T) {
	t.Run("embedding decorator batch boundary", func(t *testing.T) {
		capture := &ctxCapture{}
		embedder := NewTruncatingEmbedder(&contextCapturingEmbedder{capture: capture}, 8)
		if _, err := embedder.Embed(context.Background(), []string{"document"}); err != nil {
			t.Fatal(err)
		}
		capture.assertAll(t, egress.PurposeRAGEmbed, 1)
	})

	t.Run("embedding decorator single boundary", func(t *testing.T) {
		capture := &ctxCapture{}
		embedder := NewTruncatingEmbedder(&contextCapturingEmbedder{capture: capture}, 8)
		if _, err := embedder.EmbedOne(context.Background(), "query"); err != nil {
			t.Fatal(err)
		}
		capture.assertAll(t, egress.PurposeRAGEmbed, 1)
	})

	t.Run("document embedding", func(t *testing.T) {
		capture := &ctxCapture{}
		cfg := DefaultHybridConfig()
		cfg.ContextualEnabled = false
		mgr := NewManager(stubRepo{}, stubSearcher{}, &contextCapturingEmbedder{capture: capture},
			WithSplitter(testSplitter()), WithHybridConfig(cfg))
		if _, err := mgr.AddDocument(context.Background(), "doc", "document body", "manual"); err != nil {
			t.Fatal(err)
		}
		capture.assertAll(t, egress.PurposeRAGEmbed, 1)
	})

	t.Run("query embedding", func(t *testing.T) {
		capture := &ctxCapture{}
		cfg := DefaultHybridConfig()
		cfg.ExpandEnabled = false
		cfg.RerankEnabled = false
		mgr := NewManager(stubRepo{}, stubSearcher{}, &contextCapturingEmbedder{capture: capture},
			WithHybridConfig(cfg))
		if _, err := mgr.Search(context.Background(), "search query", 3); err != nil {
			t.Fatal(err)
		}
		capture.assertAll(t, egress.PurposeRAGEmbed, 1)
	})
}

func TestBUG20260710_RAGEgressContextPreservesExistingAuditID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		purpose egress.Purpose
		stamp   func(context.Context) context.Context
	}{
		{name: "embed", purpose: egress.PurposeRAGEmbed, stamp: ragEmbedContext},
		{name: "enrich", purpose: egress.PurposeRAGEnrich, stamp: ragEnrichContext},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := egress.WithRequest(context.Background(), tc.purpose, "audit-123", egress.ClassDocument)
			requests, ok := egress.RequestsFromContext(tc.stamp(ctx))
			if !ok || len(requests) != 1 {
				t.Fatalf("requests=%v ok=%v", requests, ok)
			}
			if requests[0].AuditID != "audit-123" {
				t.Fatalf("audit ID lost while re-stamping matching RAG context: %+v", requests[0])
			}
		})
	}
}

func TestBUG20260710_SharedEmbeddingDecoratorMustNotRelabelMemoryAsDocument(t *testing.T) {
	capture := &ctxCapture{}
	embedder := NewTruncatingEmbedder(&contextCapturingEmbedder{capture: capture}, 8)
	ctx := egress.WithRequest(context.Background(), egress.PurposeRAGEmbed, "memory-audit", egress.ClassMemory)
	if _, err := embedder.EmbedOne(ctx, "private memory"); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.seen) != 1 || len(capture.seen[0]) != 1 ||
		capture.seen[0][0].DataClass != egress.ClassMemory || capture.seen[0][0].AuditID != "memory-audit" {
		t.Fatalf("shared decorator relabeled sensitive memory: %+v", capture.seen)
	}
}

func TestBUG20260710_KnowledgeEnrichmentCallsCarryRAGEgressContext(t *testing.T) {
	t.Run("multi-query and HyDE", func(t *testing.T) {
		capture := &ctxCapture{}
		cfg := DefaultHybridConfig()
		cfg.ExpandEnabled = true
		mgr := NewManager(stubRepo{}, stubSearcher{}, nil,
			WithHybridConfig(cfg), WithLLM(&contextCapturingLLM{capture: capture, reply: "alternative query"}))
		_ = mgr.expandQueries(context.Background(), "original query")
		capture.assertAll(t, egress.PurposeRAGEnrich, 2)
	})

	t.Run("contextual document summary", func(t *testing.T) {
		capture := &ctxCapture{}
		mgr := NewManager(stubRepo{}, stubSearcher{}, nil,
			WithLLM(&contextCapturingLLM{capture: capture, reply: "summary"}))
		if _, err := mgr.generateChunkContext(context.Background(), "document", "chunk"); err != nil {
			t.Fatal(err)
		}
		capture.assertAll(t, egress.PurposeRAGEnrich, 1)
	})

	t.Run("dedicated document reranker", func(t *testing.T) {
		capture := &ctxCapture{}
		mgr := NewManager(stubRepo{}, stubSearcher{}, nil)
		pool := []*SearchResult{{Chunk: &Chunk{ID: "c1", Content: "body", Score: 0.5}}}
		if _, err := mgr.rerankWith(context.Background(), &contextCapturingDocReranker{capture: capture}, "query", pool, 1); err != nil {
			t.Fatal(err)
		}
		capture.assertAll(t, egress.PurposeRAGEnrich, 1)
	})

	t.Run("image caption", func(t *testing.T) {
		capture := &ctxCapture{}
		mgr := NewManager(stubRepo{}, stubSearcher{}, nil, WithCaptioner(CaptionerFunc(
			func(ctx context.Context, _ []byte, _ string) (string, error) {
				capture.record(ctx)
				return "caption", nil
			},
		)))
		if _, err := mgr.CaptionImage(context.Background(), []byte("image"), "image/png"); err != nil {
			t.Fatal(err)
		}
		capture.assertAll(t, egress.PurposeRAGEnrich, 1)
	})

	t.Run("faithfulness judge", func(t *testing.T) {
		capture := &ctxCapture{}
		judge := &contextCapturingLLM{capture: capture, reply: `{"faithfulness":1,"answer_relevance":1,"context_relevance":1,"unsupported_claims":[],"reason":"ok"}`}
		_, err := EvalFaithfulness(context.Background(), judge, FaithfulnessCase{
			Name: "case", Question: "q", Context: "document", Answer: "a",
		})
		if err != nil {
			t.Fatal(err)
		}
		capture.assertAll(t, egress.PurposeRAGEnrich, 1)
	})
}

type contextCapturingQueryRewriter struct{ capture *ctxCapture }

func (r contextCapturingQueryRewriter) Rewrite(ctx context.Context, query string) ([]string, error) {
	r.capture.record(ctx)
	return []string{query}, nil
}

type fixedPipelineRetriever struct{}

func (fixedPipelineRetriever) Retrieve(context.Context, []string, int) ([]SearchHit, error) {
	return []SearchHit{{ChunkID: "c1", DocID: "d1", Content: "document", Score: 1}}, nil
}

type contextCapturingPipelineReranker struct{ capture *ctxCapture }

func (r contextCapturingPipelineReranker) Rerank(ctx context.Context, _ string, hits []SearchHit, _ int) ([]SearchHit, error) {
	r.capture.record(ctx)
	return hits, nil
}

func TestBUG20260710_RAGPipelineExternalStagesCarryEnrichmentContext(t *testing.T) {
	rewriteCapture := &ctxCapture{}
	rerankCapture := &ctxCapture{}
	answerCapture := &ctxCapture{}
	p := &Pipeline{
		Rewriter:  contextCapturingQueryRewriter{capture: rewriteCapture},
		Retriever: fixedPipelineRetriever{},
		Reranker:  contextCapturingPipelineReranker{capture: rerankCapture},
		Answerer: AnswerFunc(func(ctx context.Context, _, _ string) (string, error) {
			answerCapture.record(ctx)
			return "answer", nil
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = featureflag.WithContext(ctx, featureflag.Defaults)
	if _, err := p.RunRAG(ctx, "query", 1); err != nil {
		t.Fatal(err)
	}
	for name, capture := range map[string]*ctxCapture{
		"query rewriter": rewriteCapture,
		"reranker":       rerankCapture,
		"answerer":       answerCapture,
	} {
		t.Run(name, func(t *testing.T) {
			capture.assertAll(t, egress.PurposeRAGEnrich, 1)
		})
	}
}

var _ hexagon.VectorEmbedder = (*contextCapturingEmbedder)(nil)
