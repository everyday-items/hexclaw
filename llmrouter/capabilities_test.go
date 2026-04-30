package llmrouter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
)

// fakeProvider 实现 hexagon.Provider，允许注入 Complete 响应。
type fakeProvider struct {
	name        string
	response    *llm.CompletionResponse
	completeErr error
	lastRequest llm.CompletionRequest
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	f.lastRequest = req
	if f.completeErr != nil {
		return nil, f.completeErr
	}
	return f.response, nil
}

// Stream / Models / CountTokens 不参与 probe 路径，空实现。
func (f *fakeProvider) Stream(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
	return nil, fmt.Errorf("fakeProvider.Stream not implemented")
}
func (f *fakeProvider) Models() []llm.ModelInfo                  { return nil }
func (f *fakeProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }

// memStore 实现 CapabilityStore 接口的内存版，用于测试。
type memStore struct {
	mu   sync.Mutex
	data map[string]Capability
}

func newMemStore() *memStore { return &memStore{data: map[string]Capability{}} }

func (m *memStore) key(provider, model string) string { return provider + "/" + model }

func (m *memStore) GetCapability(_ context.Context, provider, model string) (*Capability, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cap, ok := m.data[m.key(provider, model)]
	if !ok {
		return nil, nil
	}
	return &cap, nil
}

func (m *memStore) UpsertCapability(_ context.Context, cap Capability) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[m.key(cap.ProviderName, cap.ModelName)] = cap
	return nil
}

func (m *memStore) ListCapabilities(_ context.Context) ([]Capability, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Capability, 0, len(m.data))
	for _, c := range m.data {
		out = append(out, c)
	}
	return out, nil
}

// ---------- probeProvider 场景分类测试 ----------

func TestProbeProvider_Good(t *testing.T) {
	p := &fakeProvider{
		name: "mock",
		response: &llm.CompletionResponse{
			ToolCalls: []llm.ToolCall{
				{ID: "1", Name: "echo", Arguments: `{"text":"hello"}`},
			},
		},
	}
	cap := probeProvider(context.Background(), p, "mock", "mock-model")
	if cap.ToolCall != ReliabilityGood {
		t.Fatalf("期望 Good；got=%s err=%q", cap.ToolCall, cap.ProbeError)
	}
	if cap.ToolCallText != "good" {
		t.Errorf("ToolCallText 应为 good；got=%q", cap.ToolCallText)
	}
	if p.lastRequest.Model != "mock-model" {
		t.Fatalf("探测请求未设置目标模型，got=%q", p.lastRequest.Model)
	}
}

func TestProbeProvider_Partial_WrongToolName(t *testing.T) {
	p := &fakeProvider{
		response: &llm.CompletionResponse{
			ToolCalls: []llm.ToolCall{
				{Name: "speak", Arguments: `{"text":"hello"}`},
			},
		},
	}
	cap := probeProvider(context.Background(), p, "mock", "mock-model")
	if cap.ToolCall != ReliabilityPartial {
		t.Fatalf("期望 Partial；got=%s", cap.ToolCall)
	}
	if cap.ProbeError == "" {
		t.Error("ProbeError 应含错误说明")
	}
}

func TestProbeProvider_Partial_WrongArgValue(t *testing.T) {
	p := &fakeProvider{
		response: &llm.CompletionResponse{
			ToolCalls: []llm.ToolCall{
				{Name: "echo", Arguments: `{"text":"world"}`},
			},
		},
	}
	cap := probeProvider(context.Background(), p, "mock", "mock-model")
	if cap.ToolCall != ReliabilityPartial {
		t.Fatalf("期望 Partial；got=%s err=%q", cap.ToolCall, cap.ProbeError)
	}
}

func TestProbeProvider_Partial_InvalidJSON(t *testing.T) {
	p := &fakeProvider{
		response: &llm.CompletionResponse{
			ToolCalls: []llm.ToolCall{
				{Name: "echo", Arguments: `not a json`},
			},
		},
	}
	cap := probeProvider(context.Background(), p, "mock", "mock-model")
	if cap.ToolCall != ReliabilityPartial {
		t.Fatalf("期望 Partial（解析失败）；got=%s err=%q", cap.ToolCall, cap.ProbeError)
	}
}

func TestProbeProvider_Bad_NoToolCalls(t *testing.T) {
	p := &fakeProvider{
		response: &llm.CompletionResponse{
			Content: "I won't call tools, here's a text answer.",
		},
	}
	cap := probeProvider(context.Background(), p, "mock", "mock-model")
	if cap.ToolCall != ReliabilityBad {
		t.Fatalf("期望 Bad；got=%s", cap.ToolCall)
	}
	if cap.ProbeError == "" {
		t.Error("ProbeError 应说明返回纯文本")
	}
}

