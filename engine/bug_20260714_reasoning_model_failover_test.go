package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexagon"
	hruntime "github.com/hexagon-codes/hexagon/runtime"
)

type reasoningFallbackRouter struct {
	calls int
}

func (r *reasoningFallbackRouter) Fallback(...string) (hexagon.Provider, string, error) {
	r.calls++
	return nil, "", errors.New("no cross-provider fallback")
}

// BUG-20260714：强推理模型 glm-4.5 被限流时，同一智谱 provider 的默认 glm-4v-flash
// 仍可用。必须先做同 provider 模型降级，不能把 provider 整体熔断，也不能直接返回空解。
func TestRuntimeProviderSelector_ReasoningModelFallsBackWithinProviderFirst(t *testing.T) {
	provider := &failoverProvider{name: "智谱 AI"}
	router := &reasoningFallbackRouter{}
	marked := 0
	selector := &runtimeProviderSelector{
		router:                      router,
		markUnhealthy:               func(string, string) { marked++ },
		initialProvider:             provider,
		initialName:                 "智谱 AI",
		initialModel:                "glm-4.5",
		initialSameProviderFallback: "glm-4v-flash",
		modelForProvider:            func(string) string { return "glm-4v-flash" },
		wrapProvider:                func(p hexagon.Provider, _, _ string) hexagon.Provider { return p },
	}
	if _, err := selector.Select(context.Background(), hruntime.Request{}); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !selector.failoverAdvance(errors.New("429 Too Many Requests")) {
		t.Fatal("强推理模型 429 后应推进到同 provider 默认模型")
	}
	gotProvider, gotName, gotModel := selector.Current()
	if gotProvider != provider || gotName != "智谱 AI" || gotModel != "glm-4v-flash" {
		t.Fatalf("同 provider 模型降级错误: provider=%T name=%q model=%q", gotProvider, gotName, gotModel)
	}
	if router.calls != 0 {
		t.Fatalf("同 provider 默认模型可用前不应请求跨 provider fallback，calls=%d", router.calls)
	}
	if marked != 0 {
		t.Fatalf("单个模型限流不应提前熔断整个 provider，marked=%d", marked)
	}
}
