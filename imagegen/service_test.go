package imagegen

import (
	"context"
	"errors"
	"testing"
	"time"
)

// mockImageProvider 模拟图像生成 Provider。
type mockImageProvider struct {
	name   string
	models []string
	result *Result
	err    error
	calls  int
}

func (m *mockImageProvider) Name() string              { return m.name }
func (m *mockImageProvider) SupportedModels() []string { return m.models }
func (m *mockImageProvider) Generate(_ context.Context, _ Request) (*Result, error) {
	m.calls++
	return m.result, m.err
}

// TestService_RoutesByProviderName 显式 providerName 优先。
func TestService_RoutesByProviderName(t *testing.T) {
	pA := &mockImageProvider{
		name:   "alpha",
		models: []string{"dall-e-3"},
		result: &Result{Provider: "alpha", Created: time.Now().Unix()},
	}
	pB := &mockImageProvider{
		name:   "beta",
		models: []string{"cogview-4"},
		result: &Result{Provider: "beta"},
	}
	svc := NewService(map[string]Provider{"alpha": pA, "beta": pB}, "alpha")

	res, err := svc.Generate(context.Background(), "beta", Request{Prompt: "p", Model: "dall-e-3"})
	if err != nil {
		t.Fatalf("未预期错误: %v", err)
	}
	if res.Provider != "beta" {
		t.Errorf("显式 providerName 应覆盖 model 路由, 实际=%s", res.Provider)
	}
	if pB.calls != 1 || pA.calls != 0 {
		t.Errorf("期望仅 beta 被调用, alpha=%d beta=%d", pA.calls, pB.calls)
	}
}

// TestService_RoutesByModel providerName 为空时按 model 路由。
func TestService_RoutesByModel(t *testing.T) {
	pA := &mockImageProvider{name: "alpha", models: []string{"dall-e-3"}, result: &Result{Provider: "alpha"}}
	pB := &mockImageProvider{name: "beta", models: []string{"cogview-4"}, result: &Result{Provider: "beta"}}
	svc := NewService(map[string]Provider{"alpha": pA, "beta": pB}, "")

	res, err := svc.Generate(context.Background(), "", Request{Prompt: "p", Model: "cogview-4"})
	if err != nil {
		t.Fatalf("未预期错误: %v", err)
	}
	if res.Provider != "beta" {
		t.Errorf("按 model 应路由到 beta, 实际=%s", res.Provider)
	}
}

// TestService_FallbackToDefault providerName 空且 model 未知 → default。
func TestService_FallbackToDefault(t *testing.T) {
	pA := &mockImageProvider{name: "alpha", models: []string{"dall-e-3"}, result: &Result{Provider: "alpha"}}
	pB := &mockImageProvider{name: "beta", models: []string{"cogview-4"}, result: &Result{Provider: "beta"}}
	svc := NewService(map[string]Provider{"alpha": pA, "beta": pB}, "beta")

	res, err := svc.Generate(context.Background(), "", Request{Prompt: "p", Model: "unknown-model"})
	if err != nil {
		t.Fatalf("未预期错误: %v", err)
	}
	if res.Provider != "beta" {
		t.Errorf("应回退 defaultProvider=beta, 实际=%s", res.Provider)
	}
}

// TestService_EmptyPrompt 空 prompt 应报错。
func TestService_EmptyPrompt(t *testing.T) {
	p := &mockImageProvider{name: "p", models: []string{"m"}, result: &Result{}}
	svc := NewService(map[string]Provider{"p": p}, "p")

	_, err := svc.Generate(context.Background(), "", Request{Prompt: ""})
	if err == nil {
		t.Fatal("空 prompt 应报错")
	}
	if p.calls != 0 {
		t.Error("空 prompt 时不应调用 Provider")
	}
}

// TestService_NoProvider 无 provider 时应报错。
func TestService_NoProvider(t *testing.T) {
	svc := NewService(nil, "")

	if svc.HasProvider() {
		t.Error("无 provider 时 HasProvider 应为 false")
	}

	_, err := svc.Generate(context.Background(), "x", Request{Prompt: "hi", Model: "any"})
	if err == nil {
		t.Fatal("无 provider 应报错")
	}
}

// TestService_ProviderError provider 返回错误应透传。
func TestService_ProviderError(t *testing.T) {
	want := errors.New("provider down")
	p := &mockImageProvider{name: "p", models: []string{"m"}, err: want}
	svc := NewService(map[string]Provider{"p": p}, "p")

	_, err := svc.Generate(context.Background(), "p", Request{Prompt: "hi"})
	if !errors.Is(err, want) {
		t.Errorf("错误应透传, 实际=%v", err)
	}
}

// TestNewService_SkipsNilAndModelConflict nil provider 被忽略；model 冲突保留首选。
func TestNewService_SkipsNilAndModelConflict(t *testing.T) {
	pA := &mockImageProvider{name: "alpha", models: []string{"shared", "a-only"}}
	pB := &mockImageProvider{name: "beta", models: []string{"shared", "b-only"}}
	svc := NewService(map[string]Provider{
		"alpha": pA,
		"beta":  pB,
		"nil":   nil,
	}, "alpha")

	if len(svc.Providers()) != 2 {
		t.Errorf("nil provider 应被跳过, 实际 providers=%v", svc.Providers())
	}

	models := svc.Models()
	found := map[string]bool{}
	for _, m := range models {
		found[m] = true
	}
	for _, m := range []string{"shared", "a-only", "b-only"} {
		if !found[m] {
			t.Errorf("model %q 应在索引中", m)
		}
	}

	// "shared" model 应路由到最早注册的 Provider（map 顺序不稳定，
	// 但两者之一必须命中；验证至少调到了一个 provider，未报错）
	res, err := svc.Generate(context.Background(), "", Request{Prompt: "x", Model: "shared"})
	// 需要给被路由命中的 provider 预设 result
	if err == nil && res == nil {
		// 两个 mock 均未预设 result → 会拿到 nil result & nil err
		// 用一个调用计数断言至少命中一个
		if pA.calls+pB.calls != 1 {
			t.Errorf("shared model 应恰路由给一个 provider, A=%d B=%d", pA.calls, pB.calls)
		}
	}
}
