package llmrouter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/localinfer"
	"github.com/hexagon-codes/hexclaw/resourcegov"
)

type routerContextTokenCounterTestKey struct{}

type routerContextTokenCounterProvider struct {
	*capabilityGuardCountingProvider
	wantContext  context.Context
	wantMessages []llm.Message
	contextCalls int
	legacyCalls  int
}

func (p *routerContextTokenCounterProvider) CountTokens(_ []llm.Message) (int, error) {
	p.legacyCalls++
	return 0, errors.New("legacy token counter must not be called")
}

func (p *routerContextTokenCounterProvider) CountTokensContext(
	ctx context.Context,
	messages []llm.Message,
) (int, error) {
	if ctx != p.wantContext {
		return 0, context.Canceled
	}
	if len(messages) != len(p.wantMessages) {
		return 0, errors.New("unexpected messages")
	}
	for index := range messages {
		if messages[index].Role != p.wantMessages[index].Role ||
			messages[index].Content != p.wantMessages[index].Content {
			return 0, errors.New("unexpected messages")
		}
	}
	p.contextCalls++
	return 41, nil
}

func TestSelectorProviderPreservesContextTokenCounterConditionally(t *testing.T) {
	for _, locality := range []string{"remote", "local"} {
		t.Run(locality, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), routerContextTokenCounterTestKey{}, "expected")
			messages := []llm.Message{{Role: llm.RoleUser, Content: "hello"}}
			aware := &routerContextTokenCounterProvider{
				capabilityGuardCountingProvider: &capabilityGuardCountingProvider{},
				wantContext:                     ctx,
				wantMessages:                    messages,
			}
			for _, test := range []struct {
				name  string
				inner llm.Provider
				aware *routerContextTokenCounterProvider
			}{
				{name: "context-aware", inner: aware, aware: aware},
				{name: "legacy", inner: &capabilityGuardCountingProvider{}},
			} {
				t.Run(test.name, func(t *testing.T) {
					providerConfig := config.LLMProviderConfig{Model: "test-model"}
					if locality == "local" {
						providerConfig.Locality = config.ProviderLocalityLocal
					}
					selector := NewWithProviders(
						config.LLMConfig{
							Default: "provider",
							Providers: map[string]config.LLMProviderConfig{
								"provider": providerConfig,
							},
						},
						map[string]llm.Provider{"provider": test.inner},
					)
					if locality == "remote" {
						selector.SetEgressPolicy(&egress.Policy{})
					} else {
						governor, err := resourcegov.New(resourcegov.Config{
							Limits: map[resourcegov.Resource]int{
								resourcegov.ResourceVLM:            1,
								resourcegov.ResourceLocalInference: 1,
								resourcegov.ResourceCPUHeavy:       1,
								resourcegov.ResourceSQLiteWrite:    1,
							},
							BackgroundAging:     time.Second,
							MaxInteractiveBurst: 1,
						})
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(governor.Close)
						selector.SetLocalInferenceCoordinator(localinfer.New(governor))
					}

					counter, ok := selector.Default().(llm.ContextTokenCounter)
					if test.aware == nil {
						if ok {
							t.Fatal("legacy provider was presented as context-aware")
						}
						return
					}
					if !ok {
						t.Fatal("selector wrapper hid context-aware capability")
					}
					got, err := counter.CountTokensContext(ctx, messages)
					if err != nil {
						t.Fatal(err)
					}
					if got != 41 || test.aware.contextCalls != 1 || test.aware.legacyCalls != 0 {
						t.Fatalf("result=%d context=%d legacy=%d", got, test.aware.contextCalls, test.aware.legacyCalls)
					}
				})
			}
		})
	}
}
