package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

type pinnedAgentRouteCaptureProvider struct {
	name    string
	content string
	err     error

	mu     sync.Mutex
	models []string
}

func (p *pinnedAgentRouteCaptureProvider) Name() string { return p.name }

func (p *pinnedAgentRouteCaptureProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.recordModel(req.Model)
	if p.err != nil {
		return nil, p.err
	}
	return &llm.CompletionResponse{
		Content: p.content,
		Usage:   llm.Usage{TotalTokens: 1},
	}, nil
}

func (p *pinnedAgentRouteCaptureProvider) Stream(_ context.Context, req llm.CompletionRequest) (*llm.Stream, error) {
	p.recordModel(req.Model)
	if p.err != nil {
		return nil, p.err
	}
	payload, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]string{"content": p.content}}},
	})
	return llm.NewStream(
		strings.NewReader("data: "+string(payload)+"\n\ndata: [DONE]\n\n"),
		llm.StreamOpenAIFormat,
	), nil
}

func (p *pinnedAgentRouteCaptureProvider) Models() []llm.ModelInfo { return nil }

func (p *pinnedAgentRouteCaptureProvider) CountTokens(messages []llm.Message) (int, error) {
	return len(messages), nil
}

func (p *pinnedAgentRouteCaptureProvider) recordModel(model string) {
	p.mu.Lock()
	p.models = append(p.models, model)
	p.mu.Unlock()
}

func (p *pinnedAgentRouteCaptureProvider) calledModels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.models...)
}

// 绑定命名 Agent 本身就是显式固定路由。即使 Desktop 只发送 pinned_agent，
// 绑定模型不可用时也不能改用同 Provider 默认模型或跨 Provider。
func TestPinnedAgentExactRouteUnavailableHasZeroFallbackCalls(t *testing.T) {
	bound := &pinnedAgentRouteCaptureProvider{
		name: "bound-provider",
		err:  errors.New("503 Service Unavailable"),
	}
	other := &pinnedAgentRouteCaptureProvider{
		name:    "other-provider",
		content: "fallback must not run",
	}
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{
			bound.name: bound,
			other.name: other,
		},
		map[string]config.LLMProviderConfig{
			bound.name: {
				Model:          "bound-default-model",
				Models:         []string{"bound-default-model", "bound-agent-model"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "bound-default-model", Capabilities: []string{config.LLMModelCapabilityText}},
					{ID: "bound-agent-model", Capabilities: []string{config.LLMModelCapabilityText}},
				},
			},
			other.name: {
				Model:          "other-default-model",
				Models:         []string{"other-default-model", "bound-agent-model"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "other-default-model", Capabilities: []string{config.LLMModelCapabilityText}},
					{ID: "bound-agent-model", Capabilities: []string{config.LLMModelCapabilityText}},
				},
			},
		},
		other.name,
	)

	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name:     "tutor",
		Provider: bound.name,
		Model:    "bound-agent-model",
	}); err != nil {
		t.Fatalf("注册固定 Agent 失败: %v", err)
	}
	eng.SetAgentRouter(dispatcher)

	msg := &adapter.Message{
		ID:       "pinned-agent-fixed-route",
		Platform: adapter.PlatformAPI,
		UserID:   "user-1",
		ChatID:   "chat-1",
		Content:  "请讲解这道数学题",
		Metadata: map[string]string{
			"pinned_agent": "tutor",
		},
	}
	chunks, terminalErr := eng.ProcessStream(context.Background(), msg)
	if terminalErr == nil {
		for chunk := range chunks {
			if chunk.Error != nil {
				terminalErr = chunk.Error
			}
		}
	}

	if terminalErr == nil {
		t.Error("固定 Agent 的精确路由不可用时必须返回原路由错误，不能由 fallback 静默成功")
	}
	if got, want := bound.calledModels(), []string{"bound-agent-model"}; !equalStrings(got, want) {
		t.Errorf("绑定 Provider 物理调用模型=%v，期望仅调用精确模型 %v（默认模型调用必须为 0）", got, want)
	}
	if got := other.calledModels(); len(got) != 0 {
		t.Errorf("跨 Provider 物理调用=%v，期望为 0", got)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
