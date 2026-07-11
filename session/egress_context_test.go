package session

import (
	"context"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/storage"
)

type egressSessionProvider struct {
	requests [][]egress.Request
}

func (p *egressSessionProvider) Name() string { return "egress-session" }
func (p *egressSessionProvider) Complete(ctx context.Context, _ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	reqs, _ := egress.RequestsFromContext(ctx)
	p.requests = append(p.requests, reqs)
	return &hexagon.CompletionResponse{Content: "summary"}, nil
}
func (p *egressSessionProvider) Stream(context.Context, hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	return nil, nil
}
func (p *egressSessionProvider) Models() []llm.ModelInfo                { return nil }
func (p *egressSessionProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

func requireSessionEgress(t *testing.T, reqs []egress.Request) {
	t.Helper()
	want := map[egress.DataClass]bool{egress.ClassGeneral: false, egress.ClassMemory: false}
	for _, req := range reqs {
		if req.Purpose != egress.PurposeGeneralChat {
			t.Fatalf("purpose=%s, want general_chat", req.Purpose)
		}
		if _, ok := want[req.DataClass]; ok {
			want[req.DataClass] = true
		}
	}
	for class, found := range want {
		if !found {
			t.Fatalf("missing class %s in %#v", class, reqs)
		}
	}
}

func TestSuggestTitle_LabelsConversationMemory(t *testing.T) {
	p := &egressSessionProvider{}
	_, err := SuggestTitle(context.Background(), p, []*storage.MessageRecord{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("SuggestTitle: %v", err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("provider calls=%d", len(p.requests))
	}
	requireSessionEgress(t, p.requests[0])
}

func TestCompactorGenerateSummary_LabelsConversationMemory(t *testing.T) {
	p := &egressSessionProvider{}
	c := NewCompactor(newMockStore(), DefaultCompactionConfig())
	_, err := c.generateSummary(context.Background(), []*storage.MessageRecord{{Role: "user", Content: "hello"}}, p)
	if err != nil {
		t.Fatalf("generateSummary: %v", err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("provider calls=%d", len(p.requests))
	}
	requireSessionEgress(t, p.requests[0])
}
