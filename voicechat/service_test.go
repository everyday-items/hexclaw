package voicechat

import (
	"context"
	"errors"
	"testing"
)

// mockVoiceProvider 模拟语音对话 Provider。
type mockVoiceProvider struct {
	name   string
	models []string
	result *Result
	err    error
	calls  int
}

func (m *mockVoiceProvider) Name() string              { return m.name }
func (m *mockVoiceProvider) SupportedModels() []string { return m.models }
func (m *mockVoiceProvider) Chat(_ context.Context, _ Request) (*Result, error) {
	m.calls++
	return m.result, m.err
}

// TestService_Chat_RoutesByProviderName 显式 providerName 优先。
func TestService_Chat_RoutesByProviderName(t *testing.T) {
	pA := &mockVoiceProvider{name: "openai", models: []string{"gpt-4o-audio-preview"}, result: &Result{Provider: "openai"}}
	pB := &mockVoiceProvider{name: "zhipu", models: []string{"glm-4-voice"}, result: &Result{Provider: "zhipu"}}
	svc := NewService(map[string]Provider{"openai": pA, "zhipu": pB}, "openai")

	res, err := svc.Chat(context.Background(), "zhipu", Request{Text: "hi", Model: "gpt-4o-audio-preview"})
	if err != nil {
		t.Fatalf("未预期错误: %v", err)
	}
	if res.Provider != "zhipu" {
		t.Errorf("显式 name 应覆盖 model 路由, 实际=%s", res.Provider)
	}
	if pA.calls != 0 || pB.calls != 1 {
		t.Errorf("期望仅 zhipu 被调用, openai=%d zhipu=%d", pA.calls, pB.calls)
	}
}

// TestService_Chat_RoutesByModel name 为空时按 model 路由。
func TestService_Chat_RoutesByModel(t *testing.T) {
	pA := &mockVoiceProvider{name: "openai", models: []string{"gpt-4o-audio-preview"}, result: &Result{Provider: "openai"}}
	pB := &mockVoiceProvider{name: "zhipu", models: []string{"glm-4-voice"}, result: &Result{Provider: "zhipu"}}
	svc := NewService(map[string]Provider{"openai": pA, "zhipu": pB}, "")

	res, err := svc.Chat(context.Background(), "", Request{AudioB64: "AAAA", Model: "glm-4-voice"})
	if err != nil {
		t.Fatalf("未预期错误: %v", err)
	}
	if res.Provider != "zhipu" {
		t.Errorf("按 model 应路由到 zhipu, 实际=%s", res.Provider)
	}
}

// TestService_Chat_FallbackDefault 未命中 name 和 model 时回退 default。
func TestService_Chat_FallbackDefault(t *testing.T) {
	pA := &mockVoiceProvider{name: "openai", models: []string{"a"}, result: &Result{Provider: "openai"}}
	pB := &mockVoiceProvider{name: "zhipu", models: []string{"b"}, result: &Result{Provider: "zhipu"}}
	svc := NewService(map[string]Provider{"openai": pA, "zhipu": pB}, "zhipu")

	res, err := svc.Chat(context.Background(), "", Request{Text: "hi", Model: "unknown"})
	if err != nil {
		t.Fatalf("未预期错误: %v", err)
	}
	if res.Provider != "zhipu" {
		t.Errorf("应回退 default=zhipu, 实际=%s", res.Provider)
	}
}

// TestService_Chat_RequiresAudioOrText audio 和 text 皆空应报错。
func TestService_Chat_RequiresAudioOrText(t *testing.T) {
	p := &mockVoiceProvider{name: "x", models: []string{"m"}, result: &Result{}}
	svc := NewService(map[string]Provider{"x": p}, "x")

	_, err := svc.Chat(context.Background(), "", Request{})
	if err == nil {
		t.Fatal("audio 和 text 皆空应报错")
	}
	if p.calls != 0 {
		t.Error("参数校验失败时不应调用 Provider")
	}

	// 仅 text 应通过
	_, err = svc.Chat(context.Background(), "x", Request{Text: "hi"})
	if err != nil {
		t.Errorf("仅 text 应通过, 实际 err=%v", err)
	}

	// 仅 audio 应通过
	_, err = svc.Chat(context.Background(), "x", Request{AudioB64: "AAAA"})
	if err != nil {
		t.Errorf("仅 audio 应通过, 实际 err=%v", err)
	}
}

// TestService_Chat_NoProvider 无 provider 时应报错。
func TestService_Chat_NoProvider(t *testing.T) {
	svc := NewService(nil, "")

	if svc.HasProvider() {
		t.Error("空 svc HasProvider 应为 false")
	}

	_, err := svc.Chat(context.Background(), "x", Request{Text: "hi", Model: "any"})
	if err == nil {
		t.Fatal("无 provider 应报错")
	}
}

// TestService_Chat_ProviderError Provider 错误透传。
func TestService_Chat_ProviderError(t *testing.T) {
	want := errors.New("voice api down")
	p := &mockVoiceProvider{name: "x", models: []string{"m"}, err: want}
	svc := NewService(map[string]Provider{"x": p}, "x")

	_, err := svc.Chat(context.Background(), "x", Request{Text: "hi"})
	if !errors.Is(err, want) {
		t.Errorf("错误应透传, 实际=%v", err)
	}
}

// TestNewService_SkipsNil nil provider 应被跳过。
func TestNewService_SkipsNil(t *testing.T) {
	pA := &mockVoiceProvider{name: "a", models: []string{"m1"}}
	svc := NewService(map[string]Provider{
		"a":   pA,
		"nil": nil,
	}, "a")

	if len(svc.Providers()) != 1 {
		t.Errorf("nil provider 应被跳过, 实际 providers=%v", svc.Providers())
	}
	if len(svc.Models()) != 1 || svc.Models()[0] != "m1" {
		t.Errorf("Models 应仅含有效 provider 的模型, 实际=%v", svc.Models())
	}
}
