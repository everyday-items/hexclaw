package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

type engineContextTokenCounterProvider struct {
	*gwTestProvider
	wantContext  context.Context
	wantMessages []llm.Message
	contextCalls int
	legacyCalls  int
}

func (p *engineContextTokenCounterProvider) CountTokens(_ []llm.Message) (int, error) {
	p.legacyCalls++
	return 0, errors.New("legacy token counter must not be called")
}

func (p *engineContextTokenCounterProvider) CountTokensContext(
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
	return 37, nil
}

func TestPreserveContextTokenCounterOnlyExposesSupportedCapability(t *testing.T) {
	legacy := &gwTestProvider{name: "legacy"}
	if _, ok := preserveContextTokenCounter(legacy, legacy).(llm.ContextTokenCounter); ok {
		t.Fatal("legacy provider was presented as context-aware")
	}

	ctx := context.WithValue(context.Background(), ctxKey("token-counter-test"), "expected")
	inner := &engineContextTokenCounterProvider{
		gwTestProvider: &gwTestProvider{name: "context-aware"},
		wantContext:    ctx,
		wantMessages:   []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
	}
	wrapper := &modelOverrideProvider{inner: inner, model: "test-model"}
	preserved := preserveContextTokenCounter(wrapper, inner)
	counter, ok := preserved.(llm.ContextTokenCounter)
	if !ok {
		t.Fatal("context-aware capability was hidden by provider wrapper")
	}
	got, err := counter.CountTokensContext(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != 37 {
		t.Fatalf("CountTokensContext()=%d, want 37", got)
	}
}

func TestEngineProviderWrappersPreserveContextTokenCounter(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKey("token-counter-wrappers"), "expected")
	messages := []llm.Message{{Role: llm.RoleUser, Content: "hello"}}
	inner := &engineContextTokenCounterProvider{
		gwTestProvider: &gwTestProvider{name: "context-aware"},
		wantContext:    ctx,
		wantMessages:   messages,
	}
	wrappers := map[string]llm.Provider{
		"observe":               ObserveMiddleware(NewInMemoryRecorder())(inner),
		"rate-limit":            RateLimitMiddleware(1, 1)(inner),
		"prompt-rewrite":        PromptRewriteMiddleware(func(*llm.CompletionRequest) {})(inner),
		"model-override":        wrapModelOverrideProvider(inner, "test-model"),
		"vision-image-limit":    wrapVisionImageLimitProvider(inner, "test-model"),
		"code-exec-tool-choice": wrapCodeExecToolChoiceProvider(inner, "run this code with code_exec"),
		"thinking-bound": preserveContextTokenCounter(
			&thinkingBoundProvider{provider: inner},
			inner,
		),
	}
	for name, provider := range wrappers {
		t.Run(name, func(t *testing.T) {
			counter, ok := provider.(llm.ContextTokenCounter)
			if !ok {
				t.Fatal("context-aware capability was hidden")
			}
			before := inner.contextCalls
			got, err := counter.CountTokensContext(ctx, messages)
			if err != nil {
				t.Fatal(err)
			}
			if got != 37 {
				t.Fatalf("CountTokensContext()=%d, want 37", got)
			}
			if inner.contextCalls != before+1 || inner.legacyCalls != 0 {
				t.Fatalf("calls context=%d legacy=%d", inner.contextCalls, inner.legacyCalls)
			}
		})
	}
}

func TestEngineProviderWrappersDoNotPresentLegacyProviderAsContextAware(t *testing.T) {
	legacy := &gwTestProvider{name: "legacy"}
	wrappers := map[string]llm.Provider{
		"observe":               ObserveMiddleware(NewInMemoryRecorder())(legacy),
		"rate-limit":            RateLimitMiddleware(1, 1)(legacy),
		"prompt-rewrite":        PromptRewriteMiddleware(func(*llm.CompletionRequest) {})(legacy),
		"model-override":        wrapModelOverrideProvider(legacy, "test-model"),
		"vision-image-limit":    wrapVisionImageLimitProvider(legacy, "test-model"),
		"code-exec-tool-choice": wrapCodeExecToolChoiceProvider(legacy, "run this code with code_exec"),
		"thinking-bound": preserveContextTokenCounter(
			&thinkingBoundProvider{provider: legacy},
			legacy,
		),
	}
	for name, provider := range wrappers {
		t.Run(name, func(t *testing.T) {
			if _, ok := provider.(llm.ContextTokenCounter); ok {
				t.Fatal("legacy provider was presented as context-aware")
			}
		})
	}
}

func TestWrapVisionImageLimitProviderContextAwareIsIdempotent(t *testing.T) {
	ctx := context.Background()
	inner := &engineContextTokenCounterProvider{
		gwTestProvider: &gwTestProvider{name: "context-aware"},
		wantContext:    ctx,
		wantMessages:   nil,
	}
	wrapped := wrapVisionImageLimitProvider(inner, "test-model")
	twice := wrapVisionImageLimitProvider(wrapped, "test-model")
	if twice != wrapped {
		t.Fatal("context-aware vision provider was wrapped twice")
	}
	if _, ok := twice.(llm.ContextTokenCounter); !ok {
		t.Fatal("idempotent wrapper hid context-aware capability")
	}
}
