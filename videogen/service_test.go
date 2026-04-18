package videogen

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mockVideoProvider 模拟视频生成 Provider。
type mockVideoProvider struct {
	name       string
	models     []string
	submitID   string
	submitErr  error
	status     TaskStatus
	pollErr    error
	submitCall int
	pollCall   int
	lastRawID  string
}

func (m *mockVideoProvider) Name() string              { return m.name }
func (m *mockVideoProvider) SupportedModels() []string { return m.models }
func (m *mockVideoProvider) Submit(_ context.Context, _ Request) (string, error) {
	m.submitCall++
	return m.submitID, m.submitErr
}
func (m *mockVideoProvider) Poll(_ context.Context, taskID string) (TaskStatus, error) {
	m.pollCall++
	m.lastRawID = taskID
	return m.status, m.pollErr
}

// TestService_Submit_WrapsTaskID Submit 成功后应返回 "{provider}::{rawID}"。
func TestService_Submit_WrapsTaskID(t *testing.T) {
	p := &mockVideoProvider{
		name:     "zhipu",
		models:   []string{"cogvideox-2"},
		submitID: "raw-123",
	}
	svc := NewService(map[string]Provider{"zhipu": p}, "zhipu")

	id, err := svc.Submit(context.Background(), "", Request{Prompt: "a cat", Model: "cogvideox-2"})
	if err != nil {
		t.Fatalf("未预期错误: %v", err)
	}
	if id != "zhipu::raw-123" {
		t.Errorf("期望任务 ID 包装为 'zhipu::raw-123', 实际=%s", id)
	}
	if p.submitCall != 1 {
		t.Errorf("期望 Submit 调用 1 次, 实际=%d", p.submitCall)
	}
}

// TestService_Submit_RequiresPromptOrImage 两者皆空报错。
func TestService_Submit_RequiresPromptOrImage(t *testing.T) {
	p := &mockVideoProvider{name: "x", models: []string{"m"}, submitID: "r"}
	svc := NewService(map[string]Provider{"x": p}, "x")

	_, err := svc.Submit(context.Background(), "", Request{})
	if err == nil {
		t.Fatal("prompt 和 image_url 皆空应报错")
	}
	if p.submitCall != 0 {
		t.Error("参数校验失败时不应调用 Provider")
	}

	// 只有 image_url 应通过
	_, err = svc.Submit(context.Background(), "", Request{ImageURL: "http://img", Model: "m"})
	if err != nil {
		t.Errorf("仅 image_url 应通过, 实际 err=%v", err)
	}
}

// TestService_Submit_NoProvider 无 Provider 应报错。
func TestService_Submit_NoProvider(t *testing.T) {
	svc := NewService(nil, "")
	_, err := svc.Submit(context.Background(), "x", Request{Prompt: "hi"})
	if err == nil {
		t.Fatal("无 provider 应报错")
	}
	if svc.HasProvider() {
		t.Error("空 svc HasProvider 应为 false")
	}
}

// TestService_Submit_ProviderError Submit 报错透传。
func TestService_Submit_ProviderError(t *testing.T) {
	want := errors.New("submit fail")
	p := &mockVideoProvider{name: "x", models: []string{"m"}, submitErr: want}
	svc := NewService(map[string]Provider{"x": p}, "x")

	_, err := svc.Submit(context.Background(), "x", Request{Prompt: "hi"})
	if !errors.Is(err, want) {
		t.Errorf("Submit 错误应透传, 实际=%v", err)
	}
}

// TestService_Poll_HappyPath Poll 能解析前缀 → 找到 Provider → 拿到 raw ID → 返回带前缀 ID。
func TestService_Poll_HappyPath(t *testing.T) {
	p := &mockVideoProvider{
		name:   "zhipu",
		models: []string{"cogvideox-2"},
		status: TaskStatus{Status: "success", Done: true, VideoURL: "http://v"},
	}
	svc := NewService(map[string]Provider{"zhipu": p}, "zhipu")

	st, err := svc.Poll(context.Background(), "zhipu::raw-123")
	if err != nil {
		t.Fatalf("未预期错误: %v", err)
	}
	if st.TaskID != "zhipu::raw-123" {
		t.Errorf("TaskID 应保持带前缀, 实际=%s", st.TaskID)
	}
	if p.lastRawID != "raw-123" {
		t.Errorf("Provider 应收到 raw ID, 实际=%s", p.lastRawID)
	}
	if !st.Done || st.Status != "success" {
		t.Errorf("status 应透传, 实际=%+v", st)
	}
}

// TestService_Poll_BadFormat 不带 :: 的 ID 应报错。
func TestService_Poll_BadFormat(t *testing.T) {
	p := &mockVideoProvider{name: "x", models: []string{"m"}}
	svc := NewService(map[string]Provider{"x": p}, "x")

	_, err := svc.Poll(context.Background(), "no-prefix-id")
	if err == nil {
		t.Fatal("格式错误应报错")
	}
	if !strings.Contains(err.Error(), "格式") {
		t.Errorf("错误信息应提到格式, 实际=%v", err)
	}
}

// TestService_Poll_UnknownProvider 前缀未知 Provider 应报错。
func TestService_Poll_UnknownProvider(t *testing.T) {
	p := &mockVideoProvider{name: "zhipu", models: []string{"m"}}
	svc := NewService(map[string]Provider{"zhipu": p}, "zhipu")

	_, err := svc.Poll(context.Background(), "other::raw-1")
	if err == nil {
		t.Fatal("未知 provider 前缀应报错")
	}
	if p.pollCall != 0 {
		t.Error("未知 provider 不应调用已有 Provider 的 Poll")
	}
}

// TestService_Poll_ProviderError Poll 错误透传。
func TestService_Poll_ProviderError(t *testing.T) {
	want := errors.New("poll fail")
	p := &mockVideoProvider{name: "x", models: []string{"m"}, pollErr: want}
	svc := NewService(map[string]Provider{"x": p}, "x")

	_, err := svc.Poll(context.Background(), "x::raw")
	if !errors.Is(err, want) {
		t.Errorf("Poll 错误应透传, 实际=%v", err)
	}
}

// TestService_ResolveProvider_Priority 验证 name > model > default 优先级。
func TestService_ResolveProvider_Priority(t *testing.T) {
	pA := &mockVideoProvider{name: "alpha", models: []string{"m-alpha"}, submitID: "A"}
	pB := &mockVideoProvider{name: "beta", models: []string{"m-beta"}, submitID: "B"}
	svc := NewService(map[string]Provider{"alpha": pA, "beta": pB}, "beta")

	// name 指定 -> alpha
	id, err := svc.Submit(context.Background(), "alpha", Request{Prompt: "hi", Model: "m-beta"})
	if err != nil || !strings.HasPrefix(id, "alpha::") {
		t.Errorf("显式 name=alpha 应路由到 alpha, id=%s err=%v", id, err)
	}

	// name 空 model 命中 -> beta
	id, err = svc.Submit(context.Background(), "", Request{Prompt: "hi", Model: "m-beta"})
	if err != nil || !strings.HasPrefix(id, "beta::") {
		t.Errorf("按 model 应路由到 beta, id=%s err=%v", id, err)
	}

	// name 和 model 都空 -> default(beta)
	id, err = svc.Submit(context.Background(), "", Request{ImageURL: "http://x"})
	if err != nil || !strings.HasPrefix(id, "beta::") {
		t.Errorf("默认应路由到 beta, id=%s err=%v", id, err)
	}
}
