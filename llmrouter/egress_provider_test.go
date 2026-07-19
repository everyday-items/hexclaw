package llmrouter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
)

type egressCaptureProvider struct {
	completeCalls int
	streamCalls   int
}

func (p *egressCaptureProvider) Name() string { return "capture" }
func (p *egressCaptureProvider) Complete(context.Context, hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	p.completeCalls++
	return &hexagon.CompletionResponse{Content: "ok"}, nil
}
func (p *egressCaptureProvider) Stream(context.Context, hexagon.CompletionRequest) (*llm.Stream, error) {
	p.streamCalls++
	return nil, nil
}
func (p *egressCaptureProvider) Models() []llm.ModelInfo { return nil }
func (p *egressCaptureProvider) CountTokens([]llm.Message) (int, error) {
	return 0, nil
}

func newEgressSelector(baseURL string, provider *egressCaptureProvider) *Selector {
	return newEgressSelectorWithLocality(baseURL, "", provider)
}

func newEgressSelectorWithLocality(baseURL, locality string, provider *egressCaptureProvider) *Selector {
	cfg := config.LLMConfig{
		Default: "remote",
		Providers: map[string]config.LLMProviderConfig{
			"remote": {BaseURL: baseURL, APIKey: "test", Model: "test", Locality: locality},
		},
	}
	r := NewWithProviders(cfg, map[string]hexagon.Provider{"remote": provider})
	r.SetEgressPolicy(&egress.Policy{})
	return r
}

func TestCloudProviderMissingEgressEnvelopeFailsClosed(t *testing.T) {
	p := &egressCaptureProvider{}
	r := newEgressSelector("https://llm.example/v1", p)
	provider := r.Default()
	if _, err := provider.Complete(context.Background(), hexagon.CompletionRequest{}); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("missing envelope error=%v", err)
	}
	if _, err := provider.Stream(context.Background(), hexagon.CompletionRequest{}); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("missing stream envelope error=%v", err)
	}
	if p.completeCalls != 0 || p.streamCalls != 0 {
		t.Fatalf("denied cloud call reached provider: complete=%d stream=%d", p.completeCalls, p.streamCalls)
	}
}

func TestCloudProviderAllowsTaggedNonSensitiveRequest(t *testing.T) {
	p := &egressCaptureProvider{}
	r := newEgressSelector("https://llm.example/v1", p)
	ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "audit", egress.ClassGeneral)
	provider, _, err := r.Route(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(ctx, hexagon.CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Stream(ctx, hexagon.CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	if p.completeCalls != 1 || p.streamCalls != 1 {
		t.Fatalf("tagged calls not delegated exactly once: complete=%d stream=%d", p.completeCalls, p.streamCalls)
	}
}

func TestCloudProviderRejectsSensitiveGeneralChat(t *testing.T) {
	p := &egressCaptureProvider{}
	r := newEgressSelector("https://llm.example/v1", p)
	ctx := egress.WithRequest(context.Background(), egress.PurposeGeneralChat, "", egress.ClassGeneral, egress.ClassMemory)
	if _, err := r.Default().Complete(ctx, hexagon.CompletionRequest{}); err == nil {
		t.Fatal("cloud general chat carrying memory must be denied")
	}
	if p.completeCalls != 0 {
		t.Fatal("denied sensitive call reached provider")
	}
}

func TestLocalProviderBypassesCloudEgressEnvelope(t *testing.T) {
	p := &egressCaptureProvider{}
	r := newEgressSelectorWithLocality("http://127.0.0.1:11434/v1", config.ProviderLocalityLocal, p)
	if _, err := r.Default().Complete(context.Background(), hexagon.CompletionRequest{}); err != nil {
		t.Fatalf("local provider must not require cloud egress envelope: %v", err)
	}
	if p.completeCalls != 1 {
		t.Fatalf("local call count=%d", p.completeCalls)
	}
}

func TestFallbackProviderKeepsCloudEgressGuard(t *testing.T) {
	p1, p2 := &egressCaptureProvider{}, &egressCaptureProvider{}
	cfg := config.LLMConfig{Default: "a", Providers: map[string]config.LLMProviderConfig{
		"a": {BaseURL: "https://a.example", APIKey: "a"},
		"b": {BaseURL: "https://b.example", APIKey: "b"},
	}}
	r := NewWithProviders(cfg, map[string]hexagon.Provider{"a": p1, "b": p2})
	r.SetEgressPolicy(&egress.Policy{})
	p, _, err := r.Fallback("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(context.Background(), hexagon.CompletionRequest{}); err == nil {
		t.Fatal("fallback cloud provider must also fail closed without envelope")
	}
	if p2.completeCalls != 0 {
		t.Fatal("fallback bypassed egress guard")
	}
}

func TestReloadRetainsCloudEgressPolicy(t *testing.T) {
	p := &egressCaptureProvider{}
	r := newEgressSelector("https://old.example/v1", p)
	if err := r.Reload(config.LLMConfig{Default: "remote", Providers: map[string]config.LLMProviderConfig{
		"remote": {BaseURL: "https://new.example/v1", APIKey: "test", Model: "test"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Default().Complete(context.Background(), hexagon.CompletionRequest{}); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("Reload dropped cloud egress guard: %v", err)
	}
}
