package engine

import (
	"context"

	"github.com/hexagon-codes/ai-core/llm"
)

// contextTokenCounterProvider 仅在内层真实支持可取消计数时保留该能力。
type contextTokenCounterProvider struct {
	llm.Provider
	countTokensContext func(context.Context, []llm.Message) (int, error)
}

func (p *contextTokenCounterProvider) CountTokensContext(
	ctx context.Context,
	messages []llm.Message,
) (int, error) {
	return p.countTokensContext(ctx, messages)
}

// preserveContextTokenCounter 避免透明装饰器遮蔽内层的可选能力。
// 旧 Provider 仍只暴露原接口，不会被伪装成可在调用中取消。
func preserveContextTokenCounter(provider, inner llm.Provider) llm.Provider {
	if provider == nil || inner == nil {
		return provider
	}
	if _, ok := provider.(llm.ContextTokenCounter); ok {
		return provider
	}
	counter, ok := inner.(llm.ContextTokenCounter)
	if !ok {
		return provider
	}
	return &contextTokenCounterProvider{
		Provider:           provider,
		countTokensContext: counter.CountTokensContext,
	}
}

// unwrapContextTokenCounterProvider 只剥离本 helper 添加的能力外壳。
func unwrapContextTokenCounterProvider(provider llm.Provider) llm.Provider {
	for {
		wrapped, ok := provider.(*contextTokenCounterProvider)
		if !ok {
			return provider
		}
		provider = wrapped.Provider
	}
}

func wrapModelOverrideProvider(provider llm.Provider, model string) llm.Provider {
	return preserveContextTokenCounter(&modelOverrideProvider{inner: provider, model: model}, provider)
}

var (
	_ llm.Provider            = (*contextTokenCounterProvider)(nil)
	_ llm.ContextTokenCounter = (*contextTokenCounterProvider)(nil)
)