func TestProbeProvider_Bad_CompleteError(t *testing.T) {
	p := &fakeProvider{completeErr: errors.New("network unreachable")}
	cap := probeProvider(context.Background(), p, "mock", "mock-model")
	if cap.ToolCall != ReliabilityBad {
		t.Fatalf("期望 Bad；got=%s", cap.ToolCall)
	}
}

// ---------- parseToolArguments ----------

func TestParseToolArguments_AllShapes(t *testing.T) {
	// string JSON
	m, err := parseToolArguments(`{"k":"v"}`)
	if err != nil || m["k"] != "v" {
		t.Errorf("string JSON 解析失败: %v %v", m, err)
	}
	// []byte JSON
	m, err = parseToolArguments([]byte(`{"k":"v"}`))
	if err != nil || m["k"] != "v" {
		t.Errorf("[]byte JSON 解析失败: %v %v", m, err)
	}
	// map 原样返回
	m, err = parseToolArguments(map[string]any{"k": "v"})
	if err != nil || m["k"] != "v" {
		t.Errorf("map 透传失败: %v %v", m, err)
	}
	// 未知类型
	if _, err := parseToolArguments(42); err == nil {
		t.Error("未知类型应返回错误")
	}
}

// ---------- CapabilityService 行为 ----------

func TestCapabilityService_NilStore_NoPanic(t *testing.T) {
	// 校验 nil store 退化为"不持久化"，不 panic
	sel := &Selector{providers: map[string]hexagon.Provider{}}
	svc := NewCapabilityService(sel, nil)
	if _, err := svc.List(context.Background()); err != nil {
		t.Errorf("nil store 下 List 不应报错: %v", err)
	}
	if cap, err := svc.Get(context.Background(), "x", "y"); err != nil || cap != nil {
		t.Errorf("nil store 下 Get 应返回 (nil, nil)；got=%v %v", cap, err)
	}
}

func TestCapabilityService_ProbeAndCache(t *testing.T) {
	fp := &fakeProvider{
		name: "mock",
		response: &llm.CompletionResponse{
			ToolCalls: []llm.ToolCall{
				{Name: "echo", Arguments: `{"text":"hello"}`},
			},
		},
	}
	sel := &Selector{providers: map[string]hexagon.Provider{"mock": fp}}
	store := newMemStore()
	svc := NewCapabilityService(sel, store)

	cap, err := svc.Probe(context.Background(), "mock", "mock-model")
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if cap.ToolCall != ReliabilityGood {
		t.Fatalf("探测应为 Good；got=%s", cap.ToolCall)
	}

	// 缓存里能读到
	got, err := svc.Get(context.Background(), "mock", "mock-model")
	if err != nil || got == nil {
		t.Fatalf("缓存应存在；got=%v err=%v", got, err)
	}
	if got.ToolCall != ReliabilityGood {
		t.Errorf("缓存值不匹配；got=%s", got.ToolCall)
	}
}

func TestCapabilityService_Probe_UnknownProvider(t *testing.T) {
	sel := &Selector{providers: map[string]hexagon.Provider{}}
	svc := NewCapabilityService(sel, newMemStore())
	_, err := svc.Probe(context.Background(), "missing", "x")
	if err == nil {
		t.Fatal("未知 provider 应报错")
	}
}

func TestCapabilityService_Get_Expired(t *testing.T) {
	store := newMemStore()
	// 塞一条 60 天前的旧记录
	_ = store.UpsertCapability(context.Background(), Capability{
		ProviderName: "mock",
		ModelName:    "old-model",
		ToolCall:     ReliabilityGood,
		LastProbe:    time.Now().Add(-60 * 24 * time.Hour),
	})
	svc := NewCapabilityService(&Selector{providers: map[string]hexagon.Provider{}}, store)

	cap, err := svc.Get(context.Background(), "mock", "old-model")
	if err != nil {
		t.Fatalf("Get 不应报错: %v", err)
	}
	if cap != nil {
		t.Error("过期记录应返回 nil（30 天 TTL）")
	}
}

// ---------- ReliabilityLevel.String ----------

func TestReliabilityLevel_String(t *testing.T) {
	cases := map[ReliabilityLevel]string{
		ReliabilityUnknown: "unknown",
		ReliabilityGood:    "good",
		ReliabilityPartial: "partial",
		ReliabilityBad:     "bad",
	}
	for lvl, want := range cases {
		if got := lvl.String(); got != want {
			t.Errorf("%d: got=%s want=%s", lvl, got, want)
		}
	}
}
